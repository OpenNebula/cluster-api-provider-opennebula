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
	"net"
	"reflect"
	"strings"
	"time"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type managedAPIPreflightMode uint8

const (
	managedAPIsRequired managedAPIPreflightMode = iota
	managedAPIsMayBeUnavailable
)

func isRootApplication(app *applicationv1.OneKSApplication) bool {
	return app.Spec.Role == applicationv1.ApplicationRoleRoot
}

func managesTargetNamespace(app *applicationv1.OneKSApplication) bool {
	if !isRootApplication(app) {
		return false
	}
	matches := 0
	for _, resource := range app.Spec.ManagedResources {
		if resource.Scope == applicationv1.ManagedResourceScopeCluster &&
			resource.APIVersion == "v1" && resource.Kind == "Namespace" &&
			resource.Name == app.Spec.Release.TargetNamespace && resource.Namespace == "" {
			matches++
		}
	}
	return matches == 1
}

func desiredManagedResource(app *applicationv1.OneKSApplication, resource applicationv1.ManagedResourceSpec) (*unstructured.Unstructured, error) {
	object, err := parseManagedResource(resource)
	if err != nil {
		return nil, err
	}
	labels := object.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	for key, value := range ownershipLabels(app) {
		labels[key] = value
	}
	object.SetLabels(labels)
	return object, nil
}

func emptyManagedResource(resource applicationv1.ManagedResourceSpec) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	groupVersion, _ := schema.ParseGroupVersion(resource.APIVersion)
	object.SetGroupVersionKind(groupVersion.WithKind(resource.Kind))
	object.SetNamespace(resource.Namespace)
	object.SetName(resource.Name)
	return object
}

func (r *Reconciler) preflightManagedOwnership(ctx context.Context, app *applicationv1.OneKSApplication, deleting bool, managedAPIs managedAPIPreflightMode) error {
	if !isRootApplication(app) {
		return nil
	}
	reader := r.authoritativeReader()
	for _, resource := range app.Spec.ManagedResources {
		if deleting && resource.DeletionPolicy == applicationv1.DeletionPolicyRetain {
			continue
		}
		object := emptyManagedResource(resource)
		err := reader.Get(ctx, client.ObjectKeyFromObject(object), object)
		if apierrors.IsNotFound(err) {
			continue
		}
		if managedAPIs == managedAPIsMayBeUnavailable && meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("preflight managed %s %s/%s: %w", resource.Kind, resource.Namespace, resource.Name, err)
		}
		if !ownershipMatches(app, object) {
			return &OwnershipConflictError{Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name}
		}
	}
	return nil
}

func (r *Reconciler) reconcileManagedResources(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	order, validationError := managedResourceOrder(app.Spec.ManagedResources)
	if validationError != nil {
		return false, validationError
	}
	reader := r.authoritativeReader()
	for _, index := range order {
		resource := app.Spec.ManagedResources[index]
		desired, err := desiredManagedResource(app, resource)
		if err != nil {
			return false, fmt.Errorf("parse managed resource %s: %w", resource.ID, err)
		}
		current := emptyManagedResource(resource)
		err = reader.Get(ctx, client.ObjectKeyFromObject(current), current)
		if apierrors.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).V(1).Info(
				"applying managed resource",
				"action", "create", "resourceID", resource.ID,
				"apiVersion", resource.APIVersion, "kind", resource.Kind,
				"resourceNamespace", resource.Namespace, "name", resource.Name,
			)
			if createErr := r.Create(ctx, desired, client.FieldOwner(applicationv1.FieldManager)); createErr != nil {
				if apierrors.IsAlreadyExists(createErr) {
					current = emptyManagedResource(resource)
					if rereadErr := reader.Get(ctx, client.ObjectKeyFromObject(current), current); rereadErr != nil {
						if apierrors.IsNotFound(rereadErr) {
							return false, nil
						}
						return false, fmt.Errorf("re-read raced managed %s %s/%s: %w", resource.Kind, resource.Namespace, resource.Name, rereadErr)
					}
					if !ownershipMatches(app, current) {
						return false, &OwnershipConflictError{Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name}
					}
					if managedResourceNeedsApply(current, desired) {
						desired.SetResourceVersion(current.GetResourceVersion())
						ctrl.LoggerFrom(ctx).V(1).Info(
							"applying managed resource",
							"action", "update", "resourceID", resource.ID,
							"apiVersion", resource.APIVersion, "kind", resource.Kind,
							"resourceNamespace", resource.Namespace, "name", resource.Name,
						)
						if patchErr := r.Patch(ctx, desired, client.Apply, client.FieldOwner(applicationv1.FieldManager)); patchErr != nil {
							return false, fmt.Errorf("apply raced managed %s %s/%s: %w", resource.Kind, resource.Namespace, resource.Name, patchErr)
						}
						ctrl.LoggerFrom(ctx).Info(
							"managed resource applied",
							"resourceID", resource.ID,
							"apiVersion", resource.APIVersion, "kind", resource.Kind,
							"resourceNamespace", resource.Namespace, "name", resource.Name,
						)
					}
					continue
				}
				return false, fmt.Errorf("create managed %s %s/%s: %w", resource.Kind, resource.Namespace, resource.Name, createErr)
			}
			ctrl.LoggerFrom(ctx).Info(
				"managed resource created",
				"resourceID", resource.ID,
				"apiVersion", resource.APIVersion, "kind", resource.Kind,
				"resourceNamespace", resource.Namespace, "name", resource.Name,
			)
			r.event(app, corev1.EventTypeNormal, "ResourceCreated", fmt.Sprintf("%s %s/%s created", resource.Kind, resource.Namespace, resource.Name))
			continue
		}
		if err != nil {
			return false, fmt.Errorf("get managed %s %s/%s: %w", resource.Kind, resource.Namespace, resource.Name, err)
		}
		if !ownershipMatches(app, current) {
			return false, &OwnershipConflictError{Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name}
		}
		if managedResourceNeedsApply(current, desired) {
			desired.SetResourceVersion(current.GetResourceVersion())
			ctrl.LoggerFrom(ctx).V(1).Info(
				"applying managed resource",
				"action", "update", "resourceID", resource.ID,
				"apiVersion", resource.APIVersion, "kind", resource.Kind,
				"resourceNamespace", resource.Namespace, "name", resource.Name,
			)
			if err := r.Patch(ctx, desired, client.Apply, client.FieldOwner(applicationv1.FieldManager)); err != nil {
				return false, fmt.Errorf("apply managed %s %s/%s: %w", resource.Kind, resource.Namespace, resource.Name, err)
			}
			ctrl.LoggerFrom(ctx).Info(
				"managed resource applied",
				"resourceID", resource.ID,
				"apiVersion", resource.APIVersion, "kind", resource.Kind,
				"resourceNamespace", resource.Namespace, "name", resource.Name,
			)
			r.event(app, corev1.EventTypeNormal, "ResourceApplied", fmt.Sprintf("%s %s/%s applied", resource.Kind, resource.Namespace, resource.Name))
		}
	}
	observed, err := r.observeManagedResources(ctx, app, true)
	return err == nil && observed.allResources, err
}

func managedResourceNeedsApply(current, desired *unstructured.Unstructured) bool {
	for key, desiredValue := range desired.Object {
		if key == "metadata" {
			continue
		}
		if !reflect.DeepEqual(current.Object[key], desiredValue) {
			return true
		}
	}
	if !ownedLabelsEqual(current.GetLabels(), desired.GetLabels()) {
		return true
	}
	for key, value := range desired.GetAnnotations() {
		if current.GetAnnotations()[key] != value {
			return true
		}
	}
	return false
}

func (r *Reconciler) observeManagedResources(ctx context.Context, app *applicationv1.OneKSApplication, readinessEnabled bool) (observation, error) {
	result := observation{allResources: true, current: app.Spec.Release.ReleaseName}
	if !readinessEnabled {
		for _, resource := range app.Spec.ManagedResources {
			result.resources = append(result.resources, applicationv1.ResourceStatus{
				ID: resource.ID, Phase: "Pending", Reason: "DependenciesPending",
				Message: "Managed resource readiness is gated by direct dependencies",
			})
			result.allResources = false
			if result.current == app.Spec.Release.ReleaseName {
				result.current = resource.ID
			}
		}
		return result, nil
	}
	now := metav1.NewTime(r.now())
	for _, resource := range app.Spec.ManagedResources {
		previous := resourceStatusByID(app.Status.Resources, resource.ID)
		status := applicationv1.ResourceStatus{ID: resource.ID, Phase: "Pending", Reason: "NotFound", Message: fmt.Sprintf("%s is absent", resource.Kind)}
		object := emptyManagedResource(resource)
		err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(object), object)
		if apierrors.IsNotFound(err) {
			if previous != nil && previous.Phase == "Failed" && previous.Reason == "ReadinessTimeout" {
				status.Phase = "Failed"
				status.Reason = "ReadinessTimeout"
				status.Message = previous.Message
				status.ReadinessStartedAt = copyTime(previous.ReadinessStartedAt)
				result.markResourceFailed(resource.ID, status.Reason, status.Message)
				result.resources = append(result.resources, status)
				continue
			}
			result.allResources = false
			if result.current == app.Spec.Release.ReleaseName {
				result.current = resource.ID
			}
			result.resources = append(result.resources, status)
			continue
		}
		if err != nil {
			return result, fmt.Errorf("observe managed %s %s/%s: %w", resource.Kind, resource.Namespace, resource.Name, err)
		}
		status.ResourceVersion = object.GetResourceVersion()
		if !ownershipMatches(app, object) {
			status.Phase = "Failed"
			status.Reason = "OwnershipConflict"
			status.Message = "Managed object does not have exact OneKS ownership"
			result.markResourceFailed(resource.ID, status.Reason, status.Message)
			result.resources = append(result.resources, status)
			continue
		}
		if previous != nil && previous.Phase == "Failed" && previous.Reason == "ReadinessTimeout" {
			status.Phase = "Failed"
			status.Reason = "ReadinessTimeout"
			status.Message = previous.Message
			status.ReadinessStartedAt = copyTime(previous.ReadinessStartedAt)
			result.markResourceFailed(resource.ID, status.Reason, status.Message)
			result.resources = append(result.resources, status)
			continue
		}
		ready, reason, message, err := r.managedResourceReady(ctx, object, resource)
		if err != nil {
			return result, err
		}
		if ready {
			status.Phase = "Ready"
			status.Reason = "ReadinessSatisfied"
			status.Message = "Managed resource is ready"
			result.completed++
		} else {
			status.ReadinessStartedAt = &now
			if previous != nil && previous.Phase != "Ready" && previous.ReadinessStartedAt != nil {
				status.ReadinessStartedAt = copyTime(previous.ReadinessStartedAt)
			}
			if !now.Time.Before(status.ReadinessStartedAt.Add(time.Duration(resource.Readiness.TimeoutSeconds) * time.Second)) {
				status.Phase = "Failed"
				status.Reason = "ReadinessTimeout"
				status.Message = truncate(fmt.Sprintf("Managed resource %s exceeded its readiness timeout", resource.ID), 512)
				result.markResourceFailed(resource.ID, status.Reason, status.Message)
			} else {
				status.Phase = "Applying"
				status.Reason = reason
				status.Message = truncate(message, 512)
				result.allResources = false
				if result.current == app.Spec.Release.ReleaseName {
					result.current = resource.ID
				}
			}
		}
		result.resources = append(result.resources, status)
	}
	return result, nil
}

func (result *observation) markResourceFailed(id, reason, message string) {
	result.allResources = false
	result.resourcesFailed = true
	if result.resourcesReason == "" {
		result.resourcesReason = reason
		result.resourcesMessage = message
		result.current = id
	}
}

func (r *Reconciler) managedResourceReady(ctx context.Context, object *unstructured.Unstructured, resource applicationv1.ManagedResourceSpec) (bool, string, string, error) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, expected := range resource.Readiness.Conditions {
		matched := false
		for _, raw := range conditions {
			condition, ok := raw.(map[string]any)
			if ok && condition["type"] == expected.Type && condition["status"] == expected.Status {
				matched = true
				break
			}
		}
		if !matched {
			return false, "ConditionPending", fmt.Sprintf("Condition %s=%s is not satisfied", expected.Type, expected.Status), nil
		}
	}
	for _, reference := range resource.Readiness.RequiredResources {
		exists, err := r.requiredResourceExists(ctx, reference)
		if err != nil {
			return false, "", "", err
		}
		if !exists {
			return false, "RequiredResourceMissing", fmt.Sprintf("Required %s %s/%s is absent", reference.Kind, reference.Namespace, reference.Name), nil
		}
	}
	for _, check := range resource.Readiness.Checks {
		ready, err := r.dnsMatchesService(ctx, check)
		if err != nil {
			return false, "", "", err
		}
		if !ready {
			return false, "DNSCheckPending", "DNS hostname does not resolve to a Service cluster IP", nil
		}
	}
	return true, "ReadinessSatisfied", "Managed resource is ready", nil
}

func (r *Reconciler) requiredResourceExists(ctx context.Context, reference applicationv1.ManagedResourceReference) (bool, error) {
	groupVersion, _ := schema.ParseGroupVersion(reference.APIVersion)
	metadata := &metav1.PartialObjectMetadata{}
	metadata.SetGroupVersionKind(groupVersion.WithKind(reference.Kind))
	err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: reference.Namespace, Name: reference.Name}, metadata)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("observe required %s %s/%s: %w", reference.Kind, reference.Namespace, reference.Name, err)
	}
	return true, nil
}

func (r *Reconciler) dnsMatchesService(ctx context.Context, check applicationv1.ManagedResourceCheck) (bool, error) {
	service := &corev1.Service{}
	if err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: check.Service.Namespace, Name: check.Service.Name}, service); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("observe readiness Service %s/%s: %w", check.Service.Namespace, check.Service.Name, err)
	}
	serviceIPs := make(map[string]struct{})
	for _, address := range service.Spec.ClusterIPs {
		if address != "" && address != corev1.ClusterIPNone {
			serviceIPs[address] = struct{}{}
		}
	}
	if len(serviceIPs) == 0 && service.Spec.ClusterIP != "" && service.Spec.ClusterIP != corev1.ClusterIPNone {
		serviceIPs[service.Spec.ClusterIP] = struct{}{}
	}
	lookup := r.DNSLookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	addresses, err := lookup(ctx, check.Hostname)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, nil
	}
	for _, address := range addresses {
		if parsed := net.ParseIP(strings.TrimSpace(address)); parsed != nil {
			if _, found := serviceIPs[parsed.String()]; found {
				return true, nil
			}
		}
	}
	return false, nil
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func resourceStatusByID(statuses []applicationv1.ResourceStatus, id string) *applicationv1.ResourceStatus {
	for index := range statuses {
		if statuses[index].ID == id {
			return &statuses[index]
		}
	}
	return nil
}

func copyTime(value *metav1.Time) *metav1.Time {
	if value == nil {
		return nil
	}
	copy := value.DeepCopy()
	return copy
}

func (r *Reconciler) reconcileDeleteManagedResources(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	if err := r.preflightManagedOwnership(ctx, app, true, managedAPIsRequired); err != nil {
		return false, err
	}
	order, validationError := managedResourceOrder(app.Spec.ManagedResources)
	if validationError != nil {
		return false, validationError
	}
	for position := len(order) - 1; position >= 0; position-- {
		index := order[position]
		resource := app.Spec.ManagedResources[index]
		if resource.DeletionPolicy == applicationv1.DeletionPolicyRetain {
			continue
		}
		object := emptyManagedResource(resource)
		err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(object), object)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("get deleting managed %s %s/%s: %w", resource.Kind, resource.Namespace, resource.Name, err)
		}
		if !ownershipMatches(app, object) {
			return false, &OwnershipConflictError{Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name}
		}
		ctrl.LoggerFrom(ctx).V(1).Info(
			"deleting managed resource",
			"resourceID", resource.ID,
			"apiVersion", resource.APIVersion, "kind", resource.Kind,
			"resourceNamespace", resource.Namespace, "name", resource.Name,
		)
		deleteErr := r.Delete(ctx, object, deletePreconditions(object)...)
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return false, fmt.Errorf("delete managed %s %s/%s: %w", resource.Kind, resource.Namespace, resource.Name, deleteErr)
		}
		if deleteErr == nil {
			ctrl.LoggerFrom(ctx).Info(
				"managed resource deletion requested",
				"resourceID", resource.ID,
				"apiVersion", resource.APIVersion, "kind", resource.Kind,
				"resourceNamespace", resource.Namespace, "name", resource.Name,
			)
		}
		r.event(app, corev1.EventTypeNormal, "ResourceDeleted", fmt.Sprintf("%s %s/%s deletion requested", resource.Kind, resource.Namespace, resource.Name))
		return true, nil
	}
	return false, nil
}

func deletingManagedResourceStatuses(app *applicationv1.OneKSApplication) []applicationv1.ResourceStatus {
	statuses := make([]applicationv1.ResourceStatus, 0, len(app.Spec.ManagedResources))
	for _, resource := range app.Spec.ManagedResources {
		status := applicationv1.ResourceStatus{
			ID: resource.ID, Phase: "Deleting", Reason: "DeletionInProgress",
			Message: "Managed resource deletion is in progress",
		}
		if resource.DeletionPolicy == applicationv1.DeletionPolicyRetain {
			status.Phase = "Retained"
			status.Reason = "RetainPolicy"
			status.Message = "Managed resource is retained by policy"
		}
		statuses = append(statuses, status)
	}
	return statuses
}
