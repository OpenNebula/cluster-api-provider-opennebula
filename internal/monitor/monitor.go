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
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
)

type Monitor struct {
	nodeFactory informers.SharedInformerFactory
	nodes       cache.SharedIndexInformer
	sender      Sender
	queue       workqueue.TypedRateLimitingInterface[string]
	ready       atomic.Bool
}

func New(client kubernetes.Interface, sender Sender) (*Monitor, error) {
	m := &Monitor{
		sender: sender,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
	}
	m.nodeFactory = informers.NewSharedInformerFactory(client, 0)
	m.nodes = m.nodeFactory.Core().V1().Nodes().Informer()
	if _, err := m.nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    m.onNode,
		UpdateFunc: func(_, node any) { m.onNode(node) },
	}); err != nil {
		return nil, fmt.Errorf("register node handler: %w", err)
	}
	return m, nil
}

func (m *Monitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	m.nodeFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.nodes.HasSynced) {
		return fmt.Errorf("initial informer cache sync failed")
	}
	m.ready.Store(true)
	defer m.ready.Store(false)
	ctrl.Log.WithName("monitor").Info("monitor cache synchronized")
	go func() {
		<-ctx.Done()
		m.queue.ShutDown()
	}()
	for m.processNext(ctx) {
	}
	return nil
}

func (m *Monitor) Ready() bool { return m.ready.Load() }

func (m *Monitor) onNode(obj any) {
	node, ok := obj.(*corev1.Node)
	if !ok || node.Spec.ProviderID == "" {
		return
	}
	m.queue.Add(node.Name)
}

func (m *Monitor) processNext(ctx context.Context) bool {
	name, shutdown := m.queue.Get()
	if shutdown {
		return false
	}
	defer m.queue.Done(name)

	obj, exists, err := m.nodes.GetStore().GetByKey(name)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "unable to read node from cache", "node", name)
		m.queue.AddRateLimited(name)
		return true
	}
	if !exists {
		m.queue.Forget(name)
		return true
	}
	event, err := nodeReadyEvent(obj.(*corev1.Node))
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "node event was not sent", "node", name)
		m.queue.Forget(name)
		return true
	}
	if err := m.sender.Send(ctx, event); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "unable to send node event", "node", name)
		m.queue.AddRateLimited(name)
		return true
	}
	m.queue.Forget(name)
	return true
}
