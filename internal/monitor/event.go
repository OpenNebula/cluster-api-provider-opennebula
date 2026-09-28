/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

type NodeReadyEvent struct {
	Event   string           `json:"event"`
	Payload NodeReadyPayload `json:"payload"`
}

type NodeReadyPayload struct {
	VMID  int  `json:"vm_id"`
	Ready bool `json:"ready"`
}

func nodeReadyEvent(node *corev1.Node) (NodeReadyEvent, error) {
	// CAPONE provider IDs are the stable bridge between a Kubernetes Node and
	// the OpenNebula VM expected by the OneKS event API
	vmID, err := vmIDFromProviderID(node.Spec.ProviderID)
	if err != nil {
		return NodeReadyEvent{}, err
	}
	return NodeReadyEvent{Event: "node_ready", Payload: NodeReadyPayload{VMID: vmID, Ready: nodeReady(node)}}, nil
}

func vmIDFromProviderID(value string) (int, error) {
	providerID, found := strings.CutPrefix(value, "one://")
	if !found {
		return 0, fmt.Errorf("unsupported provider ID %q", value)
	}
	vmID, err := strconv.Atoi(providerID)
	if err != nil {
		return 0, fmt.Errorf("invalid OpenNebula VM ID in provider ID %q: %w", value, err)
	}
	return vmID, nil
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	// Until Kubernetes publishes a Ready condition, treat the node as not ready
	return false
}
