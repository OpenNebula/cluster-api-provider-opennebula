/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
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
		"False", modelLoadFailedReason, "runtime could not load model", "",
	)
	id, event, err := modelDeploymentEvent(resource, false)
	if err != nil || event.Event == "" {
		t.Fatalf("event=%#v err=%v", event, err)
	}
	if id != 42 || event.Event != "deployment_failed" ||
		event.Payload.Reason != modelLoadFailedReason ||
		event.Payload.ErrorMessage != "runtime could not load model" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestModelDeploymentTransientReconcileErrorIsIgnored(t *testing.T) {
	resource := testLLMInferenceService(
		"False", "ReconcileSingleNodeWorkloadError", "cannot reconcile workload", "",
	)
	resource.SetCreationTimestamp(metav1.Now())
	_, event, err := modelDeploymentEvent(resource, false)
	if err != nil || event.Event != "" {
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

func TestModelDeploymentSignatureIncludesTerminalComponentFailure(t *testing.T) {
	original := testLLMInferenceService(
		kubernetesConditionUnknown, "PredictorNotReady", "waiting", "",
	)
	original.Object["status"].(map[string]any)["conditions"] = append(
		original.Object["status"].(map[string]any)["conditions"].([]any),
		map[string]any{
			"type": "PredictorReady", "status": "True", "reason": "Ready",
		},
	)
	updated := original.DeepCopy()
	conditions := updated.Object["status"].(map[string]any)["conditions"].([]any)
	conditions[1] = map[string]any{
		"type": "PredictorReady", "status": "False", "reason": modelLoadFailedReason,
		"message": "runtime could not load model",
	}

	monitor := &ModelDeploymentMonitor{
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		pending: make(map[string]pendingModelDeployment),
	}
	t.Cleanup(monitor.queue.ShutDown)
	monitor.onUpdate(original, updated)
	if monitor.queue.Len() != 1 {
		t.Fatalf("component-only failure queued %d items, want 1", monitor.queue.Len())
	}
	_, event, err := modelDeploymentEvent(updated, false)
	if err != nil || event.Event != "deployment_failed" ||
		event.Payload.Reason != modelLoadFailedReason {
		t.Fatalf("unexpected event: %#v err=%v", event, err)
	}
}

func TestModelDeploymentWaitsForObservedGeneration(t *testing.T) {
	for _, status := range []string{"True", "False"} {
		t.Run(status, func(t *testing.T) {
			resource := testLLMInferenceService(
				status, modelLoadFailedReason, "", "https://model.example.com",
			)
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

func TestModelDeploymentRestartRecoversProtectedDeletion(t *testing.T) {
	resource := testLLMInferenceService("True", "Ready", "", "https://model.example.com")
	now := metav1.Now()
	resource.SetDeletionTimestamp(&now)
	resource.SetFinalizers([]string{modelDeploymentFinalizer})
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), resource.DeepCopy())
	var published ModelDeploymentEvent
	monitor := &ModelDeploymentMonitor{
		deployments: dynamicClient.Resource(modelDeploymentGVR),
		publisher: publisherStub{deployment: func(
			_ context.Context, _ int, event ModelDeploymentEvent,
		) error {
			published = event
			return nil
		}},
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		pending: make(map[string]pendingModelDeployment),
	}
	t.Cleanup(monitor.queue.ShutDown)

	// An informer initial-list Add after restart must recover the deletion.
	monitor.enqueue(resource, false)
	monitor.processNext(context.Background())
	if published.Event != "resources_deleted" {
		t.Fatalf("unexpected callback: %#v", published)
	}
	current, err := dynamicClient.Resource(modelDeploymentGVR).
		Namespace(resource.GetNamespace()).
		Get(context.Background(), resource.GetName(), metav1.GetOptions{})
	if err == nil && slices.Contains(current.GetFinalizers(), modelDeploymentFinalizer) {
		t.Fatal("monitor finalizer was not removed after callback delivery")
	}
}

func TestModelDeploymentRunRetriesInitialDiscoveryFailure(t *testing.T) {
	discoveryClient := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	discoveryClient.Resources = []*metav1.APIResourceList{{
		GroupVersion: modelDeploymentGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: modelDeploymentGVR.Resource}},
	}}
	firstCall := true
	discoveryClient.PrependReactor("get", "resource", func(
		k8stesting.Action,
	) (bool, runtime.Object, error) {
		if firstCall {
			firstCall = false
			return true, nil, errors.New("temporary discovery failure")
		}
		return false, nil, nil
	})
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{modelDeploymentGVR: "LLMInferenceServiceList"},
	)
	monitor, err := NewModelDeploymentMonitor(dynamicClient, discoveryClient, publisherStub{})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	monitor.discoveryInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	deadline := time.After(time.Second)
	for !monitor.Ready() {
		select {
		case err := <-done:
			t.Fatalf("monitor exited during discovery retry: %v", err)
		case <-deadline:
			t.Fatal("monitor did not recover from discovery failure")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("monitor stopped with an error: %v", err)
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
