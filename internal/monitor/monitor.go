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
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/monitoring"
)

const MaxPendingCallbackIdentities = 8192

const applicationSelector = "app.kubernetes.io/managed-by=oneks,applications.oneks.opennebula.io/producer=oneks-server"

var (
	applicationGVR = schema.GroupVersionResource{Group: "oneks.opennebula.io", Version: "v1alpha1", Resource: "oneksapplications"}
)

type pendingReport struct {
	report   CallbackPayload
	revision uint64
}

type Monitor struct {
	config  Config
	sender  Sender
	metrics *Metrics

	nodeFactory        informers.SharedInformerFactory
	applicationFactory dynamicinformer.DynamicSharedInformerFactory
	profileFactory     informers.SharedInformerFactory
	nodes              cache.SharedIndexInformer
	applications       cache.SharedIndexInformer
	profileConfigs     cache.SharedIndexInformer
	profiles           *monitoring.Store
	evaluator          *monitoring.Evaluator
	queue              workqueue.TypedRateLimitingInterface[string]

	mu       sync.Mutex
	pending  map[string]pendingReport
	revision uint64
	ready    atomic.Bool
}

func New(config Config, client kubernetes.Interface, dynamicClient dynamic.Interface, sender Sender, metrics *Metrics) (*Monitor, error) {
	m := &Monitor{
		config: config, sender: sender, metrics: metrics,
		queue:   workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		pending: map[string]pendingReport{},
	}
	// A zero resync period keeps reconciliation strictly event-driven. Watch
	// reconnects still relist objects, and initial cache sync emits snapshots.
	m.nodeFactory = informers.NewSharedInformerFactory(client, 0)
	m.applicationFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient, 0, config.ApplicationNamespace,
		func(options *metav1.ListOptions) { options.LabelSelector = applicationSelector },
	)
	m.profileFactory = informers.NewSharedInformerFactoryWithOptions(
		client, 0,
		informers.WithNamespace(config.ProfileNamespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = monitoring.ProfileLabel + "=true"
		}),
	)
	m.nodes = m.nodeFactory.Core().V1().Nodes().Informer()
	m.applications = m.applicationFactory.ForResource(applicationGVR).Informer()
	m.profileConfigs = m.profileFactory.Core().V1().ConfigMaps().Informer()
	m.profiles = monitoring.NewStore(config.PrometheusNamespaces)
	m.evaluator = monitoring.NewEvaluator(
		m.profiles,
		monitoring.NewPrometheusClient(client, nil),
		config.ClusterID,
		func(signal monitoring.ClusterSignal) {
			m.enqueue("ClusterSignal/"+signal.Identity, signal)
		},
		monitoring.EvaluationHooks{
			Failure: func(profile, rule string, err error) {
				m.metrics.RuleEvaluationFailed()
				klog.ErrorS(err, "monitoring rule evaluation failed", "profile", profile, "rule", rule)
			},
			ActiveSignals: m.metrics.SetActiveSignals,
		},
	)

	if _, err := m.nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.onNode(obj, "Added") },
		UpdateFunc: func(_, obj any) { m.onNode(obj, "Updated") },
		DeleteFunc: func(obj any) { m.onNodeDeleted(obj) },
	}); err != nil {
		return nil, fmt.Errorf("register node handler: %w", err)
	}
	if _, err := m.applications.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.onApplication(obj, "Added") },
		UpdateFunc: func(_, obj any) { m.onApplication(obj, "Updated") },
		DeleteFunc: m.onApplicationDeleted,
	}); err != nil {
		return nil, fmt.Errorf("register OneKSApplication handler: %w", err)
	}
	if _, err := m.profileConfigs.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    m.onMonitoringProfile,
		UpdateFunc: func(_, obj any) { m.onMonitoringProfile(obj) },
		DeleteFunc: m.onMonitoringProfileDeleted,
	}); err != nil {
		return nil, fmt.Errorf("register MonitoringProfile handler: %w", err)
	}
	return m, nil
}

func (m *Monitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	defer m.queue.ShutDown()
	m.nodeFactory.Start(ctx.Done())
	m.applicationFactory.Start(ctx.Done())
	m.profileFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(
		ctx.Done(), m.nodes.HasSynced, m.applications.HasSynced, m.profileConfigs.HasSynced,
	) {
		return fmt.Errorf("initial informer cache sync failed")
	}
	m.enqueueNodeSnapshot()
	m.updateNodeMetrics()
	go m.evaluator.Run(ctx)
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
	m.metrics.callbackAttempt()
	if err := m.sender.Send(ctx, pending.report); err != nil {
		klog.ErrorS(err, "unable to report resource status", "key", key)
		m.metrics.callbackFailed()
		m.queue.AddRateLimited(key)
		return true
	}
	m.metrics.callbackSucceeded(time.Now())
	m.queue.Forget(key)
	m.mu.Lock()
	if current, ok := m.pending[key]; ok && current.revision == pending.revision {
		delete(m.pending, key)
	} else {
		m.queue.Add(key)
	}
	m.metrics.setQueueDepth(len(m.pending))
	m.mu.Unlock()
	return true
}

func (m *Monitor) enqueue(key string, report CallbackPayload) bool {
	m.mu.Lock()
	if _, exists := m.pending[key]; !exists && len(m.pending) >= MaxPendingCallbackIdentities {
		m.mu.Unlock()
		m.metrics.callbackRejected()
		klog.ErrorS(
			fmt.Errorf("callback identity limit %d reached", MaxPendingCallbackIdentities),
			"callback was not queued", "key", key,
		)
		return false
	}
	m.revision++
	m.pending[key] = pendingReport{report: report, revision: m.revision}
	m.metrics.setQueueDepth(len(m.pending))
	m.mu.Unlock()
	m.queue.Add(key)
	return true
}

func (m *Monitor) onNode(obj any, event string) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	report := nodeReport(m.config, node, event)
	report.Status["readyProviderIDs"] = m.readyProviderIDs("")
	m.enqueue("Node/"+node.Name, report)
	m.updateNodeMetrics()
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
	m.updateNodeMetrics()
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

func (m *Monitor) updateNodeMetrics() {
	nodes := make([]*corev1.Node, 0, len(m.nodes.GetStore().List()))
	for _, item := range m.nodes.GetStore().List() {
		if node, ok := item.(*corev1.Node); ok {
			nodes = append(nodes, node)
		}
	}
	m.metrics.setNodes(nodes)
}

func (m *Monitor) onMonitoringProfile(obj any) {
	configMap, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	key := configMap.Namespace + "/" + configMap.Name
	if configMap.Labels[monitoring.ProfileLabel] != "true" {
		m.profiles.Delete(key)
		return
	}
	document, exists := configMap.Data[monitoring.ProfileDataKey]
	if !exists {
		m.metrics.ProfileParseFailed()
		klog.ErrorS(fmt.Errorf("profile.yaml is required"), "invalid MonitoringProfile ConfigMap", "namespace", configMap.Namespace, "name", configMap.Name)
		return
	}
	if err := m.profiles.Upsert(key, []byte(document)); err != nil {
		m.metrics.ProfileParseFailed()
		klog.ErrorS(err, "invalid MonitoringProfile ConfigMap", "namespace", configMap.Namespace, "name", configMap.Name)
	}
}

func (m *Monitor) onMonitoringProfileDeleted(obj any) {
	configMap, ok := deletedObject[*corev1.ConfigMap](obj)
	if !ok {
		return
	}
	m.profiles.Delete(configMap.Namespace + "/" + configMap.Name)
}

func (m *Monitor) onApplication(obj any, event string) {
	app, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	m.enqueue("OneKSApplication/"+app.GetNamespace()+"/"+app.GetName(), applicationReport(app, event))
}

func (m *Monitor) onApplicationDeleted(obj any) {
	app, ok := deletedObject[*unstructured.Unstructured](obj)
	if !ok {
		return
	}
	m.enqueue("OneKSApplication/"+app.GetNamespace()+"/"+app.GetName(), applicationReport(app, "Deleted"))
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
