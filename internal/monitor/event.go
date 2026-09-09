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

type Event struct {
	Event   string           `json:"event"`
	Payload NodeReadyPayload `json:"payload"`
}

type NodeReadyPayload struct {
	VMID  int  `json:"vm_id"`
	Ready bool `json:"ready"`
}

func nodeReadyEvent(node *corev1.Node) (Event, error) {
	providerID, found := strings.CutPrefix(node.Spec.ProviderID, "one://")
	if !found {
		return Event{}, fmt.Errorf("unsupported provider ID %q", node.Spec.ProviderID)
	}
	vmID, err := strconv.Atoi(providerID)
	if err != nil {
		return Event{}, fmt.Errorf("invalid OpenNebula VM ID in provider ID %q: %w", node.Spec.ProviderID, err)
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			ready = condition.Status == corev1.ConditionTrue
			break
		}
	}
	return Event{Event: "node_ready", Payload: NodeReadyPayload{VMID: vmID, Ready: ready}}, nil
}
