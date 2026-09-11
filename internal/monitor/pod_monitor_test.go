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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodSnapshotsGroupPodsByGroupAndVM(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}, Spec: corev1.NodeSpec{ProviderID: "one://2"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}, Spec: corev1.NodeSpec{ProviderID: "one://3"}},
	}
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments"},
			Spec:       corev1.PodSpec{NodeName: "worker-1"},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "queued", Namespace: "payments"},
			Status:     corev1.PodStatus{Phase: corev1.PodPending, Reason: "Unschedulable"},
		},
	}
	resolver := destinationResolverFunc(func(_ context.Context, vmID int) (NodeGroupEventDestination, error) {
		return NodeGroupEventDestination{ClusterID: 0, GroupID: vmID - 1}, nil
	})

	snapshots, resolutionErrors := podSnapshots(context.Background(), nodes, pods, resolver)
	if len(resolutionErrors) != 0 {
		t.Fatalf("unexpected resolution errors: %v", resolutionErrors)
	}
	encoded, err := json.Marshal(snapshots[0])
	if err != nil {
		t.Fatal(err)
	}
	want := `{"1":{"2":[{"pod":"api","namespace":"payments","state":"Running","reason":""}]},"2":{"3":[]}}`
	if string(encoded) != want {
		t.Fatalf("snapshot = %s, want %s", encoded, want)
	}
}

func TestPodReportIncludesFailureReason(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}

	got := podReport(pod)
	if got.State != "NotReady" || got.Reason != "CrashLoopBackOff" {
		t.Fatalf("unexpected Pod report: %#v", got)
	}
}

func TestNewPodMonitorRejectsNonPositiveInterval(t *testing.T) {
	if _, err := NewPodMonitor(fake.NewSimpleClientset(), nil, Config{}); err == nil {
		t.Fatal("expected a Pod poll interval validation error")
	}
}

func TestPodMonitorRunsPeriodically(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{ProviderID: "one://2"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments"},
		Spec:       corev1.PodSpec{NodeName: node.Name},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(node, pod)
	reports := make(chan PodSnapshot, 3)
	monitor := newPodMonitor(
		client,
		senderFunc(func(_ context.Context, destination Destination, payload any) error {
			if destination != (ClusterPodsDestination{ClusterID: 15}) {
				t.Fatalf("unexpected destination: %#v", destination)
			}
			report, ok := payload.(PodSnapshot)
			if !ok {
				t.Fatalf("unexpected payload type: %T", payload)
			}
			reports <- report
			return nil
		}),
		destinationResolverFunc(func(_ context.Context, _ int) (NodeGroupEventDestination, error) {
			return NodeGroupEventDestination{ClusterID: 15, GroupID: 16}, nil
		}),
		10*time.Millisecond,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	for range 2 {
		select {
		case <-reports:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("periodic Pod snapshot was not sent")
		}
	}
	if !monitor.Ready() {
		t.Fatal("Pod monitor is not ready after a successful poll")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Pod monitor stopped with an error: %v", err)
	}
}
