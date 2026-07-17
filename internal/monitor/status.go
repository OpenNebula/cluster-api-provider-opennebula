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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func nodeReport(config Config, node *corev1.Node, event string) Report {
	state := "warning"
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			state = "ready"
			break
		}
	}
	return Report{
		ClusterID: config.ClusterID, Kind: "Node", Name: node.Name,
		UID: string(node.UID), ResourceVersion: node.ResourceVersion, Event: event, ObservedAt: time.Now().UTC(),
		Status: map[string]any{"state": state},
	}
}

func chartReport(config Config, chart *unstructured.Unstructured, status, event string) (Report, bool) {
	chartID, watched := config.watchesChart(chart.GetAnnotations())
	if !watched {
		return Report{}, false
	}
	return Report{
		ClusterID: config.ClusterID, Kind: "HelmChart", Namespace: chart.GetNamespace(), Name: chart.GetName(),
		UID: string(chart.GetUID()), ResourceVersion: chart.GetResourceVersion(), Event: event, ObservedAt: time.Now().UTC(),
		Status: map[string]any{"chartId": chartID, "status": normalizedChartStatus(status)},
	}, true
}

func normalizedChartStatus(status string) string {
	switch status {
	case "pending", "deployed", "failed", "uninstalling":
		return status
	default:
		return "unknown"
	}
}
