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
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

type PodReport struct {
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
}

// PodSnapshot groups Pods by OneKS group ID and then by OpenNebula VM ID.
type PodSnapshot map[int]map[int][]PodReport

type PodMonitor struct {
	client   kubernetes.Interface
	sender   Sender
	resolver nodeDestinationResolver
	interval time.Duration
	ready    atomic.Bool
}

func NewPodMonitor(client kubernetes.Interface, sender Sender, config Config) (*PodMonitor, error) {
	if config.PodPollInterval <= 0 {
		return nil, fmt.Errorf("Pod poll interval must be positive")
	}
	resolver, err := newGocaNodeDestinationResolver(config)
	if err != nil {
		return nil, err
	}
	return newPodMonitor(client, sender, resolver, config.PodPollInterval), nil
}

func newPodMonitor(
	client kubernetes.Interface,
	sender Sender,
	resolver nodeDestinationResolver,
	interval time.Duration,
) *PodMonitor {
	return &PodMonitor{client: client, sender: sender, resolver: resolver, interval: interval}
}

func (m *PodMonitor) Run(ctx context.Context) error {
	defer m.ready.Store(false)
	log := ctrl.LoggerFrom(ctx).WithName("pod-monitor")
	poll := func() {
		if err := m.poll(ctx); err != nil {
			log.Error(err, "unable to send pod snapshot")
		}
	}

	poll()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			poll()
		}
	}
}

func (m *PodMonitor) Ready() bool { return m.ready.Load() }

func (m *PodMonitor) poll(ctx context.Context) error {
	nodes, err := m.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	pods, err := m.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	m.ready.Store(true)

	snapshots, resolutionErrors := podSnapshots(ctx, nodes.Items, pods.Items, m.resolver)
	log := ctrl.LoggerFrom(ctx).WithName("pod-monitor")
	for _, resolutionErr := range resolutionErrors {
		log.Error(resolutionErr, "unable to resolve node while building pod snapshot")
	}
	var sendErrors []error
	for clusterID, snapshot := range snapshots {
		log.Info(
			"sending pod snapshot",
			"clusterID", clusterID,
			"groups", len(snapshot),
		)
		if err := m.sender.Send(ctx, ClusterPodsDestination{ClusterID: clusterID}, snapshot); err != nil {
			sendErrors = append(sendErrors, fmt.Errorf("send pod snapshot for cluster %d: %w", clusterID, err))
		}
	}
	return errors.Join(sendErrors...)
}

type resolvedNode struct {
	vmID      int
	placement NodeGroupEventDestination
}

func podSnapshots(
	ctx context.Context,
	nodes []corev1.Node,
	pods []corev1.Pod,
	resolver nodeDestinationResolver,
) (map[int]PodSnapshot, []error) {
	snapshots := make(map[int]PodSnapshot)
	resolvedNodes := make(map[string]resolvedNode, len(nodes))
	var resolutionErrors []error

	for i := range nodes {
		node := &nodes[i]
		if node.Spec.ProviderID == "" {
			continue
		}
		vmID, err := vmIDFromProviderID(node.Spec.ProviderID)
		if err != nil {
			resolutionErrors = append(resolutionErrors, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}
		placement, err := resolver.Resolve(ctx, vmID)
		if err != nil {
			resolutionErrors = append(resolutionErrors, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}
		resolvedNodes[node.Name] = resolvedNode{vmID: vmID, placement: placement}

		snapshot := snapshots[placement.ClusterID]
		if snapshot == nil {
			snapshot = make(PodSnapshot)
			snapshots[placement.ClusterID] = snapshot
		}
		if _, found := snapshot[placement.GroupID]; !found {
			snapshot[placement.GroupID] = make(map[int][]PodReport)
		}
		if _, found := snapshot[placement.GroupID][vmID]; !found {
			snapshot[placement.GroupID][vmID] = []PodReport{}
		}
	}

	for i := range pods {
		pod := &pods[i]
		node, found := resolvedNodes[pod.Spec.NodeName]
		if !found {
			continue
		}
		snapshot := snapshots[node.placement.ClusterID]
		snapshot[node.placement.GroupID][node.vmID] = append(
			snapshot[node.placement.GroupID][node.vmID], podReport(pod),
		)
	}
	return snapshots, resolutionErrors
}

func podReport(pod *corev1.Pod) PodReport {
	state, reason := summarizePod(pod)
	return PodReport{Pod: pod.Name, Namespace: pod.Namespace, State: state, Reason: reason}
}

func summarizePod(pod *corev1.Pod) (string, string) {
	ready := false
	conditionReason := ""
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			ready = condition.Status == corev1.ConditionTrue
		}
		if conditionReason == "" && condition.Status == corev1.ConditionFalse {
			conditionReason = condition.Reason
		}
	}

	waitingReason := ""
	terminatedReason := ""
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.ContainerStatuses,
		pod.Status.InitContainerStatuses,
	} {
		for _, container := range statuses {
			if waitingReason == "" && container.State.Waiting != nil {
				waitingReason = container.State.Waiting.Reason
			}
			if terminatedReason == "" && container.State.Terminated != nil &&
				container.State.Terminated.Reason != "Completed" {
				terminatedReason = container.State.Terminated.Reason
			}
		}
	}

	state := string(pod.Status.Phase)
	if state == "" {
		state = string(corev1.PodPending)
	}
	if pod.DeletionTimestamp != nil {
		return "Terminating", "Terminating"
	}
	if pod.Status.Phase == corev1.PodRunning && !ready {
		state = "NotReady"
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
	return state, reason
}
