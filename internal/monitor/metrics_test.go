/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitor

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
)

func TestMetricsExposeStableLowCardinalityFamilies(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	metrics.setNodes([]*corev1.Node{
		{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
		}}},
		{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue},
			{Type: corev1.NodePIDPressure, Status: corev1.ConditionTrue},
		}}},
	})
	metrics.SetActiveSignals(0, 2, 1)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	want := map[string]bool{
		"capone_monitor_nodes_total":                             false,
		"capone_monitor_nodes_ready":                             false,
		"capone_monitor_nodes_not_ready":                         false,
		"capone_monitor_nodes_memory_pressure":                   false,
		"capone_monitor_nodes_disk_pressure":                     false,
		"capone_monitor_nodes_pid_pressure":                      false,
		"capone_monitor_callback_queue_depth":                    false,
		"capone_monitor_callback_attempts_total":                 false,
		"capone_monitor_callback_failures_total":                 false,
		"capone_monitor_callback_retries_total":                  false,
		"capone_monitor_callback_rejected_total":                 false,
		"capone_monitor_callback_last_success_timestamp_seconds": false,
		"capone_monitor_profile_parse_failures_total":            false,
		"capone_monitor_rule_evaluation_failures_total":          false,
		"capone_monitor_active_signals":                          false,
	}
	for _, family := range families {
		if _, tracked := want[family.GetName()]; tracked {
			want[family.GetName()] = true
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() != "severity" {
					t.Fatalf("metric %s has unexpected label %q", family.GetName(), label.GetName())
				}
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric family %s was not registered", name)
		}
	}
}
