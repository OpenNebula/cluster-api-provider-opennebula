/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// PodSnapshot is the complete current Pod inventory of one workload cluster.
// OneKS atomically replaces the preceding inventory when this callback arrives.
type PodSnapshot struct {
	Kind       string               `json:"kind"`
	ObservedAt string               `json:"observedAt"`
	Pods       map[string]PodStatus `json:"pods"`
}

func (snapshot PodSnapshot) CallbackKind() string { return snapshot.Kind }

// PodStatus contains only the fields needed by the OneKS overview. Status is
// the user-facing state derived from Kubernetes phase, readiness and deletion.
type PodStatus struct {
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Restarts   int32  `json:"restarts"`
	ProviderID string `json:"providerID"`
}

func podSnapshot(items []any, nodes cache.Store, observedAt time.Time) PodSnapshot {
	snapshot := PodSnapshot{
		Kind:       "PodSnapshot",
		ObservedAt: observedAt.Format(time.RFC3339),
		Pods:       make(map[string]PodStatus, len(items)),
	}
	for _, item := range items {
		pod, ok := item.(*corev1.Pod)
		if !ok {
			continue
		}
		snapshot.Pods[pod.Namespace+"/"+pod.Name] = podStatus(
			pod, providerIDFor(nodes, pod.Spec.NodeName),
		)
	}
	return snapshot
}

func providerIDFor(nodes cache.Store, nodeName string) string {
	if nodeName == "" {
		return ""
	}
	item, exists, err := nodes.GetByKey(nodeName)
	node, ok := item.(*corev1.Node)
	if err != nil || !exists || !ok {
		return ""
	}
	return node.Spec.ProviderID
}

func podStatus(pod *corev1.Pod, providerID string) PodStatus {
	status, reason, restarts := summarizePod(pod)
	return PodStatus{
		Status:     status,
		Reason:     reason,
		Restarts:   restarts,
		ProviderID: providerID,
	}
}

func summarizePod(pod *corev1.Pod) (string, string, int32) {
	ready := false
	conditionReason := ""
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			ready = condition.Status == corev1.ConditionTrue
		}
		if conditionReason == "" &&
			condition.Status == corev1.ConditionFalse {
			conditionReason = condition.Reason
		}
	}

	waitingReason := ""
	terminatedReason := ""
	var restarts int32
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.ContainerStatuses,
		pod.Status.InitContainerStatuses,
	} {
		for _, container := range statuses {
			restarts += container.RestartCount
			if waitingReason == "" && container.State.Waiting != nil {
				waitingReason = container.State.Waiting.Reason
			}
			if terminatedReason == "" && container.State.Terminated != nil &&
				container.State.Terminated.Reason != "Completed" {
				terminatedReason = container.State.Terminated.Reason
			}
		}
	}

	status := string(pod.Status.Phase)
	if status == "" {
		status = string(corev1.PodPending)
	}
	if pod.DeletionTimestamp != nil {
		return "Terminating", "Terminating", restarts
	}
	if pod.Status.Phase == corev1.PodRunning && !ready {
		status = "NotReady"
	}

	reason := waitingReason
	if reason == "" {
		reason = pod.Status.Reason
	}
	if reason == "" && pod.Status.Phase == corev1.PodFailed {
		reason = terminatedReason
	}
	if reason == "" {
		reason = conditionReason
	}
	return status, reason, restarts
}
