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

// NodeMonitor reports initial node readiness and later readiness transitions.
// OpenNebula metadata is resolved lazily instead of duplicating it on the Node.
type NodeMonitor struct {
	nodeFactory informers.SharedInformerFactory
	nodes       cache.SharedIndexInformer
	publisher   Publisher
	resolver    PlacementResolver
	queue       workqueue.TypedRateLimitingInterface[string]
	ready       atomic.Bool
}

func NewNodeMonitor(
	client kubernetes.Interface,
	publisher Publisher,
	resolver PlacementResolver,
) (*NodeMonitor, error) {
	m := &NodeMonitor{
		publisher: publisher,
		resolver:  resolver,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
	}
	m.nodeFactory = informers.NewSharedInformerFactory(client, 0)
	m.nodes = m.nodeFactory.Core().V1().Nodes().Informer()
	if _, err := m.nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    m.onNode,
		UpdateFunc: m.onNodeUpdate,
	}); err != nil {
		return nil, fmt.Errorf("register node handler: %w", err)
	}
	return m, nil
}

func (m *NodeMonitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	m.nodeFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.nodes.HasSynced) {
		return fmt.Errorf("initial informer cache sync failed")
	}
	m.ready.Store(true)
	defer m.ready.Store(false)
	ctrl.Log.WithName("monitor").Info("node monitor cache synchronized")
	go func() {
		<-ctx.Done()
		m.queue.ShutDown()
	}()
	for m.processNext(ctx) {
	}
	return nil
}

func (m *NodeMonitor) Ready() bool { return m.ready.Load() }

func (m *NodeMonitor) onNode(obj any) {
	node, ok := obj.(*corev1.Node)
	if !ok || node.Spec.ProviderID == "" {
		return
	}
	ctrl.Log.WithName("monitor").Info("node event received", "node", node.Name, "providerID", node.Spec.ProviderID)
	m.queue.Add(node.Name)
}

func (m *NodeMonitor) onNodeUpdate(oldObj, newObj any) {
	oldNode, oldOK := oldObj.(*corev1.Node)
	newNode, newOK := newObj.(*corev1.Node)
	if !oldOK || !newOK {
		return
	}
	// Ignore status churn that cannot change the event sent to OneKS
	if oldNode.Spec.ProviderID == newNode.Spec.ProviderID && nodeReady(oldNode) == nodeReady(newNode) {
		return
	}
	m.onNode(newNode)
}

func (m *NodeMonitor) processNext(ctx context.Context) bool {
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
		// Invalid node metadata is not transient. A later Kubernetes update will
		// enqueue the node again if the provider ID is corrected
		ctrl.LoggerFrom(ctx).Error(err, "node event was not sent", "node", name)
		m.queue.Forget(name)
		return true
	}
	groupID, err := m.resolver.GroupForVM(ctx, event.Payload.VMID)
	if err != nil {
		// OpenNebula lookups and callback delivery are transient operations, so
		// both use the workqueue rate limiter instead of dropping the event
		ctrl.LoggerFrom(ctx).Error(err, "unable to resolve node event destination",
			"node", name,
			"event", event.Event,
			"vmID", event.Payload.VMID,
			"ready", event.Payload.Ready,
		)
		m.queue.AddRateLimited(name)
		return true
	}
	log := ctrl.LoggerFrom(ctx).WithValues(
		"node", name,
		"event", event.Event,
		"vmID", event.Payload.VMID,
		"ready", event.Payload.Ready,
		"groupID", groupID,
	)
	log.Info("sending node event")
	if err := m.publisher.PublishNodeReady(ctx, groupID, event); err != nil {
		log.Error(err, "unable to send node event")
		m.queue.AddRateLimited(name)
		return true
	}
	log.Info("node event sent")
	m.queue.Forget(name)
	return true
}
