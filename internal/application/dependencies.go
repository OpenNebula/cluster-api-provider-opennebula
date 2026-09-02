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

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
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
	ready     bool
	completed int32
	current   string
	reason    string
	message   string
	terminal  bool
}

func (result *dependencyObservation) markPending(name, reason, message string) {
	result.ready = false
	if result.reason != "" {
		return
	}
	result.current = name
	result.reason = reason
	result.message = message
}

func (result *dependencyObservation) markTerminal(name, reason, message string) {
	result.ready = false
	result.terminal = true
	result.current = name
	result.reason = reason
	result.message = message
}

type dependencyMaterialization struct {
	raced       bool
	terminating string
	conflict    *DependencyConflictError
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

func dependencyIdentityError(existing, expected *applicationv1.OneKSApplication, clusterID string) *DependencyConflictError {
	if conflict := validateDependencyApplication(existing, expected.Name, clusterID); conflict != nil {
		return conflict
	}
	if existing.Spec.PlanDigest != expected.Spec.PlanDigest {
		return dependencyConflict(expected.Name, "immutable spec differs from the expected dependency plan")
	}
	existingCanonical, err := CanonicalPlan(existing.Spec)
	if err != nil {
		return dependencyConflict(expected.Name, fmt.Sprintf("current spec cannot be canonicalized: %v", err))
	}
	expectedCanonical, err := CanonicalPlan(expected.Spec)
	if err != nil {
		return dependencyConflict(expected.Name, fmt.Sprintf("expected dependency spec cannot be canonicalized: %v", err))
	}
	if !bytes.Equal(existingCanonical, expectedCanonical) {
		return dependencyConflict(expected.Name, "immutable spec differs from the expected dependency plan")
	}
	return nil
}

func validateDependencyApplication(dependency *applicationv1.OneKSApplication, expectedName, clusterID string) *DependencyConflictError {
	if dependency.Namespace != applicationv1.ApplicationNamespace || dependency.Name != expectedName {
		return dependencyConflict(expectedName, "metadata identity differs from the expected dependency")
	}
	if validationError := ValidatePlan(dependency, ValidationConfig{ClusterID: clusterID}); validationError != nil {
		return dependencyConflict(expectedName, fmt.Sprintf("dependency plan is invalid: %s", validationError.Reason))
	}
	return nil
}

func dependencyConflict(name, message string) *DependencyConflictError {
	return &DependencyConflictError{Name: name, Message: message}
}

// materializeRootDependencies performs a complete identity scan before it
// creates any missing application. A create race requires another complete
// scan so the object that won the race is verified before execution continues.
func (r *Reconciler) materializeRootDependencies(ctx context.Context, root *applicationv1.OneKSApplication) (dependencyMaterialization, error) {
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
			return dependencyMaterialization{}, fmt.Errorf("preflight dependency %s/%s: %w", expected.Namespace, expected.Name, getErr)
		}
		if !existing.DeletionTimestamp.IsZero() {
			return dependencyMaterialization{terminating: existing.Name}, nil
		}
		if identityError := dependencyIdentityError(existing, expected, root.Spec.ClusterID); identityError != nil {
			return dependencyMaterialization{conflict: identityError}, nil
		}
	}

	for _, expected := range missing {
		ctrl.LoggerFrom(ctx).V(1).Info(
			"reconciling dependency",
			"action", "create", "dependency", expected.Name,
		)
		if createErr := r.Create(ctx, expected); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				return dependencyMaterialization{raced: true}, nil
			}
			return dependencyMaterialization{}, fmt.Errorf("create dependency application %s/%s: %w", expected.Namespace, expected.Name, createErr)
		}
		ctrl.LoggerFrom(ctx).Info("dependency created", "dependency", expected.Name)
		r.event(root, corev1.EventTypeNormal, "DependencyCreated", fmt.Sprintf("Dependency application %s/%s created", expected.Namespace, expected.Name))
	}
	return dependencyMaterialization{}, nil
}

func (r *Reconciler) observeDependencies(ctx context.Context, app *applicationv1.OneKSApplication) (dependencyObservation, error) {
	result := dependencyObservation{ready: true}
	if len(app.Spec.Dependencies) == 0 {
		result.reason = "NoDependencies"
		result.message = "Application has no direct dependencies"
		return result, nil
	}

	for _, reference := range app.Spec.Dependencies {
		dependency := &applicationv1.OneKSApplication{}
		err := r.Get(ctx, types.NamespacedName{Namespace: applicationv1.ApplicationNamespace, Name: reference.Name}, dependency)
		if apierrors.IsNotFound(err) {
			result.markPending(reference.Name, "DependencyMissing", fmt.Sprintf("Direct dependency %s is missing", reference.Name))
			continue
		}
		if err != nil {
			return result, fmt.Errorf("observe dependency %s/%s: %w", applicationv1.ApplicationNamespace, reference.Name, err)
		}
		if !dependency.DeletionTimestamp.IsZero() {
			result.markPending(reference.Name, "DependencyTerminating", fmt.Sprintf("Direct dependency %s is terminating", reference.Name))
			continue
		}
		if conflict := dependencyReferenceConflict(dependency, reference, app.Spec.ClusterID); conflict != nil {
			result.markTerminal(reference.Name, "DependencyConflict", conflict.Error())
			return result, nil
		}
		if dependencyStatusReady(dependency) {
			result.completed++
			continue
		}

		if dependencyStatusIsCurrent(dependency) && dependency.Status.Phase == applicationv1.PhaseFailed {
			if !result.terminal {
				failureReason := "Failed"
				if dependency.Status.LastError != nil && dependency.Status.LastError.Reason != "" {
					failureReason = dependency.Status.LastError.Reason
				}
				result.markTerminal(
					reference.Name, "DependencyFailed",
					fmt.Sprintf("Direct dependency %s reported failure (%s)", reference.Name, failureReason),
				)
			}
			continue
		}
		switch dependency.Status.Phase {
		case applicationv1.PhaseInstalling:
			result.markPending(reference.Name, "DependencyInstalling", fmt.Sprintf("Direct dependency %s is installing", reference.Name))
		case applicationv1.PhaseObserving:
			result.markPending(reference.Name, "DependencyObserving", fmt.Sprintf("Direct dependency %s is observing", reference.Name))
		default:
			result.markPending(reference.Name, "DependencyPending", fmt.Sprintf("Direct dependency %s is not ready", reference.Name))
		}
	}
	if result.ready {
		result.reason = "DependenciesReady"
		result.message = "All direct dependencies are ready"
	}
	return result, nil
}

func dependencyReferenceConflict(dependency *applicationv1.OneKSApplication, reference applicationv1.DependencyReference, clusterID string) *DependencyConflictError {
	if conflict := validateDependencyApplication(dependency, reference.Name, clusterID); conflict != nil {
		return conflict
	}
	if dependency.Spec.CatalogueChartID != reference.CatalogueChartID || dependency.Spec.PlanDigest != reference.PlanDigest {
		return dependencyConflict(reference.Name, "catalogueChartID or planDigest differs from the dependency reference")
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
		ctrl.LoggerFrom(ctx).V(1).Info("dependency retained by policy", "dependency", dependency.Name)
		r.event(consumer, corev1.EventTypeNormal, "DependencyRetained", fmt.Sprintf("Dependency application %s/%s retained by policy", dependency.Namespace, dependency.Name))
		return false, nil
	}
	if !containsString(dependency.Finalizers, applicationv1.ApplicationFinalizer) {
		finalizers := append([]string(nil), dependency.Finalizers...)
		_, err := r.patchApplicationFinalizers(
			ctx, dependency,
			append(finalizers, applicationv1.ApplicationFinalizer),
		)
		if err != nil {
			return false, fmt.Errorf("add dependency cleanup finalizer to %s/%s: %w", dependency.Namespace, dependency.Name, err)
		}
		ctrl.LoggerFrom(ctx).Info("dependency finalizer acquired", "dependency", dependency.Name)
		r.event(consumer, corev1.EventTypeNormal, "DependencyFinalizerAdded", fmt.Sprintf("Dependency application %s/%s cleanup finalizer added", dependency.Namespace, dependency.Name))
		return true, nil
	}
	ctrl.LoggerFrom(ctx).V(1).Info("deleting dependency", "dependency", dependency.Name)
	deleteErr := r.Delete(ctx, dependency, deletePreconditions(dependency)...)
	if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
		return false, fmt.Errorf("delete dependency application %s/%s: %w", dependency.Namespace, dependency.Name, deleteErr)
	}
	if deleteErr == nil {
		ctrl.LoggerFrom(ctx).Info("dependency deletion requested", "dependency", dependency.Name)
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
