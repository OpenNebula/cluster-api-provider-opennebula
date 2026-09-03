/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitor

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func TestPodSnapshotContainsScheduledAndUnscheduledPods(t *testing.T) {
	nodes := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := nodes.Add(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{ProviderID: "one://317"},
	}); err != nil {
		t.Fatal(err)
	}
	ready := corev1.ConditionTrue
	items := []any{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "api", UID: "api-uid"},
			Spec:       corev1.PodSpec{NodeName: "worker-1"},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "queued", UID: "queued-uid"},
			Status:     corev1.PodStatus{Phase: corev1.PodPending, Reason: "Unschedulable"},
		},
	}

	snapshot := podSnapshot(items, nodes, time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))
	if snapshot.Kind != "PodSnapshot" || snapshot.ObservedAt != "2026-09-02T12:00:00Z" {
		t.Fatalf("unexpected snapshot envelope: %#v", snapshot)
	}
	if got := snapshot.Pods["payments/api"]; got.ProviderID != "one://317" || got.Status != "Running" {
		t.Fatalf("unexpected scheduled Pod: %#v", got)
	}
	if got := snapshot.Pods["payments/queued"]; got.ProviderID != "" || got.Status != "Pending" || got.Reason != "Unschedulable" {
		t.Fatalf("unexpected unscheduled Pod: %#v", got)
	}
}

func TestPodStatusDistinguishesRunningButNotReady(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "api", UID: "api-uid"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				RestartCount: 6,
				State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}

	got := podStatus(pod, "")
	if got.Status != "NotReady" || got.Reason != "CrashLoopBackOff" || got.Restarts != 6 {
		t.Fatalf("unexpected Pod status: %#v", got)
	}
}

func TestPodStatusMarksDeletingPodTerminating(t *testing.T) {
	now := metav1.NewTime(time.Now())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", DeletionTimestamp: &now},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	got := podStatus(pod, "one://317")
	if got.Status != "Terminating" || got.Reason != "Terminating" {
		t.Fatalf("unexpected terminating status: %#v", got)
	}
}

func TestHealthyPodIgnoresCompletedInitContainerReason(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			InitContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
			}},
		},
	}
	if got := podStatus(pod, ""); got.Reason != "" || got.Status != "Running" {
		t.Fatalf("healthy Pod inherited init-container reason: %#v", got)
	}
}
