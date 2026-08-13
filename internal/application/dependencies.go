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
	"bytes"
	"context"
	"fmt"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const dependencyNameIndex = "oneks.application.directDependencyName"

type DependencyConflictError struct {
	Name    string
	Message string
}

func (e *DependencyConflictError) Error() string {
	return fmt.Sprintf("dependency application %s/%s conflicts: %s", applicationv1.ApplicationNamespace, e.Name, e.Message)
}

type dependencyObservation struct {
	enabled   bool
	ready     bool
	completed int32
	current   string
	reason    string
	message   string
	failed    bool
	conflict  bool
}

func expectedDependencyApplication(root *applicationv1.OneKSApplication, plan applicationv1.DependencyPlan) *applicationv1.OneKSApplication {
	expected := &applicationv1.OneKSApplication{
		TypeMeta: metav1.TypeMeta{
			APIVersion: applicationv1.GroupVersion.String(),
			Kind:       "OneKSApplication",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: applicationv1.ApplicationNamespace,
			Name:      plan.Name,
			Finalizers: []string{
				applicationv1.ApplicationFinalizer,
			},
		},
		Spec: dependencyPlanChildSpec(root.Spec.ClusterID, plan),
	}
	expected.Labels = producerLabels(expected)
	return expected
}

func dependencyCompatibilityError(existing, expected *applicationv1.OneKSApplication, clusterID string) *DependencyConflictError {
	conflict := func(message string) *DependencyConflictError {
		return &DependencyConflictError{Name: expected.Name, Message: message}
	}
	if existing.Namespace != expected.Namespace || existing.Name != expected.Name {
		return conflict("metadata identity differs from the expected dependency")
	}
	if len(existing.OwnerReferences) != 0 {
		return conflict("shared dependencies must not have ownerReferences")
	}
	if existing.Spec.PlanDigest != expected.Spec.PlanDigest {
		return conflict("immutable spec differs from the expected dependency plan")
	}
	existingCanonical, err := CanonicalPlan(existing.Spec)
	if err != nil {
		return conflict(fmt.Sprintf("current spec cannot be canonicalized: %v", err))
	}
	expectedCanonical, err := CanonicalPlan(expected.Spec)
	if err != nil {
		return conflict(fmt.Sprintf("expected dependency spec cannot be canonicalized: %v", err))
	}
	if !bytes.Equal(existingCanonical, expectedCanonical) {
		return conflict("immutable spec differs from the expected dependency plan")
	}
	if !producerLabelsMatch(existing) {
		return conflict("producer identity labels do not match the expected dependency")
	}
	if validationError := ValidatePlan(existing, ValidationConfig{ClusterID: clusterID}); validationError != nil {
		return conflict(fmt.Sprintf("current plan is invalid: %s", validationError.Reason))
	}
	return nil
}

// materializeRootDependencies performs a complete compatibility scan before it
// creates any missing application. raced is true when Create reported
// AlreadyExists; the caller must requeue so the next scan verifies that object.
func (r *Reconciler) materializeRootDependencies(ctx context.Context, root *applicationv1.OneKSApplication) (raced bool, terminating string, conflict *DependencyConflictError, err error) {
	missing := make([]*applicationv1.OneKSApplication, 0, len(root.Spec.DependencyPlans))
	reader := r.authoritativeReader()
	for _, plan := range root.Spec.DependencyPlans {
		expected := expectedDependencyApplication(root, plan)
		existing := &applicationv1.OneKSApplication{}
		getErr := reader.Get(ctx, client.ObjectKeyFromObject(expected), existing)
		if apierrors.IsNotFound(getErr) {
			missing = append(missing, expected)
			continue
		}
		if getErr != nil {
			return false, "", nil, fmt.Errorf("preflight dependency %s/%s: %w", expected.Namespace, expected.Name, getErr)
		}
		if !existing.DeletionTimestamp.IsZero() {
			return false, existing.Name, nil, nil
		}
		if compatibilityError := dependencyCompatibilityError(existing, expected, root.Spec.ClusterID); compatibilityError != nil {
			return false, "", compatibilityError, nil
		}
	}

	for _, expected := range missing {
		if createErr := r.Create(ctx, expected); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				return true, "", nil, nil
			}
			return false, "", nil, fmt.Errorf("create dependency application %s/%s: %w", expected.Namespace, expected.Name, createErr)
		}
		r.event(root, corev1.EventTypeNormal, "DependencyCreated", fmt.Sprintf("Dependency application %s/%s created", expected.Namespace, expected.Name))
	}
	return false, "", nil, nil
}

func (r *Reconciler) observeDependencies(ctx context.Context, app *applicationv1.OneKSApplication) (dependencyObservation, error) {
	result := dependencyObservation{ready: true}
	if app.Spec.PlanVersion != applicationv1.PlanVersionV1Alpha2 && app.Spec.PlanVersion != applicationv1.PlanVersionV1Alpha3 {
		return result, nil
	}
	result.enabled = true
	if len(app.Spec.Dependencies) == 0 {
		result.reason = "NoDependencies"
		result.message = "Application has no direct dependencies"
		return result, nil
	}

	for _, reference := range app.Spec.Dependencies {
		dependency := &applicationv1.OneKSApplication{}
		err := r.Get(ctx, types.NamespacedName{Namespace: applicationv1.ApplicationNamespace, Name: reference.Name}, dependency)
		if apierrors.IsNotFound(err) {
			result.ready = false
			if result.reason == "" {
				result.current = reference.Name
				result.reason = "DependencyMissing"
				result.message = fmt.Sprintf("Direct dependency %s is missing", reference.Name)
			}
			continue
		}
		if err != nil {
			return result, fmt.Errorf("observe dependency %s/%s: %w", applicationv1.ApplicationNamespace, reference.Name, err)
		}
		if !dependency.DeletionTimestamp.IsZero() {
			result.ready = false
			if result.reason == "" {
				result.current = reference.Name
				result.reason = "DependencyTerminating"
				result.message = fmt.Sprintf("Direct dependency %s is terminating", reference.Name)
			}
			continue
		}
		if conflict := dependencyReferenceConflict(dependency, reference, app.Spec.ClusterID); conflict != nil {
			result.ready = false
			result.conflict = true
			result.current = reference.Name
			result.reason = "DependencyConflict"
			result.message = conflict.Error()
			return result, nil
		}
		if dependencyStatusReady(dependency) {
			result.completed++
			continue
		}

		result.ready = false
		if dependencyStatusFailed(dependency) {
			if !result.failed {
				failureReason := "Failed"
				if dependency.Status.LastError != nil && dependency.Status.LastError.Reason != "" {
					failureReason = dependency.Status.LastError.Reason
				}
				result.failed = true
				result.current = reference.Name
				result.reason = "DependencyFailed"
				result.message = fmt.Sprintf("Direct dependency %s reported failure (%s)", reference.Name, failureReason)
			}
			continue
		}
		if result.reason != "" {
			continue
		}
		result.current = reference.Name
		switch dependency.Status.Phase {
		case applicationv1.PhaseInstalling:
			result.reason = "DependencyInstalling"
			result.message = fmt.Sprintf("Direct dependency %s is installing", reference.Name)
		case applicationv1.PhaseObserving:
			result.reason = "DependencyObserving"
			result.message = fmt.Sprintf("Direct dependency %s is observing", reference.Name)
		default:
			result.reason = "DependencyPending"
			result.message = fmt.Sprintf("Direct dependency %s is not ready", reference.Name)
		}
	}
	if result.ready {
		result.reason = "DependenciesReady"
		result.message = "All direct dependencies are ready"
	}
	return result, nil
}

func dependencyReferenceConflict(dependency *applicationv1.OneKSApplication, reference applicationv1.DependencyReference, clusterID string) *DependencyConflictError {
	conflict := func(message string) *DependencyConflictError {
		return &DependencyConflictError{Name: reference.Name, Message: message}
	}
	if dependency.Namespace != applicationv1.ApplicationNamespace || dependency.Name != reference.Name {
		return conflict("metadata identity differs from the dependency reference")
	}
	if len(dependency.OwnerReferences) != 0 {
		return conflict("shared dependencies must not have ownerReferences")
	}
	if dependency.Spec.PlanVersion != applicationv1.PlanVersionV1Alpha2 || dependency.Spec.Role != applicationv1.ApplicationRoleDependency {
		return conflict("referenced application is not a plan-v1alpha2 Dependency")
	}
	if dependency.Spec.CatalogueChartID != reference.CatalogueChartID || dependency.Spec.PlanDigest != reference.PlanDigest {
		return conflict("catalogueChartID or planDigest differs from the dependency reference")
	}
	if !producerLabelsMatch(dependency) {
		return conflict("producer identity labels do not match the dependency reference")
	}
	if validationError := ValidatePlan(dependency, ValidationConfig{ClusterID: clusterID}); validationError != nil {
		return conflict(fmt.Sprintf("referenced plan is invalid: %s", validationError.Reason))
	}
	return nil
}

func dependencyStatusReady(dependency *applicationv1.OneKSApplication) bool {
	if !dependency.DeletionTimestamp.IsZero() || !dependencyStatusIsCurrent(dependency) || dependency.Status.Phase != applicationv1.PhaseReady {
		return false
	}
	condition := meta.FindStatusCondition(dependency.Status.Conditions, ConditionReady)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == dependency.Generation
}

func dependencyStatusFailed(dependency *applicationv1.OneKSApplication) bool {
	return dependencyStatusIsCurrent(dependency) && dependency.Status.Phase == applicationv1.PhaseFailed
}

func dependencyStatusIsCurrent(dependency *applicationv1.OneKSApplication) bool {
	return dependency.Status.ObservedGeneration == dependency.Generation &&
		dependency.Status.ObservedPlanDigest == dependency.Spec.PlanDigest
}

func (r *Reconciler) releaseDependencies(ctx context.Context, app *applicationv1.OneKSApplication) (retry bool, err error) {
	if len(app.Spec.Dependencies) == 0 {
		return false, nil
	}

	applications := &applicationv1.OneKSApplicationList{}
	if err := r.authoritativeReader().List(ctx, applications, client.InNamespace(applicationv1.ApplicationNamespace)); err != nil {
		return false, fmt.Errorf("list dependency consumers: %w", err)
	}

	for _, reference := range app.Spec.Dependencies {
		if hasLiveDependencyConsumer(applications.Items, reference.Name) {
			continue
		}
		retry, err := r.releaseDependency(ctx, app, reference)
		if err != nil {
			return false, err
		}
		if retry {
			return true, nil
		}
	}
	return false, nil
}

func hasLiveDependencyConsumer(applications []applicationv1.OneKSApplication, dependencyName string) bool {
	for index := range applications {
		consumer := &applications[index]
		if consumer.Spec.ExecutionMode != applicationv1.ExecutionModeExecute || !consumer.DeletionTimestamp.IsZero() {
			continue
		}
		for _, reference := range consumer.Spec.Dependencies {
			if reference.Name == dependencyName {
				return true
			}
		}
	}
	return false
}

func (r *Reconciler) releaseDependency(ctx context.Context, consumer *applicationv1.OneKSApplication, reference applicationv1.DependencyReference) (retry bool, err error) {
	dependency := &applicationv1.OneKSApplication{}
	key := types.NamespacedName{Namespace: applicationv1.ApplicationNamespace, Name: reference.Name}
	if err := r.authoritativeReader().Get(ctx, key, dependency); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get dependency %s/%s for garbage collection: %w", key.Namespace, key.Name, err)
	}
	if !dependency.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if conflict := dependencyReferenceConflict(dependency, reference, consumer.Spec.ClusterID); conflict != nil {
		r.event(consumer, corev1.EventTypeWarning, "DependencyGCConflict", conflict.Error())
		return false, nil
	}
	if dependency.Spec.DeletionPolicy == applicationv1.DeletionPolicyRetain {
		r.event(consumer, corev1.EventTypeNormal, "DependencyRetained", fmt.Sprintf("Dependency application %s/%s retained by policy", dependency.Namespace, dependency.Name))
		return false, nil
	}
	if !containsString(dependency.Finalizers, applicationv1.ApplicationFinalizer) {
		updated := dependency.DeepCopy()
		updated.Finalizers = append(updated.Finalizers, applicationv1.ApplicationFinalizer)
		if err := r.Update(ctx, updated); err != nil {
			return false, fmt.Errorf("add dependency cleanup finalizer to %s/%s: %w", dependency.Namespace, dependency.Name, err)
		}
		r.event(consumer, corev1.EventTypeNormal, "DependencyFinalizerAdded", fmt.Sprintf("Dependency application %s/%s cleanup finalizer added", dependency.Namespace, dependency.Name))
		return true, nil
	}
	if err := r.Delete(ctx, dependency, deletePreconditions(dependency)...); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete dependency application %s/%s: %w", dependency.Namespace, dependency.Name, err)
	}
	r.event(consumer, corev1.EventTypeNormal, "DependencyDeleted", fmt.Sprintf("Dependency application %s/%s deletion requested", dependency.Namespace, dependency.Name))
	return false, nil
}

func directDependencyNames(object client.Object) []string {
	app, ok := object.(*applicationv1.OneKSApplication)
	if !ok || len(app.Spec.Dependencies) == 0 {
		return nil
	}
	names := make([]string, 0, len(app.Spec.Dependencies))
	seen := make(map[string]struct{}, len(app.Spec.Dependencies))
	for _, dependency := range app.Spec.Dependencies {
		if _, exists := seen[dependency.Name]; exists {
			continue
		}
		seen[dependency.Name] = struct{}{}
		names = append(names, dependency.Name)
	}
	return names
}
