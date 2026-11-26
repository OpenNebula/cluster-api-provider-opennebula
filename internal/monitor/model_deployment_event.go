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
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const (
	modelDeploymentFailureGracePeriod = 15 * time.Minute
	modelLoadFailedReason             = "ModelLoadFailed"
	kubernetesConditionFalse          = "False"
	kubernetesConditionUnknown        = "Unknown"
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
	case kubernetesConditionFalse, kubernetesConditionUnknown:
		// Ready=Unknown may keep the useful error on a component condition.
		ready = modelDeploymentFailureCondition(resource)
		if ready == nil {
			return id, ModelDeploymentEvent{}, nil
		}
	default:
		return id, ModelDeploymentEvent{}, nil
	}
	reason := strings.TrimSpace(stringValue(ready["reason"]))
	if modelDeploymentFailureNeedsGrace(reason) &&
		modelDeploymentFailureRetryAfter(resource, ready, time.Now()) > 0 {
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

func modelDeploymentFailureCondition(resource *unstructured.Unstructured) map[string]any {
	ready := kserveReadyCondition(resource)
	switch stringValue(ready["status"]) {
	case kubernetesConditionFalse:
		if modelDeploymentFailureReason(strings.TrimSpace(stringValue(ready["reason"]))) {
			return ready
		}
	case kubernetesConditionUnknown:
		return kserveTerminalFailureCondition(resource)
	}
	return nil
}

// Keep this list explicit. KServe uses many Error/Failed reasons for
// recoverable reconciliation attempts, so suffix matching creates false
// deployment failures.
func modelDeploymentFailureReason(reason string) bool {
	if modelDeploymentFailureNeedsGrace(reason) {
		return true
	}
	switch reason {
	case "CombineBaseError", "ConfigNotFound", modelLoadFailedReason,
		"ProgressDeadlineExceeded", "ScalingCRDNotFound":
		return true
	default:
		return false
	}
}

func modelDeploymentFailureNeedsGrace(reason string) bool {
	switch reason {
	case "FailedGetExternalMetric", "HTTPRouteFetchError", "InferencePoolFetchError",
		"MinimumReplicasUnavailable", "PlatformNetworkingReconcileError",
		"ReconcileCertsError", "ReconcileSingleNodeWorkloadError",
		"ReconcileWorkloadServiceError", "RefsInvalid", "SchedulerReconcileError":
		return true
	default:
		return false
	}
}

func modelDeploymentFailureRetryAfter(
	resource *unstructured.Unstructured,
	condition map[string]any,
	now time.Time,
) time.Duration {
	reason := strings.TrimSpace(stringValue(condition["reason"]))
	if !modelDeploymentFailureNeedsGrace(reason) {
		return 0
	}
	transition, err := time.Parse(time.RFC3339, stringValue(condition["lastTransitionTime"]))
	if err != nil {
		transition = resource.GetCreationTimestamp().Time
	}
	if transition.IsZero() {
		return 0
	}
	remaining := transition.Add(modelDeploymentFailureGracePeriod).Sub(now)
	if remaining > 0 {
		return remaining
	}
	return 0
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

// kserveTerminalFailureCondition only examines workload rollup leaves and the
// preset condition. Other conditions may be false without preventing serving.
func kserveTerminalFailureCondition(resource *unstructured.Unstructured) map[string]any {
	conditions, _, _ := unstructured.NestedSlice(resource.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && modelDeploymentFailureConditionType(stringValue(condition["type"])) &&
			stringValue(condition["status"]) == kubernetesConditionFalse &&
			modelDeploymentFailureReason(strings.TrimSpace(stringValue(condition["reason"]))) {
			return condition
		}
	}
	return nil
}

func modelDeploymentFailureConditionType(conditionType string) bool {
	return conditionType == "PredictorReady" || conditionType == "PresetsCombined" ||
		strings.HasSuffix(conditionType, "WorkloadReady")
}

func modelDeploymentStatusIsCurrent(resource *unstructured.Unstructured) bool {
	// Matching generations ensure status describes the current spec.
	observed, found, err := unstructured.NestedInt64(resource.Object, "status", "observedGeneration")
	return err == nil && found && observed == resource.GetGeneration()
}

type modelDeploymentStatusSnapshot struct {
	uid            types.UID
	deploymentID   string
	generation     int64
	statusCurrent  bool
	terminating    bool
	url            string
	status         string
	reason         string
	message        string
	transitionTime string
}

// modelDeploymentEventSignature tracks fields relevant to OneKS.
func modelDeploymentEventSignature(obj any) modelDeploymentStatusSnapshot {
	resource, ok := modelDeploymentFromEvent(obj)
	if !ok {
		return modelDeploymentStatusSnapshot{}
	}
	url, _, _ := unstructured.NestedString(resource.Object, "status", "url")
	condition := kserveReadyCondition(resource)
	// Keep this selection aligned with modelDeploymentEvent: when Ready remains
	// Unknown, a terminal component transition is what changes the callback.
	if stringValue(condition["status"]) == kubernetesConditionUnknown {
		if failure := modelDeploymentFailureCondition(resource); failure != nil {
			condition = failure
		}
	}
	return modelDeploymentStatusSnapshot{
		uid:            resource.GetUID(),
		deploymentID:   resource.GetLabels()[modelDeploymentIDLabel],
		generation:     resource.GetGeneration(),
		statusCurrent:  modelDeploymentStatusIsCurrent(resource),
		terminating:    resource.GetDeletionTimestamp() != nil,
		url:            url,
		status:         stringValue(condition["status"]),
		reason:         stringValue(condition["reason"]),
		message:        stringValue(condition["message"]),
		transitionTime: stringValue(condition["lastTransitionTime"]),
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
