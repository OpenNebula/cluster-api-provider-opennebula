/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestModelDeploymentAPIDiscovery(t *testing.T) {
	fake := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	monitor := &ModelDeploymentMonitor{discovery: fake}
	available, err := monitor.apiAvailable()
	if err != nil || available {
		t.Fatalf("missing API: available=%t err=%v", available, err)
	}
	fake.Resources = []*metav1.APIResourceList{{
		GroupVersion: modelDeploymentGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: modelDeploymentGVR.Resource}},
	}}
	available, err = monitor.apiAvailable()
	if err != nil || !available {
		t.Fatalf("installed API: available=%t err=%v", available, err)
	}
}

func TestModelDeploymentReadyEventIncludesURL(t *testing.T) {
	resource := testLLMInferenceService("True", "Ready", "", "https://model.example.com")
	id, event, err := modelDeploymentEvent(resource, false)
	if err != nil || event.Event == "" {
		t.Fatalf("event=%#v err=%v", event, err)
	}
	if id != 42 || event.Event != "inference_ready" ||
		event.Payload.URL != "https://model.example.com" ||
		event.Payload.ResourceVersion != json.Number("17") {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestModelDeploymentIgnoresTransientNotReady(t *testing.T) {
	resource := testLLMInferenceService(
		"False", "WaitingForGateway", "Gateway has not accepted the route", "",
	)
	_, event, err := modelDeploymentEvent(resource, false)
	if err != nil || event.Event != "" {
		t.Fatalf("event=%#v err=%v", event, err)
	}
}

func TestModelDeploymentFailureIncludesControllerMessage(t *testing.T) {
	resource := testLLMInferenceService(
		"False", "ModelLoadFailed", "runtime could not load model", "",
	)
	id, event, err := modelDeploymentEvent(resource, false)
	if err != nil || event.Event == "" {
		t.Fatalf("event=%#v err=%v", event, err)
	}
	if id != 42 || event.Event != "deployment_failed" ||
		event.Payload.Reason != "ModelLoadFailed" ||
		event.Payload.ErrorMessage != "runtime could not load model" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestModelDeploymentErrorReasonIsReported(t *testing.T) {
	resource := testLLMInferenceService(
		"False", "ReconcileSingleNodeWorkloadError", "cannot reconcile workload", "",
	)
	id, event, err := modelDeploymentEvent(resource, false)
	if err != nil || id != 42 || event.Event != "deployment_failed" ||
		event.Payload.Reason != "ReconcileSingleNodeWorkloadError" {
		t.Fatalf("unexpected event: %#v err=%v", event, err)
	}
}

func TestModelDeploymentConfigurationMissingIsReported(t *testing.T) {
	resource := testLLMInferenceService("False", "ConfigNotFound", "configuration missing", "")
	_, event, err := modelDeploymentEvent(resource, false)
	if err != nil || event.Event != "deployment_failed" {
		t.Fatalf("unexpected event: %#v err=%v", event, err)
	}
}

func TestModelDeploymentSignatureIncludesDeploymentID(t *testing.T) {
	original := testLLMInferenceService("True", "Ready", "", "https://model.example.com")
	updated := original.DeepCopy()
	updated.SetLabels(map[string]string{modelDeploymentIDLabel: "43"})
	if modelDeploymentEventSignature(original) == modelDeploymentEventSignature(updated) {
		t.Fatal("deployment ID change must trigger an update")
	}
}

func TestModelDeploymentWaitsForObservedGeneration(t *testing.T) {
	for _, status := range []string{"True", "False"} {
		t.Run(status, func(t *testing.T) {
			resource := testLLMInferenceService(status, "ModelLoadFailed", "", "https://model.example.com")
			resource.SetGeneration(resource.GetGeneration() + 1)
			staleSignature := modelDeploymentEventSignature(resource)
			if _, event, err := modelDeploymentEvent(resource, false); err != nil || event.Event != "" {
				t.Fatalf("stale status produced event=%#v err=%v", event, err)
			}

			resource.Object["status"].(map[string]any)["observedGeneration"] = resource.GetGeneration()
			if staleSignature == modelDeploymentEventSignature(resource) {
				t.Fatal("generation acknowledgment must trigger an update")
			}
			if _, event, err := modelDeploymentEvent(resource, false); err != nil || event.Event == "" {
				t.Fatalf("current status produced event=%#v err=%v", event, err)
			}
		})
	}
}

func TestModelDeploymentDeletionEvent(t *testing.T) {
	resource := testLLMInferenceService("True", "Ready", "", "https://model.example.com")
	now := metav1.Now()
	resource.SetDeletionTimestamp(&now)
	if _, event, err := modelDeploymentEvent(resource, false); err != nil || event.Event != "" {
		t.Fatalf("terminating resource produced event=%#v err=%v", event, err)
	}
	id, event, err := modelDeploymentEvent(resource, true)
	if err != nil || id != 42 || event.Event != "resources_deleted" {
		t.Fatalf("event=%#v err=%v", event, err)
	}
}

func TestModelDeploymentWaitsForURL(t *testing.T) {
	resource := testLLMInferenceService("True", "Ready", "", "https://model.example.com")
	resource.Object["status"].(map[string]any)["url"] = ""
	if _, event, err := modelDeploymentEvent(resource, false); err != nil || event.Event != "" {
		t.Fatalf("empty URL event=%#v err=%v", event, err)
	}
}

func testLLMInferenceService(status, reason, message, url string) *unstructured.Unstructured {
	resource := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "serving.kserve.io/v1alpha2",
		"kind":       "LLMInferenceService",
		"status": map[string]any{
			"url":                url,
			"observedGeneration": int64(2),
			"conditions": []any{map[string]any{
				"type": "Ready", "status": status, "reason": reason, "message": message,
			}},
		},
	}}
	resource.SetName("onellm-42")
	resource.SetNamespace("onellm-system")
	resource.SetGeneration(2)
	resource.SetResourceVersion("17")
	resource.SetLabels(map[string]string{modelDeploymentIDLabel: "42"})
	return resource
}
