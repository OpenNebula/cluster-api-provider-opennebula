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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/util/workqueue"
)

type publisherStub struct {
	nodeReady    func(context.Context, int, NodeReadyEvent) error
	pods         func(context.Context, PodSnapshot) error
	observations func(context.Context, ObservationSnapshot) error
	chart        func(context.Context, ChartEvent) error
}

func (p publisherStub) PublishNodeReady(ctx context.Context, groupID int, event NodeReadyEvent) error {
	if p.nodeReady == nil {
		return nil
	}
	return p.nodeReady(ctx, groupID, event)
}

func (p publisherStub) ReplacePods(ctx context.Context, snapshot PodSnapshot) error {
	if p.pods == nil {
		return nil
	}
	return p.pods(ctx, snapshot)
}

func (p publisherStub) ReplaceObservations(ctx context.Context, snapshot ObservationSnapshot) error {
	if p.observations == nil {
		return nil
	}
	return p.observations(ctx, snapshot)
}

func (p publisherStub) PublishChartEvent(ctx context.Context, event ChartEvent) error {
	if p.chart == nil {
		return nil
	}
	return p.chart(ctx, event)
}

type placementResolverFunc func(context.Context, int) (int, error)

func (f placementResolverFunc) GroupForVM(ctx context.Context, vmID int) (int, error) {
	return f(ctx, vmID)
}

func TestNodeMonitorOnlyQueuesRelevantNodeUpdates(t *testing.T) {
	monitor := &NodeMonitor{
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
	}
	t.Cleanup(monitor.queue.ShutDown)

	oldNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{ProviderID: "one://2"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	}
	metadataUpdate := oldNode.DeepCopy()
	metadataUpdate.Annotations = map[string]string{"example.com/heartbeat": "updated"}

	monitor.onNodeUpdate(oldNode, metadataUpdate)
	if monitor.queue.Len() != 0 {
		t.Fatal("metadata-only update was queued")
	}

	notReadyUpdate := metadataUpdate.DeepCopy()
	notReadyUpdate.Status.Conditions[0].Status = corev1.ConditionFalse
	monitor.onNodeUpdate(metadataUpdate, notReadyUpdate)
	if monitor.queue.Len() != 1 {
		t.Fatalf("readiness update queue length = %d, want 1", monitor.queue.Len())
	}
	item, shutdown := monitor.queue.Get()
	if shutdown {
		t.Fatal("queue unexpectedly shut down")
	}
	monitor.queue.Done(item)
	monitor.queue.Forget(item)

	providerUpdate := notReadyUpdate.DeepCopy()
	providerUpdate.Spec.ProviderID = "one://3"
	monitor.onNodeUpdate(notReadyUpdate, providerUpdate)
	if monitor.queue.Len() != 1 {
		t.Fatalf("provider ID update queue length = %d, want 1", monitor.queue.Len())
	}
}

func TestNodeMonitorDoesNotSendDeletedNodeEvents(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{ProviderID: "one://2"},
	}
	client := fake.NewSimpleClientset(node)
	events := make(chan NodeReadyEvent, 2)
	monitor, err := NewNodeMonitor(client, publisherStub{nodeReady: func(
		_ context.Context, groupID int, event NodeReadyEvent,
	) error {
		if groupID != 16 {
			t.Errorf("unexpected group ID: %d", groupID)
		}
		events <- event
		return nil
	}}, placementResolverFunc(func(_ context.Context, vmID int) (int, error) {
		if vmID != 2 {
			t.Errorf("unexpected VM ID: %d", vmID)
		}
		return 16, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("monitor stopped: %v", err)
		}
	})

	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("added node event was not sent")
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if _, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if !event.Payload.Ready {
			t.Fatalf("updated node event is not ready: %#v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("updated node event was not sent")
	}
	if err := client.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("unexpected deleted node event: %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}
