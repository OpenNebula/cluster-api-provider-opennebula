/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package application

import (
	"unicode/utf8"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionPlanValid             = "PlanValid"
	ConditionDependenciesReady     = "DependenciesReady"
	ConditionResourcesReady        = "ResourcesReady"
	ConditionProtectedSecretsReady = "ProtectedSecretsReady"
	ConditionHelmReleaseReady      = "HelmReleaseReady"
	ConditionReady                 = "Ready"
	ConditionOwnershipConflict     = "OwnershipConflict"
	maxStatusConditions            = 8
	maxStatusResources             = 16
)

func baseStatus(app *applicationv1.OneKSApplication) applicationv1.OneKSApplicationStatus {
	status := app.Status.DeepCopy()
	if status == nil {
		status = &applicationv1.OneKSApplicationStatus{}
	}
	status.ObservedGeneration = app.Generation
	status.ObservedPlanDigest = app.Spec.PlanDigest
	status.SupportedPlanVersions = []string{
		applicationv1.PlanVersion,
	}
	status.LastError = nil
	return *status
}

func applicationProgressTotal(app *applicationv1.OneKSApplication) int32 {
	total := 1 + len(app.Spec.Dependencies)
	if app.Spec.Role == applicationv1.ApplicationRoleRoot {
		total += len(app.Spec.ManagedResources) + len(app.Spec.ProtectedSecrets)
	}
	return int32(total)
}

func setCondition(status *applicationv1.OneKSApplicationStatus, generation int64, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: conditionStatus, ObservedGeneration: generation,
		Reason: truncate(reason, 128), Message: truncate(message, 512),
	})
}

func setLastError(status *applicationv1.OneKSApplicationStatus, reason, message string) {
	status.LastError = &applicationv1.ApplicationError{
		Reason: truncate(reason, 128), Message: truncate(message, 512),
	}
}

func normalizeStatus(status *applicationv1.OneKSApplicationStatus) {
	status.ObservedPlanDigest = truncate(status.ObservedPlanDigest, 50)
	status.SecretInputUID = truncate(status.SecretInputUID, 128)
	status.Progress.Current = truncate(status.Progress.Current, 128)
	if len(status.SupportedPlanVersions) > 8 {
		status.SupportedPlanVersions = status.SupportedPlanVersions[:8]
	}
	for index := range status.SupportedPlanVersions {
		status.SupportedPlanVersions[index] = truncate(status.SupportedPlanVersions[index], 128)
	}
	if len(status.Conditions) > maxStatusConditions {
		status.Conditions = status.Conditions[:maxStatusConditions]
	}
	for index := range status.Conditions {
		condition := &status.Conditions[index]
		condition.Type = truncate(condition.Type, 128)
		condition.Reason = truncate(condition.Reason, 128)
		condition.Message = truncate(condition.Message, 512)
	}
	if status.HelmChartRef != nil {
		status.HelmChartRef.Namespace = truncate(status.HelmChartRef.Namespace, 63)
		status.HelmChartRef.Name = truncate(status.HelmChartRef.Name, 253)
		status.HelmChartRef.UID = truncate(status.HelmChartRef.UID, 128)
		status.HelmChartRef.ResourceVersion = truncate(status.HelmChartRef.ResourceVersion, 64)
	}
	if len(status.Resources) > maxStatusResources {
		status.Resources = status.Resources[:maxStatusResources]
	}
	for index := range status.Resources {
		resource := &status.Resources[index]
		resource.ID = truncate(resource.ID, 63)
		resource.Phase = truncate(resource.Phase, 32)
		resource.Reason = truncate(resource.Reason, 128)
		resource.Message = truncate(resource.Message, 512)
		resource.ResourceVersion = truncate(resource.ResourceVersion, 64)
	}
	if status.LastError != nil {
		status.LastError.Reason = truncate(status.LastError.Reason, 128)
		status.LastError.Message = truncate(status.LastError.Message, 512)
	}
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
