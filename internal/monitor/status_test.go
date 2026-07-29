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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestNodeReportReady(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", ResourceVersion: "12"},
		Spec:       corev1.NodeSpec{ProviderID: "one://317"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady",
		}}},
	}
	report := nodeReport(Config{}, node, "Updated")
	if report.Status["state"] != "ready" {
		t.Fatalf("expected ready node, got %#v", report.Status)
	}
	if report.Status["providerID"] != "one://317" {
		t.Fatalf("expected providerID, got %#v", report.Status)
	}
}

func TestNodeReportWarning(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionFalse,
		}}},
	}
	report := nodeReport(Config{}, node, "Updated")
	if report.Status["state"] != "warning" {
		t.Fatalf("expected warning node, got %#v", report.Status)
	}
}

func TestChartReportUsesHelmStatus(t *testing.T) {
	report, watched := chartReport(testConfig(), chartObject(), "deployed", "Updated")
	if !watched {
		t.Fatal("expected annotated chart to be watched")
	}
	if report.Status["status"] != "deployed" || report.Status["chartId"] != "cni" {
		t.Fatalf("unexpected chart status: %#v", report.Status)
	}
}

func TestUnknownChartStatus(t *testing.T) {
	if status := normalizedChartStatus("superseded"); status != "unknown" {
		t.Fatalf("expected unknown, got %q", status)
	}
}

func TestSupportedChartStatuses(t *testing.T) {
	statuses := []string{
		"pending", "deployed", "failed", "uninstalling", "unknown",
	}
	for _, expected := range statuses {
		if actual := normalizedChartStatus(expected); actual != expected {
			t.Fatalf("expected %q, got %q", expected, actual)
		}
	}
}

func TestReconcileReport(t *testing.T) {
	report := reconcileReport(true)
	if report.Kind != "Reconcile" || report.Name != "chart-reconciler" {
		t.Fatalf("unexpected reconcile report: %#v", report)
	}
	if report.Event != "Updated" {
		t.Fatalf("unexpected reconcile event: %q", report.Event)
	}
	if report.Status["activeOperation"] != true {
		t.Fatalf("expected active chart operation: %#v", report.Status)
	}
}

func chartObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.cattle.io/v1", "kind": "HelmChart",
		"metadata": map[string]any{
			"name": "rke2-cni", "namespace": "kube-system",
			"annotations": map[string]any{defaultChartAnnotation: "cni"},
		},
		"status": map[string]any{"jobName": "helm-install-cni"},
	}}
}

func testConfig() Config {
	return Config{ChartAnnotation: defaultChartAnnotation}
}
