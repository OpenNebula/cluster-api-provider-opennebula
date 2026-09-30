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
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
)

// helmChartGVR is the Rancher Helm controller resource watched for OneKS
// application lifecycle changes.
var helmChartGVR = schema.GroupVersionResource{
	Group: "helm.cattle.io", Version: "v1", Resource: "helmcharts",
}

// applicationOperationGVR selects ConfigMaps used to report failures that
// happen before a HelmChart exists.
var applicationOperationGVR = schema.GroupVersionResource{
	Group: "", Version: "v1", Resource: "configmaps",
}

const (
	// helmChartNamespace is where OneKS application HelmCharts and failed
	// operation markers are created and observed.
	helmChartNamespace = "kube-system"

	// These labels and annotations form a cross-system protocol with OneKS and
	// the component that creates HelmChart/operation objects. Renaming one or
	// changing its accepted values requires a coordinated producer and OneKS
	// server change.
	// managedLabel selects objects whose lifecycle is owned by OneKS.
	managedLabel = "oneks.opennebula.io/managed"
	// releaseAnnotation identifies the OneKS application release and is required
	// on every managed HelmChart and failed operation marker.
	releaseAnnotation = "oneks.opennebula.io/release-name"
	// parentAnnotation optionally associates a release with its parent release.
	parentAnnotation = "oneks.opennebula.io/parent"
	// stateAnnotation carries installing, ready, deleting or failed.
	stateAnnotation = "oneks.opennebula.io/state"
	// errorAnnotation contains the human-readable reason for a failed operation.
	errorAnnotation = "oneks.opennebula.io/error"
	// operationLabel distinguishes pre-Helm application operation markers from
	// ordinary ConfigMaps selected by managedLabel.
	operationLabel = "oneks.opennebula.io/operation"
	// applicationOperation is the operationLabel value accepted by the monitor.
	applicationOperation = "application"
)

// ChartEvent is an application lifecycle callback consumed by OneKS. Event is
// app_state_changed for normal transitions and app_failed for failures.
type ChartEvent struct {
	Event   string            `json:"event"`
	Payload ChartEventPayload `json:"payload"`
}

// ChartEventPayload identifies the release and the Kubernetes revision that
// produced an application lifecycle callback. State is installing, ready,
// deleting or done. ErrorMessage is populated only for app_failed.
type ChartEventPayload struct {
	ReleaseName     string      `json:"release_name"`
	ResourceVersion json.Number `json:"resource_version"`
	Parent          string      `json:"parent,omitempty"`
	State           string      `json:"state,omitempty"`
	ErrorMessage    string      `json:"error_msg,omitempty"`
}

type pendingChart struct {
	chart   *unstructured.Unstructured
	deleted bool
}

// ChartMonitor converts OneKS-managed HelmCharts and failed operation markers
// into application lifecycle events. Managed objects must be in kube-system
// and carry oneks.opennebula.io/managed=true.
//
// Its rate-limited queue stores the latest object per namespace/name. Updates
// can therefore be coalesced while OneKS is unavailable: delivery converges on
// the latest lifecycle state but is not an audit log of every transition.
type ChartMonitor struct {
	factory    dynamicinformer.DynamicSharedInformerFactory
	charts     cache.SharedIndexInformer
	operations cache.SharedIndexInformer
	publisher  Publisher
	queue      workqueue.TypedRateLimitingInterface[string]

	mu      sync.Mutex
	pending map[string]pendingChart
	ready   atomic.Bool
}

// NewChartMonitor constructs filtered HelmChart and ConfigMap informers. Run
// must be called to synchronize them and start the delivery worker.
func NewChartMonitor(dynamicClient dynamic.Interface, publisher Publisher) (*ChartMonitor, error) {
	m := &ChartMonitor{
		publisher: publisher,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		pending: make(map[string]pendingChart),
	}
	m.factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient, 0, helmChartNamespace, func(options *metav1.ListOptions) {
			options.LabelSelector = managedLabel + "=true"
		},
	)
	m.charts = m.factory.ForResource(helmChartGVR).Informer()
	m.operations = m.factory.ForResource(applicationOperationGVR).Informer()
	if _, err := m.charts.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { m.enqueue(obj, false) },
		UpdateFunc: func(oldObj, newObj any) {
			// Kubernetes may update unrelated fields frequently. Only changes
			// that affect the OneKS event payload need to enter the queue.
			if chartEventSignature(oldObj) != chartEventSignature(newObj) {
				m.enqueue(newObj, false)
			}
		},
		DeleteFunc: func(obj any) { m.enqueue(obj, true) },
	}); err != nil {
		return nil, fmt.Errorf("register HelmChart handler: %w", err)
	}
	if _, err := m.operations.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { m.enqueueOperation(obj) },
		UpdateFunc: func(oldObj, newObj any) {
			if chartEventSignature(oldObj) != chartEventSignature(newObj) {
				m.enqueueOperation(newObj)
			}
		},
	}); err != nil {
		return nil, fmt.Errorf("register application operation handler: %w", err)
	}
	return m, nil
}

// Run synchronizes both application informers and retries OneKS delivery with
// rate limiting until ctx is cancelled.
func (m *ChartMonitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	m.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.charts.HasSynced, m.operations.HasSynced) {
		return fmt.Errorf("initial application informer cache sync failed")
	}
	m.ready.Store(true)
	defer m.ready.Store(false)
	ctrl.Log.WithName("chart-monitor").Info("Application monitor caches synchronized")
	go func() {
		<-ctx.Done()
		m.queue.ShutDown()
	}()
	for m.processNext(ctx) {
	}
	return nil
}

// Ready reports whether both application informer caches have synchronized. It
// does not imply that OneKS is currently reachable.
func (m *ChartMonitor) Ready() bool { return m.ready.Load() }

func (m *ChartMonitor) enqueue(obj any, deleted bool) {
	chart, ok := chartFromEvent(obj)
	if !ok {
		return
	}
	key := chart.GetNamespace() + "/" + chart.GetName()
	// Coalesce repeated updates for the same chart while retaining the newest
	// object for the worker and HTTP retries
	m.mu.Lock()
	m.pending[key] = pendingChart{chart: chart.DeepCopy(), deleted: deleted}
	m.mu.Unlock()
	m.queue.Add(key)
}

func (m *ChartMonitor) enqueueOperation(obj any) {
	operation, ok := applicationOperationFromEvent(obj)
	if !ok {
		return
	}
	m.enqueue(operation, false)
}

func (m *ChartMonitor) processNext(ctx context.Context) bool {
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

	event, err := chartEvent(pending.chart, pending.deleted)
	if err != nil {
		ctrl.LoggerFrom(ctx).WithName("chart-monitor").Error(
			err, "invalid managed HelmChart", "chart", key,
		)
		m.forgetPending(key, pending)
		m.queue.Forget(key)
		return true
	}
	if err := m.publisher.PublishChartEvent(ctx, event); err != nil {
		ctrl.LoggerFrom(ctx).WithName("chart-monitor").Error(
			err, "unable to send chart event", "chart", key, "event", event.Event,
		)
		m.queue.AddRateLimited(key)
		return true
	}

	m.forgetPending(key, pending)
	m.queue.Forget(key)
	return true
}

func (m *ChartMonitor) forgetPending(key string, processed pendingChart) {
	// Do not remove a newer update that arrived while the processed event was
	// being published
	m.mu.Lock()
	defer m.mu.Unlock()
	current, found := m.pending[key]
	if found && current.deleted == processed.deleted &&
		current.chart.GetResourceVersion() == processed.chart.GetResourceVersion() {
		delete(m.pending, key)
	}
}

func chartEvent(chart *unstructured.Unstructured, deleted bool) (ChartEvent, error) {
	annotations := chart.GetAnnotations()
	releaseName := strings.TrimSpace(annotations[releaseAnnotation])
	if releaseName == "" {
		return ChartEvent{}, fmt.Errorf("annotation %s is required", releaseAnnotation)
	}
	resourceVersion := chart.GetResourceVersion()
	if !decimalNumber(resourceVersion) {
		return ChartEvent{}, fmt.Errorf("resourceVersion must be a non-negative integer: %q", resourceVersion)
	}
	payload := ChartEventPayload{
		ReleaseName:     releaseName,
		ResourceVersion: json.Number(resourceVersion),
		Parent:          strings.TrimSpace(annotations[parentAnnotation]),
	}
	state := annotations[stateAnnotation]
	deleting := chart.GetDeletionTimestamp() != nil || state == "deleting"
	if state == "failed" && !deleted && !deleting {
		message := strings.TrimSpace(annotations[errorAnnotation])
		if message == "" {
			message = "Application installation failed"
		}
		payload.ErrorMessage = message
		return ChartEvent{Event: "app_failed", Payload: payload}, nil
	}
	payload.State = "installing"
	// OneKS interprets deletion completion as done. A deletion timestamp or
	// explicit deleting state reports the in-progress transition instead.
	switch {
	case deleted:
		payload.State = "done"
	case deleting:
		payload.State = "deleting"
	case state == "ready":
		payload.State = "ready"
	case state == "", state == "installing":
	default:
		return ChartEvent{}, fmt.Errorf("annotation %s has unsupported state %q", stateAnnotation, state)
	}
	return ChartEvent{Event: "app_state_changed", Payload: payload}, nil
}

func decimalNumber(value string) bool {
	if value == "" {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func chartEventSignature(obj any) string {
	chart, ok := chartFromEvent(obj)
	if !ok {
		return ""
	}
	annotations := chart.GetAnnotations()
	return fmt.Sprintf("%t\x00%s\x00%s\x00%s\x00%s", chart.GetDeletionTimestamp() != nil,
		annotations[releaseAnnotation], annotations[parentAnnotation],
		annotations[stateAnnotation], annotations[errorAnnotation])
}

func chartFromEvent(obj any) (*unstructured.Unstructured, bool) {
	if chart, ok := obj.(*unstructured.Unstructured); ok {
		return chart, true
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, false
	}
	chart, ok := tombstone.Obj.(*unstructured.Unstructured)
	return chart, ok
}

func applicationOperationFromEvent(obj any) (*unstructured.Unstructured, bool) {
	operation, ok := obj.(*unstructured.Unstructured)
	if !ok || operation.GetLabels()[operationLabel] != applicationOperation ||
		operation.GetAnnotations()[stateAnnotation] != "failed" {
		return nil, false
	}
	return operation, true
}
