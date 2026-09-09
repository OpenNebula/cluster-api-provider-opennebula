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
)

type senderFunc func(context.Context, Event) error

func (f senderFunc) Send(ctx context.Context, event Event) error { return f(ctx, event) }

func TestMonitorDoesNotSendDeletedNodeEvents(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{ProviderID: "one://2"},
	}
	client := fake.NewSimpleClientset(node)
	events := make(chan Event, 2)
	monitor, err := New(client, senderFunc(func(_ context.Context, event Event) error {
		events <- event
		return nil
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
