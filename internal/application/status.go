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
	"context"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
)

type observation struct {
	resources    []applicationv1.ResourceStatus
	managed      componentObservation
	protected    componentObservation
	helm         *unstructured.Unstructured
	helmState    componentObservation
	current      string
	completed    int32
	allResources bool
}

type componentObservation struct {
	ready   bool
	failed  bool
	reason  string
	message string
}

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

type failureKind uint8

const (
	failureInvalidPlan failureKind = iota
	failureExecution
	failureOwnership
)

func baseStatus(app *applicationv1.OneKSApplication) applicationv1.OneKSApplicationStatus {
	status := app.Status.DeepCopy()
	status.ObservedGeneration = app.Generation
	status.LastError = nil
	return *status
}

func applicationProgressTotal(app *applicationv1.OneKSApplication) int32 {
	total := 1 + len(app.Spec.Dependencies) + len(app.Spec.ManagedResources)
	if app.Spec.Role == applicationv1.ApplicationRoleRoot {
		total += len(app.Spec.ProtectedSecrets)
	}
	return int32(total)
}

func setCondition(status *applicationv1.OneKSApplicationStatus, generation int64, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: conditionStatus, ObservedGeneration: generation,
		Reason: truncate(reason, 128), Message: truncate(message, 512),
	})
}

func setReadyCondition(status *applicationv1.OneKSApplicationStatus, generation int64, conditionType string, ready bool, readyReason, readyMessage, pendingReason, pendingMessage string) {
	condition := conditionStatus(ready)
	setCondition(status, generation, conditionType, condition,
		conditionText(condition, readyReason, pendingReason), conditionText(condition, readyMessage, pendingMessage))
}

func setLastError(status *applicationv1.OneKSApplicationStatus, reason, message string) {
	status.LastError = &applicationv1.ApplicationError{Reason: truncate(reason, 128), Message: truncate(message, 512)}
}

func normalizeStatus(status *applicationv1.OneKSApplicationStatus) {
	status.SecretInputUID, status.Progress.Current = truncate(status.SecretInputUID, 128), truncate(status.Progress.Current, 128)
	if len(status.Conditions) > maxStatusConditions {
		status.Conditions = status.Conditions[:maxStatusConditions]
	}
	for index := range status.Conditions {
		condition := &status.Conditions[index]
		condition.Type, condition.Reason, condition.Message = truncate(condition.Type, 128), truncate(condition.Reason, 128), truncate(condition.Message, 512)
	}
	if status.HelmChartRef != nil {
		ref := status.HelmChartRef
		ref.Namespace, ref.Name = truncate(ref.Namespace, 63), truncate(ref.Name, 253)
		ref.UID, ref.ResourceVersion = truncate(ref.UID, 128), truncate(ref.ResourceVersion, 64)
	}
	if len(status.Resources) > maxStatusResources {
		status.Resources = status.Resources[:maxStatusResources]
	}
	for index := range status.Resources {
		resource := &status.Resources[index]
		resource.ID, resource.Phase = truncate(resource.ID, 63), truncate(resource.Phase, 32)
		resource.Reason, resource.Message = truncate(resource.Reason, 128), truncate(resource.Message, 512)
		resource.ResourceVersion = truncate(resource.ResourceVersion, 64)
	}
	if status.LastError != nil {
		status.LastError.Reason, status.LastError.Message = truncate(status.LastError.Reason, 128), truncate(status.LastError.Message, 512)
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

func (r *Reconciler) reconcileStatus(ctx context.Context, app *applicationv1.OneKSApplication, dependencies dependencyObservation) (ctrl.Result, error) {
	managedReadinessEnabled := dependencies.ready
	observed, err := r.observe(ctx, app, managedReadinessEnabled)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.recordObservedStatus(ctx, app, dependencies, observed)
}

func (r *Reconciler) recordObservedStatus(ctx context.Context, app *applicationv1.OneKSApplication, dependencies dependencyObservation, observed observation) (ctrl.Result, error) {
	status := baseStatus(app)
	status.Resources = observed.resources
	status.Progress = applicationv1.ApplicationProgress{
		Completed: observed.completed + dependencies.completed, Total: applicationProgressTotal(app), Current: observed.current,
	}
	if !dependencies.ready && dependencies.current != "" {
		status.Progress.Current = dependencies.current
	}
	status.HelmChartRef = nil
	if observed.helm != nil {
		status.HelmChartRef = &applicationv1.HelmChartReference{
			Namespace: observed.helm.GetNamespace(), Name: observed.helm.GetName(),
			UID: string(observed.helm.GetUID()), ResourceVersion: observed.helm.GetResourceVersion(),
		}
	}
	setCondition(&status, app.Generation, ConditionPlanValid, metav1.ConditionTrue, "Validated", "Plan schema is valid")
	setCondition(&status, app.Generation, ConditionDependenciesReady, conditionStatus(dependencies.ready), dependencies.reason, dependencies.message)
	resourcesReady := observed.managed.ready
	resourceCondition := conditionStatus(resourcesReady)
	resourceReason := conditionText(resourceCondition, "ResourcesReady", "ResourcesPending")
	resourceMessage := conditionText(resourceCondition, "All managed resources are ready", "Managed resources are not ready")
	if observed.managed.failed {
		resourceReason = observed.managed.reason
		resourceMessage = observed.managed.message
	} else if !dependencies.ready {
		resourceCondition = metav1.ConditionUnknown
		resourceReason = "DependenciesPending"
		resourceMessage = "Managed resources are gated by direct dependencies"
	}
	setCondition(&status, app.Generation, ConditionResourcesReady, resourceCondition, resourceReason, resourceMessage)
	if usesProtectedSecrets(app) {
		setReadyCondition(&status, app.Generation, ConditionProtectedSecretsReady, observed.protected.ready,
			"ProtectedSecretsReady", "All protected Secrets are ready", observed.protected.reason, observed.protected.message)
	}
	helmReason, helmMessage := "HelmReleaseReady", "Helm release is ready"
	if observed.helmState.ready && observed.helmState.reason == "ExternalDependencyReady" {
		helmReason = observed.helmState.reason
		helmMessage = observed.helmState.message
	}
	setReadyCondition(&status, app.Generation, ConditionHelmReleaseReady, observed.helmState.ready,
		helmReason, helmMessage, observed.helmState.reason, observed.helmState.message)
	setCondition(&status, app.Generation, ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflict", "Managed children have exact OneKS ownership")

	ready := dependencies.ready && observed.allResources && observed.helmState.ready
	readyReason := "ApplicationProgressing"
	readyMessage := "Application installation is in progress"
	failed := observed.firstFailure()
	if failed.failed {
		readyReason = failed.reason
		readyMessage = failed.message
	} else if !dependencies.ready {
		readyReason = dependencies.reason
		readyMessage = dependencies.message
	}
	setReadyCondition(&status, app.Generation, ConditionReady, ready,
		"ApplicationReady", "Application resources and Helm release are ready", readyReason, readyMessage)
	if dependencies.terminal {
		status.Phase = applicationv1.PhaseFailed
		setLastError(&status, dependencies.reason, dependencies.message)
	} else if failed.failed {
		status.Phase = applicationv1.PhaseFailed
		setLastError(&status, failed.reason, failed.message)
	} else if ready {
		status.Phase = applicationv1.PhaseReady
	} else {
		status.Phase = applicationv1.PhaseInstalling
	}
	if err := r.updateStatus(ctx, app, status); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
}

func (observed observation) firstFailure() componentObservation {
	for _, component := range []componentObservation{observed.managed, observed.protected, observed.helmState} {
		if component.failed {
			return component
		}
	}
	return componentObservation{}
}

func (r *Reconciler) observe(ctx context.Context, app *applicationv1.OneKSApplication, managedReadinessEnabled bool) (observation, error) {
	result, err := r.observeManagedResources(ctx, app, managedReadinessEnabled)
	if err != nil {
		return result, err
	}
	result, err = r.observeProtected(ctx, app, result)
	if err != nil {
		return result, err
	}
	return r.observeHelm(ctx, app, result)
}

func (r *Reconciler) observeProtected(ctx context.Context, app *applicationv1.OneKSApplication, result observation) (observation, error) {
	result.managed.ready = result.allResources
	if !usesProtectedSecrets(app) {
		return result, nil
	}
	protected, err := r.observeProtectedSecrets(ctx, app, result.managed.ready)
	if err != nil {
		return result, err
	}
	result.resources = append(result.resources, protected.statuses...)
	result.completed += protected.completed
	result.protected = protected.componentObservation
	result.allResources = result.managed.ready && protected.ready
	if result.managed.ready && !protected.ready && protected.current != "" {
		result.current = protected.current
	}
	return result, nil
}

func (r *Reconciler) recordFailure(ctx context.Context, app *applicationv1.OneKSApplication, reason, message string, failure failureKind) (ctrl.Result, error) {
	status := baseStatus(app)
	status.Phase = applicationv1.PhaseFailed
	status.Progress = applicationv1.ApplicationProgress{Total: applicationProgressTotal(app)}
	setLastError(&status, reason, message)
	planCondition := metav1.ConditionTrue
	if failure == failureInvalidPlan {
		planCondition = metav1.ConditionFalse
	}
	setCondition(&status, app.Generation, ConditionPlanValid, planCondition, reason, message)
	if len(app.Spec.Dependencies) == 0 {
		setCondition(&status, app.Generation, ConditionDependenciesReady, metav1.ConditionTrue, "NoDependencies", "Application has no direct dependencies")
	} else {
		setCondition(&status, app.Generation, ConditionDependenciesReady, metav1.ConditionFalse, "DependenciesPending", "Direct dependencies have not been evaluated")
	}
	if usesProtectedSecrets(app) {
		setCondition(&status, app.Generation, ConditionProtectedSecretsReady, metav1.ConditionFalse, reason, message)
	}
	conflictCondition := conditionStatus(failure == failureOwnership)
	setCondition(&status, app.Generation, ConditionOwnershipConflict, conflictCondition, reason, message)
	setCondition(&status, app.Generation, ConditionReady, metav1.ConditionFalse, reason, message)
	if err := r.updateStatus(ctx, app, status); err != nil {
		return ctrl.Result{}, err
	}
	r.event(app, corev1.EventTypeWarning, reason, truncate(message, 512))
	return ctrl.Result{}, nil
}

func (r *Reconciler) updateStatus(ctx context.Context, app *applicationv1.OneKSApplication, status applicationv1.OneKSApplicationStatus) error {
	normalizeStatus(&status)
	if reflect.DeepEqual(app.Status, status) {
		return nil
	}
	updated := app.DeepCopy()
	updated.Status = status
	if err := r.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("update OneKSApplication status: %w", err)
	}
	app.Status = status
	app.ResourceVersion = updated.ResourceVersion
	ctrl.LoggerFrom(ctx).Info(
		"application status updated",
		"phase", status.Phase,
		"observedGeneration", status.ObservedGeneration,
		"completed", status.Progress.Completed,
		"total", status.Progress.Total,
		"current", status.Progress.Current,
	)
	return nil
}

func conditionText(status metav1.ConditionStatus, positive, negative string) string {
	if status == metav1.ConditionTrue {
		return positive
	}
	return firstNonEmpty(negative, "Pending")
}

func conditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}
