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
	report := nodeReport(node, "Updated")
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
	report := nodeReport(node, "Updated")
	if report.Status["state"] != "warning" {
		t.Fatalf("expected warning node, got %#v", report.Status)
	}
}

func TestApplicationReportProjectsOnlyCorrelationAndStatus(t *testing.T) {
	report := applicationReport(applicationObject(), "Updated")
	if report.Kind != "OneKSApplication" || report.Namespace != "oneks-system" || report.Name != "runai" {
		t.Fatalf("unexpected application identity: %#v", report)
	}
	if report.Status["clusterID"] != "42" || report.Status["catalogueChartID"] != "runai-chart" ||
		report.Status["planDigest"] != "sha256-plan" || report.Status["releaseName"] != "runai" {
		t.Fatalf("missing application correlation: %#v", report.Status)
	}
	status, ok := report.Status["application"].(map[string]any)
	if !ok || status["phase"] != "Ready" {
		t.Fatalf("missing application status: %#v", report.Status)
	}
	if _, leaked := report.Status["valuesContent"]; leaked {
		t.Fatalf("application report leaked release values: %#v", report.Status)
	}
}

func TestApplicationReportUsesEmptyStatusBeforeFirstReconcile(t *testing.T) {
	app := applicationObject()
	delete(app.Object, "status")
	report := applicationReport(app, "Added")
	status, ok := report.Status["application"].(map[string]any)
	if !ok || len(status) != 0 {
		t.Fatalf("expected an empty application status object, got %#v", report.Status)
	}
}

func applicationObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "oneks.opennebula.io/v1alpha1", "kind": "OneKSApplication",
		"metadata": map[string]any{
			"name": "runai", "namespace": "oneks-system", "uid": "app-uid",
			"resourceVersion": "17", "generation": int64(1),
		},
		"spec": map[string]any{
			"clusterID": "42", "catalogueChartID": "runai-chart", "planDigest": "sha256-plan",
			"release": map[string]any{"releaseName": "runai", "valuesContent": "sensitive"},
		},
		"status": map[string]any{"phase": "Ready", "observedGeneration": int64(1), "observedPlanDigest": "sha256-plan"},
	}}
}
