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
	"sync"
	"sync/atomic"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
)

var applicationGVR = schema.GroupVersionResource{
	Group: "oneks.opennebula.io", Version: "v1beta1", Resource: "oneksapplications",
}

type ChartEvent struct {
	Event   string         `json:"event"`
	Payload map[string]any `json:"payload"`
}

type pendingApplication struct {
	application *unstructured.Unstructured
	deleted     bool
}

type ApplicationMonitor struct {
	factory      dynamicinformer.DynamicSharedInformerFactory
	applications cache.SharedIndexInformer
	sender       Sender
	destination  ClusterEventDestination
	queue        workqueue.TypedRateLimitingInterface[string]

	mu      sync.Mutex
	pending map[string]pendingApplication
	ready   atomic.Bool
}

func NewApplicationMonitor(dynamicClient dynamic.Interface, sender Sender, config Config) (*ApplicationMonitor, error) {
	m := &ApplicationMonitor{
		sender: sender, destination: ClusterEventDestination{ClusterID: config.ClusterID},
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		pending: make(map[string]pendingApplication),
	}
	m.factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient, 0, applicationv1.ApplicationNamespace, nil,
	)
	m.applications = m.factory.ForResource(applicationGVR).Informer()
	if _, err := m.applications.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { m.enqueue(obj, false) },
		UpdateFunc: func(oldObj, newObj any) {
			if applicationEventSignature(oldObj) != applicationEventSignature(newObj) {
				m.enqueue(newObj, false)
			}
		},
		DeleteFunc: func(obj any) { m.enqueue(obj, true) },
	}); err != nil {
		return nil, fmt.Errorf("register OneKSApplication handler: %w", err)
	}
	return m, nil
}

func (m *ApplicationMonitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	m.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.applications.HasSynced) {
		return fmt.Errorf("initial OneKSApplication informer cache sync failed")
	}
	m.ready.Store(true)
	defer m.ready.Store(false)
	ctrl.Log.WithName("application-monitor").Info("application monitor cache synchronized")
	go func() {
		<-ctx.Done()
		m.queue.ShutDown()
	}()
	for m.processNext(ctx) {
	}
	return nil
}

func (m *ApplicationMonitor) Ready() bool { return m.ready.Load() }

func (m *ApplicationMonitor) enqueue(obj any, deleted bool) {
	app, ok := applicationFromEvent(obj)
	if !ok {
		return
	}
	key := app.GetNamespace() + "/" + app.GetName()
	m.mu.Lock()
	m.pending[key] = pendingApplication{application: app.DeepCopy(), deleted: deleted}
	m.mu.Unlock()
	m.queue.Add(key)
}

func (m *ApplicationMonitor) processNext(ctx context.Context) bool {
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

	event := chartEvent(pending.application, pending.deleted)
	if err := m.sender.Send(ctx, m.destination, event); err != nil {
		ctrl.LoggerFrom(ctx).WithName("application-monitor").Error(
			err, "unable to send chart event", "application", key, "event", event.Event,
		)
		m.queue.AddRateLimited(key)
		return true
	}

	m.mu.Lock()
	current, found := m.pending[key]
	if found && current.deleted == pending.deleted &&
		current.application.GetResourceVersion() == pending.application.GetResourceVersion() {
		delete(m.pending, key)
	}
	m.mu.Unlock()
	m.queue.Forget(key)
	return true
}

func chartEvent(app *unstructured.Unstructured, deleted bool) ChartEvent {
	payload := map[string]any{
		"release_name":     nestedString(app.Object, "spec", "release", "releaseName"),
		"resource_version": app.GetResourceVersion(),
	}
	phase := nestedString(app.Object, "status", "phase")
	deleting := app.GetDeletionTimestamp() != nil || phase == string(applicationv1.PhaseDeleting)
	if phase == string(applicationv1.PhaseFailed) && !deleted && !deleting {
		payload["error_msg"] = applicationError(app)
		return ChartEvent{Event: "chart_failed", Payload: payload}
	}
	state := "installing"
	switch {
	case deleted:
		state = "done"
	case deleting:
		state = "deleting"
	case phase == string(applicationv1.PhaseReady):
		state = "ready"
	}
	payload["state"] = state
	return ChartEvent{Event: "chart_state_changed", Payload: payload}
}

func applicationEventSignature(obj any) string {
	app, ok := applicationFromEvent(obj)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%t\x00%s\x00%s", app.GetDeletionTimestamp() != nil,
		nestedString(app.Object, "status", "phase"), applicationError(app))
}

func applicationError(app *unstructured.Unstructured) string {
	message := nestedString(app.Object, "status", "lastError", "message")
	if message != "" {
		return message
	}
	reason := nestedString(app.Object, "status", "lastError", "reason")
	if reason != "" {
		return reason
	}
	return "Application installation failed"
}

func nestedString(object map[string]any, fields ...string) string {
	value, _, _ := unstructured.NestedString(object, fields...)
	return value
}

func applicationFromEvent(obj any) (*unstructured.Unstructured, bool) {
	if app, ok := obj.(*unstructured.Unstructured); ok {
		return app, true
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, false
	}
	app, ok := tombstone.Obj.(*unstructured.Unstructured)
	return app, ok
}
