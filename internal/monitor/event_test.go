/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestNodeReadyEvent(t *testing.T) {
	for _, test := range []struct {
		name      string
		condition corev1.ConditionStatus
		ready     bool
	}{
		{name: "ready", condition: corev1.ConditionTrue, ready: true},
		{name: "not ready", condition: corev1.ConditionFalse},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := &corev1.Node{
				Spec: corev1.NodeSpec{ProviderID: "one://2"},
				Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
					Type: corev1.NodeReady, Status: test.condition,
				}}},
			}
			event, err := nodeReadyEvent(node)
			if err != nil {
				t.Fatal(err)
			}
			if event.Event != "node_ready" || event.Payload.VMID != 2 || event.Payload.Ready != test.ready {
				t.Fatalf("unexpected event: %#v", event)
			}
		})
	}
}

func TestNodeReadyEventRejectsInvalidProviderID(t *testing.T) {
	if _, err := nodeReadyEvent(&corev1.Node{Spec: corev1.NodeSpec{ProviderID: "one://invalid"}}); err == nil {
		t.Fatal("expected an invalid provider ID error")
	}
}
