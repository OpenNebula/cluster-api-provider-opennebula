/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func nodeReport(node *corev1.Node, event string) Report {
	ready := nodeReady(node)
	if event == "Deleted" {
		ready = false
	}
	return Report{
		Kind: "Node", Name: node.Name,
		UID: string(node.UID), ResourceVersion: node.ResourceVersion, Event: event,
		Status: map[string]any{"providerID": node.Spec.ProviderID, "ready": ready},
	}
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func applicationReport(app *unstructured.Unstructured, event string) Report {
	status, _, _ := unstructured.NestedMap(app.Object, "status")
	if status == nil {
		status = map[string]any{}
	}
	clusterID, _, _ := unstructured.NestedString(app.Object, "spec", "clusterID")
	chartID, _, _ := unstructured.NestedString(app.Object, "spec", "catalogueChartID")
	planDigest, _, _ := unstructured.NestedString(app.Object, "spec", "planDigest")
	releaseName, _, _ := unstructured.NestedString(app.Object, "spec", "release", "releaseName")
	return Report{
		Kind: "OneKSApplication", Namespace: app.GetNamespace(), Name: app.GetName(),
		UID: string(app.GetUID()), ResourceVersion: app.GetResourceVersion(), Event: event,
		Status: map[string]any{
			"clusterID": clusterID, "catalogueChartID": chartID,
			"planDigest": planDigest, "releaseName": releaseName,
			"generation": app.GetGeneration(), "deleting": app.GetDeletionTimestamp() != nil,
			"application": status,
		},
	}
}
