/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	chartOperationMarker       = "oneks-chart-reconcile"
	readinessJobLabel          = "oneks.opennebula.io/readiness-job"
	readinessReleaseAnnotation = "oneks.opennebula.io/release-name"
)

var (
	helmChartGVR = schema.GroupVersionResource{Group: "helm.cattle.io", Version: "v1", Resource: "helmcharts"}
)

type pendingReport struct {
	report   Report
	revision uint64
}

type Monitor struct {
	config Config
	sender Sender

	nodeFactory  informers.SharedInformerFactory
	chartFactory dynamicinformer.DynamicSharedInformerFactory
	jobFactory   informers.SharedInformerFactory
	nodes        cache.SharedIndexInformer
	charts       cache.SharedIndexInformer
	jobs         cache.SharedIndexInformer
	operations   cache.SharedIndexInformer
	queue        workqueue.TypedRateLimitingInterface[string]

	mu       sync.Mutex
	pending  map[string]pendingReport
	revision uint64
	ready    atomic.Bool
}

func New(config Config, client kubernetes.Interface, dynamicClient dynamic.Interface, sender Sender) (*Monitor, error) {
	m := &Monitor{
		config: config, sender: sender,
		queue:   workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		pending: map[string]pendingReport{},
	}
	// A zero resync period keeps reconciliation strictly event-driven. Watch
	// reconnects still relist objects, and initial cache sync emits snapshots.
	m.nodeFactory = informers.NewSharedInformerFactory(client, 0)
	m.chartFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, 0, config.KubeSystemNS, nil)
	m.jobFactory = informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(config.KubeSystemNS))
	m.nodes = m.nodeFactory.Core().V1().Nodes().Informer()
	m.charts = m.chartFactory.ForResource(helmChartGVR).Informer()
	m.jobs = m.jobFactory.Batch().V1().Jobs().Informer()
	m.operations = m.jobFactory.Core().V1().ConfigMaps().Informer()

	if _, err := m.nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.onNode(obj, "Added") },
		UpdateFunc: func(_, obj any) { m.onNode(obj, "Updated") },
		DeleteFunc: func(obj any) { m.onNodeDeleted(obj) },
	}); err != nil {
		return nil, fmt.Errorf("register node handler: %w", err)
	}
	if _, err := m.charts.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.onChart(obj, "Added") },
		UpdateFunc: func(_, obj any) { m.onChart(obj, "Updated") },
		DeleteFunc: func(obj any) { m.onChartDeleted(obj) },
	}); err != nil {
		return nil, fmt.Errorf("register HelmChart handler: %w", err)
	}
	jobHandler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.onJob(obj, "Added") },
		UpdateFunc: func(_, obj any) { m.onJob(obj, "Updated") },
		DeleteFunc: func(obj any) { m.onJob(obj, "Deleted") },
	}
	if _, err := m.jobs.AddEventHandler(jobHandler); err != nil {
		return nil, fmt.Errorf("register Helm job handler: %w", err)
	}
	operationHandler := cache.ResourceEventHandlerFuncs{
		AddFunc:    m.onChartOperation,
		UpdateFunc: func(_, obj any) { m.onChartOperation(obj) },
		DeleteFunc: m.onChartOperation,
	}
	if _, err := m.operations.AddEventHandler(operationHandler); err != nil {
		return nil, fmt.Errorf("register chart operation handler: %w", err)
	}
	return m, nil
}

func (m *Monitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	defer m.queue.ShutDown()
	m.nodeFactory.Start(ctx.Done())
	m.chartFactory.Start(ctx.Done())
	m.jobFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(
		ctx.Done(), m.nodes.HasSynced, m.charts.HasSynced,
		m.jobs.HasSynced, m.operations.HasSynced,
	) {
		return fmt.Errorf("initial informer cache sync failed")
	}
	m.enqueueNodeSnapshot()
	m.enqueueReconcile(m.hasActiveChartOperation())
	m.ready.Store(true)
	klog.InfoS("monitor caches synchronized")

	go func() {
		<-ctx.Done()
		m.ready.Store(false)
		m.queue.ShutDown()
	}()
	for m.processNext(ctx) {
	}
	return nil
}

func (m *Monitor) Ready() bool { return m.ready.Load() }

func (m *Monitor) processNext(ctx context.Context) bool {
	key, shutdown := m.queue.Get()
	if shutdown {
		return false
	}
	defer m.queue.Done(key)
	m.mu.Lock()
	pending, exists := m.pending[key]
	m.mu.Unlock()
	if !exists {
		m.queue.Forget(key)
		return true
	}
	if err := m.sender.Send(ctx, pending.report); err != nil {
		klog.ErrorS(err, "unable to report resource status", "key", key)
		m.queue.AddRateLimited(key)
		return true
	}
	m.queue.Forget(key)
	m.mu.Lock()
	if current, ok := m.pending[key]; ok && current.revision == pending.revision {
		delete(m.pending, key)
	} else {
		m.queue.Add(key)
	}
	m.mu.Unlock()
	return true
}

func (m *Monitor) enqueue(key string, report Report) {
	m.mu.Lock()
	m.revision++
	m.pending[key] = pendingReport{report: report, revision: m.revision}
	m.mu.Unlock()
	m.queue.Add(key)
}

func (m *Monitor) onNode(obj any, event string) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	report := nodeReport(m.config, node, event)
	report.Status["readyProviderIDs"] = m.readyProviderIDs("")
	m.enqueue("Node/"+node.Name, report)
}

func (m *Monitor) onNodeDeleted(obj any) {
	node, ok := deletedObject[*corev1.Node](obj)
	if !ok {
		return
	}
	report := nodeReport(m.config, node, "Deleted")
	report.Status["deleted"] = true
	report.Status["readyProviderIDs"] = m.readyProviderIDs(node.Spec.ProviderID)
	m.enqueue("Node/"+node.Name, report)
}

func (m *Monitor) readyProviderIDs(exclude string) []string {
	providerIDs := make([]string, 0)
	for _, item := range m.nodes.GetStore().List() {
		node, ok := item.(*corev1.Node)
		if !ok || node.Spec.ProviderID == "" || node.Spec.ProviderID == exclude || !nodeReady(node) {
			continue
		}
		providerIDs = append(providerIDs, node.Spec.ProviderID)
	}
	sort.Strings(providerIDs)
	return providerIDs
}

func (m *Monitor) enqueueNodeSnapshot() {
	readyProviderIDs := m.readyProviderIDs("")
	for _, item := range m.nodes.GetStore().List() {
		node, ok := item.(*corev1.Node)
		if !ok {
			continue
		}
		report := nodeReport(m.config, node, "Updated")
		report.Status["readyProviderIDs"] = readyProviderIDs
		m.enqueue("Node/"+node.Name, report)
	}
}

func (m *Monitor) enqueueReconcile(active bool) {
	m.enqueue("Reconcile/charts", reconcileReport(active))
}

func (m *Monitor) hasActiveChartOperation() bool {
	_, exists, err := m.operations.GetStore().GetByKey(
		m.config.KubeSystemNS + "/" + chartOperationMarker,
	)
	return err == nil && exists
}

func (m *Monitor) onChartOperation(obj any) {
	configMap, ok := deletedObject[*corev1.ConfigMap](obj)
	if !ok || configMap.Name != chartOperationMarker {
		return
	}

	m.enqueueReconcile(m.hasActiveChartOperation())
}

func (m *Monitor) onChart(obj any, event string) {
	chart, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	status, jobRV := m.chartStatus(chart)
	report, watched := chartReport(m.config, chart, status, event)
	if !watched {
		return
	}
	report.RelatedResourceVersion = jobRV
	m.enqueue("HelmChart/"+chart.GetNamespace()+"/"+chart.GetName(), report)
}

func (m *Monitor) onChartDeleted(obj any) {
	chart, ok := deletedObject[*unstructured.Unstructured](obj)
	if !ok {
		return
	}
	report, watched := chartReport(m.config, chart, "unknown", "Deleted")
	if !watched {
		return
	}
	report.Status = map[string]any{"chartId": report.Status["chartId"], "deleted": true}
	m.enqueue("HelmChart/"+chart.GetNamespace()+"/"+chart.GetName(), report)
}

func (m *Monitor) onJob(obj any, event string) {
	job, ok := deletedObject[*batchv1.Job](obj)
	if !ok {
		return
	}
	if report, watched := readinessJobReport(m.config, job, event); watched {
		m.enqueue("ReadinessJob/"+job.Namespace+"/"+job.Name, report)
	}
	for _, item := range m.charts.GetStore().List() {
		chart, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		jobName, _, _ := unstructured.NestedString(chart.Object, "status", "jobName")
		if chart.GetNamespace() != job.Namespace || jobName != job.Name {
			continue
		}
		m.onChart(chart, "Updated")
	}
}

func (m *Monitor) chartStatus(chart *unstructured.Unstructured) (string, string) {
	if chart.GetDeletionTimestamp() != nil {
		return "uninstalling", ""
	}
	if chartConditionTrue(chart, "Failed") {
		return "failed", ""
	}
	jobName, found, _ := unstructured.NestedString(chart.Object, "status", "jobName")
	if !found || strings.TrimSpace(jobName) == "" {
		return "pending", ""
	}
	item, exists, err := m.jobs.GetStore().GetByKey(chart.GetNamespace() + "/" + strings.TrimSpace(jobName))
	if err != nil || !exists {
		return "unknown", ""
	}
	job, ok := item.(*batchv1.Job)
	if !ok {
		return "unknown", ""
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobFailed:
			return "failed", job.ResourceVersion
		case batchv1.JobComplete:
			return "deployed", job.ResourceVersion
		}
	}
	return "pending", job.ResourceVersion
}

func chartConditionTrue(chart *unstructured.Unstructured, conditionType string) bool {
	conditions, found, _ := unstructured.NestedSlice(chart.Object, "status", "conditions")
	if !found {
		return false
	}
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if ok && condition["type"] == conditionType && condition["status"] == string(corev1.ConditionTrue) {
			return true
		}
	}
	return false
}

func deletedObject[T any](obj any) (T, bool) {
	if value, ok := obj.(T); ok {
		return value, true
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		var zero T
		return zero, false
	}
	value, ok := tombstone.Obj.(T)
	return value, ok
}
