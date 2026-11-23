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
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// ModelDeploymentEvent is a serving lifecycle callback consumed by OneKS.
type ModelDeploymentEvent struct {
	Event   string                      `json:"event"`
	Payload ModelDeploymentEventPayload `json:"payload"`
}

// ModelDeploymentEventPayload contains serving lifecycle data.
type ModelDeploymentEventPayload struct {
	URL             string      `json:"url,omitempty"`
	ErrorMessage    string      `json:"error_msg,omitempty"`
	Reason          string      `json:"reason,omitempty"`
	ResourceVersion json.Number `json:"resource_version"`
}

// modelDeploymentEvent translates a KServe object into a OneKS callback.
// An empty event means that Kubernetes has not reached a reportable state.
func modelDeploymentEvent(
	resource *unstructured.Unstructured,
	deleted bool,
) (int, ModelDeploymentEvent, error) {
	id, err := strconv.Atoi(resource.GetLabels()[modelDeploymentIDLabel])
	if err != nil || id < 0 {
		return 0, ModelDeploymentEvent{}, fmt.Errorf(
			"label %s must be a non-negative integer", modelDeploymentIDLabel,
		)
	}
	version := resource.GetResourceVersion()
	// Kubernetes changes resourceVersion on every persisted object update.
	if !decimalNumber(version) {
		return 0, ModelDeploymentEvent{}, fmt.Errorf(
			"resourceVersion must be a non-negative integer: %q", version,
		)
	}
	event := ModelDeploymentEvent{
		Payload: ModelDeploymentEventPayload{ResourceVersion: json.Number(version)},
	}
	if deleted {
		// A delete event carries the last object version seen by the informer.
		event.Event = "resources_deleted"
		return id, event, nil
	}

	// Retained conditions can describe an older spec or a terminating resource.
	if resource.GetDeletionTimestamp() != nil || !modelDeploymentStatusIsCurrent(resource) {
		return id, ModelDeploymentEvent{}, nil
	}

	ready := kserveReadyCondition(resource)
	if ready == nil {
		return id, ModelDeploymentEvent{}, nil
	}
	switch stringValue(ready["status"]) {
	case "True":
		// KServe publishes the routable endpoint after the service becomes ready.
		url, _, err := unstructured.NestedString(resource.Object, "status", "url")
		if err != nil || strings.TrimSpace(url) == "" {
			return id, ModelDeploymentEvent{}, err
		}
		event.Event = "inference_ready"
		event.Payload.URL = url
		return id, event, nil
	case "False":
		// Ready itself contains the terminal failure details.
	case "Unknown":
		// Ready=Unknown keeps the useful error on a component condition.
		ready = kserveTerminalFailureCondition(resource)
		if ready == nil {
			return id, ModelDeploymentEvent{}, nil
		}
	default:
		return id, ModelDeploymentEvent{}, nil
	}
	reason := strings.TrimSpace(stringValue(ready["reason"]))
	if !modelDeploymentFailureReason(reason) {
		return id, ModelDeploymentEvent{}, nil
	}
	event.Event = "deployment_failed"
	event.Payload.Reason = reason
	event.Payload.ErrorMessage = strings.TrimSpace(stringValue(ready["message"]))
	if event.Payload.ErrorMessage == "" {
		event.Payload.ErrorMessage = reason
	}
	return id, event, nil
}

// KServe reports both failure and error reasons. Keep transient NotReady
// reasons (such as WaitingForGateway) out of the failure notification path.
func modelDeploymentFailureReason(reason string) bool {
	return strings.HasSuffix(reason, "Failed") ||
		strings.HasSuffix(reason, "Error") ||
		reason == "ConfigNotFound" ||
		reason == "ScalingCRDNotFound"
}

func kserveReadyCondition(resource *unstructured.Unstructured) map[string]any {
	// Conditions are Kubernetes' standard representation of resource state.
	conditions, _, _ := unstructured.NestedSlice(resource.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && stringValue(condition["type"]) == "Ready" {
			return condition
		}
	}
	return nil
}

// kserveTerminalFailureCondition finds a terminal component error.
func kserveTerminalFailureCondition(resource *unstructured.Unstructured) map[string]any {
	conditions, _, _ := unstructured.NestedSlice(resource.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && stringValue(condition["status"]) == "False" &&
			modelDeploymentFailureReason(strings.TrimSpace(stringValue(condition["reason"]))) {
			return condition
		}
	}
	return nil
}

func modelDeploymentStatusIsCurrent(resource *unstructured.Unstructured) bool {
	// Matching generations ensure status describes the current spec.
	observed, found, err := unstructured.NestedInt64(resource.Object, "status", "observedGeneration")
	return err == nil && found && observed == resource.GetGeneration()
}

type modelDeploymentStatusSnapshot struct {
	uid           types.UID
	deploymentID  string
	generation    int64
	statusCurrent bool
	terminating   bool
	url           string
	status        string
	reason        string
	message       string
}

// modelDeploymentEventSignature tracks fields relevant to OneKS.
func modelDeploymentEventSignature(obj any) modelDeploymentStatusSnapshot {
	resource, ok := modelDeploymentFromEvent(obj)
	if !ok {
		return modelDeploymentStatusSnapshot{}
	}
	url, _, _ := unstructured.NestedString(resource.Object, "status", "url")
	ready := kserveReadyCondition(resource)
	return modelDeploymentStatusSnapshot{
		uid:           resource.GetUID(),
		deploymentID:  resource.GetLabels()[modelDeploymentIDLabel],
		generation:    resource.GetGeneration(),
		statusCurrent: modelDeploymentStatusIsCurrent(resource),
		terminating:   resource.GetDeletionTimestamp() != nil,
		url:           url,
		status:        stringValue(ready["status"]),
		reason:        stringValue(ready["reason"]),
		message:       stringValue(ready["message"]),
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
