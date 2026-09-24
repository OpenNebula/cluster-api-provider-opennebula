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

var helmChartGVR = schema.GroupVersionResource{
	Group: "helm.cattle.io", Version: "v1", Resource: "helmcharts",
}

const (
	helmChartNamespace = "kube-system"
	managedLabel       = "oneks.opennebula.io/managed"
	releaseAnnotation  = "oneks.opennebula.io/release-name"
	parentAnnotation   = "oneks.opennebula.io/parent"
	stateAnnotation    = "oneks.opennebula.io/state"
	errorAnnotation    = "oneks.opennebula.io/error"
)

type ChartEvent struct {
	Event   string         `json:"event"`
	Payload map[string]any `json:"payload"`
}

type pendingChart struct {
	chart   *unstructured.Unstructured
	deleted bool
}

type ChartMonitor struct {
	factory     dynamicinformer.DynamicSharedInformerFactory
	charts      cache.SharedIndexInformer
	sender      Sender
	destination ClusterEventDestination
	queue       workqueue.TypedRateLimitingInterface[string]

	mu      sync.Mutex
	pending map[string]pendingChart
	ready   atomic.Bool
}

func NewChartMonitor(dynamicClient dynamic.Interface, sender Sender, config Config) (*ChartMonitor, error) {
	m := &ChartMonitor{
		sender: sender, destination: ClusterEventDestination{ClusterID: config.ClusterID},
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
	if _, err := m.charts.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { m.enqueue(obj, false) },
		UpdateFunc: func(oldObj, newObj any) {
			if chartEventSignature(oldObj) != chartEventSignature(newObj) {
				m.enqueue(newObj, false)
			}
		},
		DeleteFunc: func(obj any) { m.enqueue(obj, true) },
	}); err != nil {
		return nil, fmt.Errorf("register HelmChart handler: %w", err)
	}
	return m, nil
}

func (m *ChartMonitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	m.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.charts.HasSynced) {
		return fmt.Errorf("initial HelmChart informer cache sync failed")
	}
	m.ready.Store(true)
	defer m.ready.Store(false)
	ctrl.Log.WithName("chart-monitor").Info("HelmChart monitor cache synchronized")
	go func() {
		<-ctx.Done()
		m.queue.ShutDown()
	}()
	for m.processNext(ctx) {
	}
	return nil
}

func (m *ChartMonitor) Ready() bool { return m.ready.Load() }

func (m *ChartMonitor) enqueue(obj any, deleted bool) {
	chart, ok := chartFromEvent(obj)
	if !ok {
		return
	}
	key := chart.GetNamespace() + "/" + chart.GetName()
	m.mu.Lock()
	m.pending[key] = pendingChart{chart: chart.DeepCopy(), deleted: deleted}
	m.mu.Unlock()
	m.queue.Add(key)
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

	event := chartEvent(pending.chart, pending.deleted)
	if err := m.sender.Send(ctx, m.destination, event); err != nil {
		ctrl.LoggerFrom(ctx).WithName("chart-monitor").Error(
			err, "unable to send chart event", "chart", key, "event", event.Event,
		)
		m.queue.AddRateLimited(key)
		return true
	}

	m.mu.Lock()
	current, found := m.pending[key]
	if found && current.deleted == pending.deleted &&
		current.chart.GetResourceVersion() == pending.chart.GetResourceVersion() {
		delete(m.pending, key)
	}
	m.mu.Unlock()
	m.queue.Forget(key)
	return true
}

func chartEvent(chart *unstructured.Unstructured, deleted bool) ChartEvent {
	annotations := chart.GetAnnotations()
	payload := map[string]any{
		"release_name":     annotations[releaseAnnotation],
		"resource_version": json.Number(chart.GetResourceVersion()),
	}
	if parent := annotations[parentAnnotation]; parent != "" {
		payload["parent"] = parent
	}
	state := annotations[stateAnnotation]
	deleting := chart.GetDeletionTimestamp() != nil || state == "deleting"
	if state == "failed" && !deleted && !deleting {
		message := annotations[errorAnnotation]
		if message == "" {
			message = "Application installation failed"
		}
		payload["error_msg"] = message
		return ChartEvent{Event: "app_failed", Payload: payload}
	}
	eventState := "installing"
	switch {
	case deleted:
		eventState = "done"
	case deleting:
		eventState = "deleting"
	case state == "ready":
		eventState = "ready"
	}
	payload["state"] = eventState
	return ChartEvent{Event: "app_state_changed", Payload: payload}
}

func chartEventSignature(obj any) string {
	chart, ok := chartFromEvent(obj)
	if !ok {
		return ""
	}
	annotations := chart.GetAnnotations()
	return fmt.Sprintf("%t\x00%s\x00%s", chart.GetDeletionTimestamp() != nil,
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
