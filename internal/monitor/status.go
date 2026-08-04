/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func nodeReport(config Config, node *corev1.Node, event string) Report {
	state := "warning"
	if nodeReady(node) {
		state = "ready"
	}
	return Report{
		Kind: "Node", Name: node.Name,
		UID: string(node.UID), ResourceVersion: node.ResourceVersion, Event: event,
		Status: map[string]any{"state": state, "providerID": node.Spec.ProviderID},
	}
}

func readinessJobReport(config Config, job *batchv1.Job, event string) (Report, bool) {
	if job.GetLabels()[readinessJobLabel] != "true" {
		return Report{}, false
	}
	chartID := job.GetAnnotations()[config.ChartAnnotation]
	releaseName := job.GetAnnotations()[readinessReleaseAnnotation]
	if chartID == "" || releaseName == "" {
		return Report{}, false
	}
	status, message := readinessJobStatus(job)
	if event == "Deleted" && status == "pending" {
		status = "failed"
		message = "readiness Job was deleted before completion"
	}
	result := Report{
		Kind: "ReadinessJob", Namespace: job.Namespace, Name: job.Name,
		UID: string(job.UID), ResourceVersion: job.ResourceVersion, Event: event,
		Status: map[string]any{
			"chartId": chartID, "releaseName": releaseName, "status": status,
		},
	}
	if message != "" {
		result.Status["message"] = message
	}
	return result, true
}

func readinessJobStatus(job *batchv1.Job) (string, string) {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobFailed:
			return "failed", condition.Message
		case batchv1.JobComplete:
			return "complete", condition.Message
		}
	}
	return "pending", ""
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func chartReport(config Config, chart *unstructured.Unstructured, status, event string) (Report, bool) {
	chartID, watched := config.watchesChart(chart.GetAnnotations())
	if !watched {
		return Report{}, false
	}
	return Report{
		Kind: "HelmChart", Namespace: chart.GetNamespace(), Name: chart.GetName(),
		UID: string(chart.GetUID()), ResourceVersion: chart.GetResourceVersion(), Event: event,
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

func reconcileReport(active bool) Report {
	return Report{
		Kind:  "Reconcile",
		Name:  "chart-reconciler",
		Event: "Updated",
		Status: map[string]any{
			"activeOperation": active,
		},
	}
}
