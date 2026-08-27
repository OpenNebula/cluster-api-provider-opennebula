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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	HelmChartNamespace = "kube-system"
	defaultRequeue     = 15 * time.Second
)

var helmChartGVK = schema.GroupVersionKind{
	Group: "helm.cattle.io", Version: "v1", Kind: "HelmChart",
}

type Reconciler struct {
	client.Client
	APIReader         client.Reader
	Scheme            *runtime.Scheme
	Recorder          record.EventRecorder
	ClusterID         string
	ControllerVersion string
	RequeueAfter      time.Duration
	DNSLookup         func(context.Context, string) ([]string, error)
	Now               func() time.Time
}

type observation struct {
	resources               []applicationv1.ResourceStatus
	managedResourcesReady   bool
	protectedSecretsReady   bool
	protectedSecretsFailed  bool
	protectedSecretsReason  string
	protectedSecretsMessage string
	helm                    *unstructured.Unstructured
	helmReady               bool
	helmFailed              bool
	helmReason              string
	helmMessage             string
	resourcesFailed         bool
	resourcesReason         string
	resourcesMessage        string
	current                 string
	completed               int32
	allResources            bool
}

func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, reconcileErr error) {
	app := &applicationv1.OneKSApplication{}
	if err := r.Get(ctx, request.NamespacedName, app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctx = contextWithApplicationLogger(ctx, app)
	ctrl.LoggerFrom(ctx).Info(
		"application reconciliation started",
		"observedGeneration", app.Status.ObservedGeneration,
		"phase", app.Status.Phase,
	)
	defer func() {
		ctrl.LoggerFrom(ctx).Info(
			"application reconciliation completed",
			"state", app.Status.Phase,
			"requeue", result.Requeue,
			"requeueAfter", result.RequeueAfter,
			"success", reconcileErr == nil,
		)
	}()

	deleting := !app.DeletionTimestamp.IsZero()
	hasCleanupFinalizer := containsString(app.Finalizers, applicationv1.ApplicationFinalizer)
	var validationError *PlanError
	if deleting && hasCleanupFinalizer {
		validationError = ValidateDeletionPlan(app)
	} else {
		validationError = ValidatePlan(app, ValidationConfig{ClusterID: r.ClusterID})
	}
	if validationError != nil {
		return r.recordTerminal(ctx, app, validationError.Reason, validationError.Message, false)
	}
	if !deleting && app.Spec.ExecutionMode == applicationv1.ExecutionModeExecute &&
		!hasCleanupFinalizer {
		updated, err := r.patchApplicationFinalizers(
			ctx, app, append(append([]string(nil), app.Finalizers...), applicationv1.ApplicationFinalizer),
		)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("add application finalizer: %w", err)
		}
		ctrl.LoggerFrom(ctx).Info("application finalizer acquired before input binding")
		r.event(updated, corev1.EventTypeNormal, "FinalizerAdded", "Application cleanup finalizer added")
		return ctrl.Result{Requeue: true}, nil
	}
	if !deleting {
		bound, err := r.bindSecretInput(ctx, app)
		if err != nil {
			var invalidInput *InputSecretValidationError
			if errors.As(err, &invalidInput) {
				return r.recordTerminal(ctx, app, "InputSecretInvalid", invalidInput.Error(), false)
			}
			return ctrl.Result{}, err
		}
		if bound {
			return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
		}
	}

	if !deleting && !app.Spec.Release.CreateNamespace {
		if err := r.checkTargetNamespace(ctx, app.Spec.Release.TargetNamespace); err != nil {
			if apierrors.IsNotFound(err) {
				if !managesTargetNamespace(app) {
					result, statusErr := r.recordTerminal(
						ctx, app, "TargetNamespaceMissing",
						fmt.Sprintf("target namespace %s is missing", app.Spec.Release.TargetNamespace), false,
					)
					if statusErr != nil {
						return ctrl.Result{}, statusErr
					}
					result.RequeueAfter = r.requeueDuration()
					return result, nil
				}
			} else {
				return ctrl.Result{}, fmt.Errorf("check target namespace: %w", err)
			}
		}
	}

	externalMode := ""
	externalSelectionToPersist := ""
	if !deleting && app.Spec.ExecutionMode == applicationv1.ExecutionModeExecute && usesExternalDetection(app) {
		var selectionErr error
		externalMode, selectionErr = externalSelection(app)
		if selectionErr != nil {
			result, statusErr := r.recordTerminal(ctx, app, "ExternalSelectionInvalid", selectionErr.Error(), false)
			if statusErr == nil {
				result.RequeueAfter = r.requeueDuration()
			}
			return result, statusErr
		}
		if externalMode == "" {
			detection, detectionErr := r.detectExternalDependency(ctx, app)
			if detectionErr != nil {
				return ctrl.Result{}, detectionErr
			}
			switch detection.state {
			case externalDetectionUsable:
				externalSelectionToPersist = ExternalSelectionExternal
			case externalDetectionAbsent:
				externalSelectionToPersist = ExternalSelectionManaged
			case externalDetectionUnusable:
				result, statusErr := r.recordTerminal(ctx, app, "ExternalDependencyUnusable", detection.message, false)
				if statusErr == nil {
					result.RequeueAfter = r.requeueDuration()
				}
				return result, statusErr
			}
		}
	}

	if deleting {
		if app.Spec.ExecutionMode == applicationv1.ExecutionModeObserve {
			return ctrl.Result{}, nil
		}
		if err := r.preflightOwnership(ctx, app, true, managedAPIsRequired); err != nil {
			var conflict *OwnershipConflictError
			if errors.As(err, &conflict) {
				return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
			}
			return ctrl.Result{}, err
		}
		return r.reconcileDelete(ctx, app)
	}

	if app.Spec.ExecutionMode == applicationv1.ExecutionModeObserve {
		dependencies, err := r.observeDependencies(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.preflightOwnership(ctx, app, false, managedAPIsRequired); err != nil {
			var conflict *OwnershipConflictError
			if errors.As(err, &conflict) {
				return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
			}
			return ctrl.Result{}, err
		}
		return r.reconcileStatus(ctx, app, true, dependencies)
	}

	if externalMode != ExternalSelectionExternal && externalSelectionToPersist != ExternalSelectionExternal {
		if err := r.preflightOwnership(ctx, app, false, managedAPIsMayBeUnavailable); err != nil {
			var conflict *OwnershipConflictError
			if errors.As(err, &conflict) {
				return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
			}
			return ctrl.Result{}, err
		}
	}

	if !containsString(app.Finalizers, applicationv1.ApplicationFinalizer) {
		finalizers := append([]string(nil), app.Finalizers...)
		updated, err := r.patchApplicationFinalizers(
			ctx, app, append(finalizers, applicationv1.ApplicationFinalizer),
		)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("add application finalizer: %w", err)
		}
		ctrl.LoggerFrom(ctx).Info("application finalizer acquired")
		r.event(updated, corev1.EventTypeNormal, "FinalizerAdded", "Application cleanup finalizer added")
		return ctrl.Result{Requeue: true}, nil
	}
	if externalSelectionToPersist != "" {
		updated, err := r.patchExternalSelection(ctx, app, externalSelectionToPersist)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("persist external dependency selection: %w", err)
		}
		ctrl.LoggerFrom(ctx).Info("external dependency lifecycle selected", "selection", externalSelectionToPersist)
		r.event(updated, corev1.EventTypeNormal, "ExternalDependencySelected", fmt.Sprintf("Dependency selected %s lifecycle", externalSelectionToPersist))
		return ctrl.Result{Requeue: true}, nil
	}

	if app.Spec.Role == applicationv1.ApplicationRoleRoot {
		raced, terminating, conflict, err := r.materializeRootDependencies(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		if terminating != "" {
			dependencies := dependencyObservation{
				enabled: true, ready: false, current: terminating,
				reason: "DependencyTerminating", message: fmt.Sprintf("Dependency application %s is terminating", terminating),
			}
			return r.reconcileStatus(ctx, app, false, dependencies)
		}
		if conflict != nil {
			dependencies := dependencyObservation{
				enabled: true, ready: false, conflict: true,
				reason: "DependencyConflict", message: conflict.Error(), current: conflict.Name,
			}
			r.event(app, corev1.EventTypeWarning, dependencies.reason, dependencies.message)
			return r.reconcileStatus(ctx, app, false, dependencies)
		}
		if raced {
			dependencies, observeErr := r.observeDependencies(ctx, app)
			if observeErr != nil {
				return ctrl.Result{}, observeErr
			}
			return r.reconcileStatus(ctx, app, false, dependencies)
		}
	}

	dependencies, err := r.observeDependencies(ctx, app)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !dependencies.ready {
		return r.reconcileStatus(ctx, app, false, dependencies)
	}
	if externalMode == ExternalSelectionExternal {
		return r.reconcileStatus(ctx, app, false, dependencies)
	}
	if isRootApplication(app) {
		if err := r.preflightOwnership(ctx, app, false, managedAPIsRequired); err != nil {
			var conflict *OwnershipConflictError
			if errors.As(err, &conflict) {
				return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
			}
			return ctrl.Result{}, err
		}
	}

	resourcesReady := true
	if isRootApplication(app) {
		resourcesReady, err = r.reconcileManagedResources(ctx, app)
	}
	if err != nil {
		var conflict *OwnershipConflictError
		if errors.As(err, &conflict) {
			return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
		}
		return ctrl.Result{}, err
	}
	if !resourcesReady {
		return r.reconcileStatus(ctx, app, false, dependencies)
	}
	if usesProtectedSecrets(app) {
		protectedReady, protectedErr := r.reconcileProtectedSecrets(ctx, app)
		if protectedErr != nil {
			var conflict *OwnershipConflictError
			if errors.As(protectedErr, &conflict) {
				return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
			}
			var invalidInput *InputSecretValidationError
			if errors.As(protectedErr, &invalidInput) {
				return r.recordTerminal(ctx, app, "InputSecretInvalid", invalidInput.Error(), false)
			}
			return ctrl.Result{}, protectedErr
		}
		if !protectedReady {
			return r.reconcileStatus(ctx, app, false, dependencies)
		}
	}

	if err := r.reconcileHelmChart(ctx, app); err != nil {
		var conflict *OwnershipConflictError
		if errors.As(err, &conflict) {
			return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
		}
		return ctrl.Result{}, err
	}
	return r.reconcileStatus(ctx, app, false, dependencies)
}

func (r *Reconciler) checkTargetNamespace(ctx context.Context, targetNamespace string) error {
	namespace := &corev1.Namespace{}
	return r.authoritativeReader().Get(ctx, types.NamespacedName{Name: targetNamespace}, namespace)
}

func (r *Reconciler) authoritativeReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *Reconciler) preflightOwnership(ctx context.Context, app *applicationv1.OneKSApplication, deleting bool, managedAPIs managedAPIPreflightMode) error {
	reader := r.authoritativeReader()
	if !deleting {
		if err := r.preflightManagedOwnership(ctx, app, false, managedAPIs); err != nil {
			return err
		}
		if err := r.preflightProtectedSecretOwnership(ctx, app, false); err != nil {
			return err
		}
	}
	if deleting && app.Spec.DeletionPolicy == applicationv1.DeletionPolicyRetain {
		return nil
	}
	if usesExternalDetection(app) {
		selection, err := externalSelection(app)
		if err != nil {
			return err
		}
		if selection == ExternalSelectionExternal ||
			(app.Spec.ExecutionMode == applicationv1.ExecutionModeObserve && selection != ExternalSelectionManaged) ||
			(deleting && selection != ExternalSelectionManaged) {
			return nil
		}
	}
	helm := helmChartObject(app.Spec.Release.ReleaseName)
	err := reader.Get(ctx, client.ObjectKeyFromObject(helm), helm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("preflight HelmChart %s/%s: %w", HelmChartNamespace, app.Spec.Release.ReleaseName, err)
	}
	if !ownershipMatches(app, helm) {
		return &OwnershipConflictError{Kind: "HelmChart", Namespace: HelmChartNamespace, Name: app.Spec.Release.ReleaseName}
	}
	return nil
}

func (r *Reconciler) reconcileHelmChart(ctx context.Context, app *applicationv1.OneKSApplication) error {
	desired := desiredHelmChart(app)
	current := helmChartObject(desired.GetName())
	err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).V(1).Info(
			"reconciling Helm release",
			"action", "create", "release", app.Spec.Release.ReleaseName,
			"releaseNamespace", app.Spec.Release.TargetNamespace,
		)
		if err := r.Create(ctx, desired, client.FieldOwner(applicationv1.FieldManager)); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return &OwnershipConflictError{Kind: "HelmChart", Namespace: desired.GetNamespace(), Name: desired.GetName()}
			}
			return fmt.Errorf("create HelmChart %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)
		}
		ctrl.LoggerFrom(ctx).Info(
			"Helm release created",
			"release", app.Spec.Release.ReleaseName,
			"releaseNamespace", app.Spec.Release.TargetNamespace,
		)
		r.event(app, corev1.EventTypeNormal, "HelmChartCreated", fmt.Sprintf("HelmChart %s/%s created", desired.GetNamespace(), desired.GetName()))
		return nil
	}
	if err != nil {
		return fmt.Errorf("get HelmChart %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)
	}
	if !ownershipMatches(app, current) {
		return &OwnershipConflictError{Kind: "HelmChart", Namespace: desired.GetNamespace(), Name: desired.GetName()}
	}
	if helmChartNeedsApply(current, desired) {
		// SSA honors resourceVersion as an optimistic precondition.
		desired.SetResourceVersion(current.GetResourceVersion())
		ctrl.LoggerFrom(ctx).V(1).Info(
			"reconciling Helm release",
			"action", "update", "release", app.Spec.Release.ReleaseName,
			"releaseNamespace", app.Spec.Release.TargetNamespace,
		)
		if err := r.Patch(ctx, desired, client.Apply, client.FieldOwner(applicationv1.FieldManager)); err != nil {
			return fmt.Errorf("apply HelmChart %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)
		}
		ctrl.LoggerFrom(ctx).Info(
			"Helm release applied",
			"release", app.Spec.Release.ReleaseName,
			"releaseNamespace", app.Spec.Release.TargetNamespace,
		)
		r.event(app, corev1.EventTypeNormal, "HelmChartApplied", fmt.Sprintf("HelmChart %s/%s applied", desired.GetNamespace(), desired.GetName()))
	}
	return nil
}

func desiredHelmChart(app *applicationv1.OneKSApplication) *unstructured.Unstructured {
	object := helmChartObject(app.Spec.Release.ReleaseName)
	object.SetLabels(ownershipLabels(app))
	object.SetAnnotations(map[string]string{ChartIDAnnotation: app.Spec.Release.ChartID})
	spec := map[string]any{
		"chart":   app.Spec.Release.Chart,
		"version": app.Spec.Release.Version, "targetNamespace": app.Spec.Release.TargetNamespace,
		"createNamespace": app.Spec.Release.CreateNamespace, "valuesContent": app.Spec.Release.ValuesContent,
	}
	if app.Spec.Release.RepositoryURL != "" {
		spec["repo"] = app.Spec.Release.RepositoryURL
	}
	if app.Spec.Release.AuthSecret != nil {
		spec["authSecret"] = map[string]any{"name": app.Spec.Release.AuthSecret.Name}
	}
	object.Object["spec"] = spec
	return object
}

func helmChartObject(name string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(helmChartGVK)
	object.SetNamespace(HelmChartNamespace)
	object.SetName(name)
	return object
}

func helmChartNeedsApply(current, desired *unstructured.Unstructured) bool {
	currentSpec, _, _ := unstructured.NestedMap(current.Object, "spec")
	desiredSpec, _, _ := unstructured.NestedMap(desired.Object, "spec")
	return !reflect.DeepEqual(currentSpec, desiredSpec) ||
		!labelSubsetMatches(current.GetLabels(), desired.GetLabels()) ||
		current.GetAnnotations()[ChartIDAnnotation] != desired.GetAnnotations()[ChartIDAnnotation]
}

func (r *Reconciler) reconcileStatus(ctx context.Context, app *applicationv1.OneKSApplication, observeOnly bool, dependencies dependencyObservation) (ctrl.Result, error) {
	managedReadinessEnabled := dependencies.ready || observeOnly
	observed, err := r.observe(ctx, app, managedReadinessEnabled)
	if err != nil {
		return ctrl.Result{}, err
	}
	status := baseStatus(app, r.ControllerVersion)
	status.Resources = observed.resources
	status.Progress = applicationv1.ApplicationProgress{
		Completed: observed.completed + dependencies.completed, Total: applicationProgressTotal(app), Current: observed.current,
	}
	if dependencies.enabled && !dependencies.ready && dependencies.current != "" {
		status.Progress.Current = dependencies.current
	}
	status.HelmChartRef = nil
	if observed.helm != nil {
		status.HelmChartRef = &applicationv1.HelmChartReference{
			Namespace: observed.helm.GetNamespace(), Name: observed.helm.GetName(),
			UID: string(observed.helm.GetUID()), ResourceVersion: observed.helm.GetResourceVersion(),
		}
	}
	setCondition(&status, app.Generation, ConditionPlanValid, metav1.ConditionTrue, "Validated", "Plan digest and schema are valid")
	if dependencies.enabled {
		dependencyCondition := metav1.ConditionFalse
		if dependencies.ready {
			dependencyCondition = metav1.ConditionTrue
		}
		setCondition(&status, app.Generation, ConditionDependenciesReady, dependencyCondition, dependencies.reason, dependencies.message)
	}
	resourceCondition := metav1.ConditionFalse
	resourcesReady := observed.allResources
	if isRootApplication(app) {
		resourcesReady = observed.managedResourcesReady
	}
	if resourcesReady {
		resourceCondition = metav1.ConditionTrue
	}
	resourceReason := conditionText(resourceCondition, "ResourcesReady", "ResourcesPending")
	resourceMessage := conditionText(resourceCondition, "All managed resources are ready", "Managed resources are not ready")
	if !isRootApplication(app) {
		resourceMessage = conditionText(resourceCondition, "Dependency has no managed resources", "Dependency resources are not ready")
	} else if observed.resourcesFailed {
		resourceReason = observed.resourcesReason
		resourceMessage = observed.resourcesMessage
	} else if dependencies.enabled && !dependencies.ready {
		resourceCondition = metav1.ConditionUnknown
		resourceReason = "DependenciesPending"
		resourceMessage = "Managed resources are gated by direct dependencies"
	}
	setCondition(&status, app.Generation, ConditionResourcesReady, resourceCondition, resourceReason, resourceMessage)
	if usesProtectedSecrets(app) {
		protectedCondition := metav1.ConditionFalse
		if observed.protectedSecretsReady {
			protectedCondition = metav1.ConditionTrue
		}
		setCondition(
			&status, app.Generation, ConditionProtectedSecretsReady, protectedCondition,
			conditionText(protectedCondition, "ProtectedSecretsReady", observed.protectedSecretsReason),
			conditionText(protectedCondition, "All protected Secrets are ready", observed.protectedSecretsMessage),
		)
	}
	helmCondition := metav1.ConditionFalse
	if observed.helmReady {
		helmCondition = metav1.ConditionTrue
	}
	helmReason := conditionText(helmCondition, "HelmReleaseReady", observed.helmReason)
	helmMessage := conditionText(helmCondition, "Helm release is ready", observed.helmMessage)
	if observed.helmReady && observed.helmReason == "ExternalDependencyReady" {
		helmReason = observed.helmReason
		helmMessage = observed.helmMessage
	}
	setCondition(&status, app.Generation, ConditionHelmReleaseReady, helmCondition, helmReason, helmMessage)
	setCondition(&status, app.Generation, ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflict", "Managed children have exact OneKS ownership")

	ready := dependencies.ready && observed.allResources && observed.helmReady
	readyCondition := metav1.ConditionFalse
	if ready {
		readyCondition = metav1.ConditionTrue
	}
	readyReason := "ApplicationProgressing"
	readyMessage := "Application installation is in progress"
	if observed.resourcesFailed {
		readyReason = observed.resourcesReason
		readyMessage = observed.resourcesMessage
	} else if observed.protectedSecretsFailed {
		readyReason = observed.protectedSecretsReason
		readyMessage = observed.protectedSecretsMessage
	} else if observed.helmFailed {
		readyReason = observed.helmReason
		readyMessage = observed.helmMessage
	} else if dependencies.enabled && !dependencies.ready {
		readyReason = dependencies.reason
		readyMessage = dependencies.message
	}
	setCondition(&status, app.Generation, ConditionReady, readyCondition, conditionText(readyCondition, "ApplicationReady", readyReason), conditionText(readyCondition, "Application resources and Helm release are ready", readyMessage))
	clearLastError(&status)

	if observeOnly {
		status.Phase = applicationv1.PhaseObserving
	} else if dependencies.failed || dependencies.conflict {
		status.Phase = applicationv1.PhaseFailed
		setLastError(&status, dependencies.reason, dependencies.message)
	} else if observed.resourcesFailed {
		status.Phase = applicationv1.PhaseFailed
		setLastError(&status, observed.resourcesReason, observed.resourcesMessage)
	} else if observed.protectedSecretsFailed {
		status.Phase = applicationv1.PhaseFailed
		setLastError(&status, observed.protectedSecretsReason, observed.protectedSecretsMessage)
	} else if observed.helmFailed {
		status.Phase = applicationv1.PhaseFailed
		setLastError(&status, observed.helmReason, observed.helmMessage)
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

func (r *Reconciler) observe(ctx context.Context, app *applicationv1.OneKSApplication, managedReadinessEnabled bool) (observation, error) {
	if isRootApplication(app) {
		result, err := r.observeManagedResources(ctx, app, managedReadinessEnabled)
		if err != nil {
			return result, err
		}
		result.managedResourcesReady = result.allResources
		if usesProtectedSecrets(app) {
			protected, protectedErr := r.observeProtectedSecrets(ctx, app, result.managedResourcesReady)
			if protectedErr != nil {
				return result, protectedErr
			}
			result.resources = append(result.resources, protected.statuses...)
			result.completed += protected.completed
			result.protectedSecretsReady = protected.ready
			result.protectedSecretsFailed = protected.failed
			result.protectedSecretsReason = protected.reason
			result.protectedSecretsMessage = protected.message
			result.allResources = result.managedResourcesReady && protected.ready
			if result.managedResourcesReady && !protected.ready && protected.current != "" {
				result.current = protected.current
			}
		}
		return r.observeHelm(ctx, app, result)
	}
	result := observation{allResources: true, current: app.Spec.Release.ReleaseName}
	return r.observeHelm(ctx, app, result)
}

func (r *Reconciler) observeHelm(ctx context.Context, app *applicationv1.OneKSApplication, result observation) (observation, error) {
	if usesExternalDetection(app) {
		selection, err := externalSelection(app)
		if err != nil {
			return result, err
		}
		if selection != ExternalSelectionManaged {
			detection, detectionErr := r.detectExternalDependency(ctx, app)
			if detectionErr != nil {
				return result, detectionErr
			}
			if detection.state == externalDetectionUsable {
				result.helmReady = true
				result.completed++
				result.helmReason = "ExternalDependencyReady"
				result.helmMessage = detection.message
				return result, nil
			}
			result.helmFailed = true
			if selection == ExternalSelectionExternal {
				result.helmReason = "ExternalDependencyLost"
				result.helmMessage = "Previously selected external prerequisite is no longer usable: " + detection.message
			} else {
				result.helmReason = "ExternalDependencyUnusable"
				result.helmMessage = detection.message
			}
			return result, nil
		}
	}
	helm := helmChartObject(app.Spec.Release.ReleaseName)
	if err := r.Get(ctx, client.ObjectKeyFromObject(helm), helm); err != nil {
		if apierrors.IsNotFound(err) {
			result.helmReason = "HelmChartNotFound"
			result.helmMessage = "HelmChart is absent"
			return result, nil
		}
		return result, fmt.Errorf("observe HelmChart %s/%s: %w", HelmChartNamespace, app.Spec.Release.ReleaseName, err)
	}
	result.helm = helm
	if chartConditionTrue(helm, "Failed") {
		result.helmFailed = true
		result.helmReason = "HelmChartFailed"
		result.helmMessage = chartConditionMessage(helm, "Failed", "HelmChart reported failure")
		return result, nil
	}

	jobName, _, _ := unstructured.NestedString(helm.Object, "status", "jobName")
	if strings.TrimSpace(jobName) == "" {
		result.helmReason = "InstallerJobPending"
		result.helmMessage = "HelmChart has not reported an installer Job"
		return result, nil
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: helm.GetNamespace(), Name: strings.TrimSpace(jobName)}, job); err != nil {
		if apierrors.IsNotFound(err) {
			if app.Status.Phase == applicationv1.PhaseReady {
				result.helmReady = true
				result.completed++
				result.helmReason = "PreviouslyReady"
				result.helmMessage = "Installer Job is gone after a previously ready release"
				return result, nil
			}
			result.helmReason = "InstallerJobNotFound"
			result.helmMessage = "Helm installer Job is absent"
			return result, nil
		}
		return result, fmt.Errorf("observe Helm installer Job %s/%s: %w", helm.GetNamespace(), jobName, err)
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && condition.Type == batchv1.JobFailed {
			result.helmFailed = true
			result.helmReason = "InstallerJobFailed"
			result.helmMessage = firstNonEmpty(condition.Message, condition.Reason, "Helm installer Job failed")
			return result, nil
		}
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && condition.Type == batchv1.JobComplete {
			result.helmReady = true
			result.completed++
			result.helmReason = "InstallerJobComplete"
			result.helmMessage = "Helm installer Job completed"
			return result, nil
		}
	}
	result.helmReason = "InstallerJobPending"
	result.helmMessage = "Helm installer Job is pending"
	return result, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, app *applicationv1.OneKSApplication) (ctrl.Result, error) {
	status := baseStatus(app, r.ControllerVersion)
	status.Phase = applicationv1.PhaseDeleting
	status.Progress = applicationv1.ApplicationProgress{Total: applicationProgressTotal(app), Current: app.Spec.Release.ReleaseName}
	if isRootApplication(app) {
		status.Resources = deletingManagedResourceStatuses(app)
		if usesProtectedSecrets(app) {
			status.Resources = append(status.Resources, deletingProtectedSecretStatuses(app)...)
		}
	}
	clearLastError(&status)
	if err := r.updateStatus(ctx, app, status); err != nil {
		return ctrl.Result{}, err
	}

	var helm *unstructured.Unstructured
	var err error
	if ownsHelmLifecycle(app) {
		helm = helmChartObject(app.Spec.Release.ReleaseName)
		err = r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(helm), helm)
	}
	if helm != nil && err == nil && app.Spec.DeletionPolicy == applicationv1.DeletionPolicyDelete {
		if !ownershipMatches(app, helm) {
			conflict := (&OwnershipConflictError{Kind: "HelmChart", Namespace: helm.GetNamespace(), Name: helm.GetName()}).Error()
			return r.recordTerminal(ctx, app, "OwnershipConflict", conflict, true)
		}
		if deletionTimestamp := helm.GetDeletionTimestamp(); deletionTimestamp != nil && !deletionTimestamp.IsZero() {
			return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
		}
		if err := r.executePreUninstallActions(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
		ctrl.LoggerFrom(ctx).V(1).Info(
			"deleting Helm release",
			"release", app.Spec.Release.ReleaseName,
			"releaseNamespace", app.Spec.Release.TargetNamespace,
		)
		deleteErr := r.Delete(ctx, helm, deletePreconditions(helm)...)
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return ctrl.Result{}, fmt.Errorf("delete HelmChart %s/%s: %w", helm.GetNamespace(), helm.GetName(), deleteErr)
		}
		if deleteErr == nil {
			ctrl.LoggerFrom(ctx).Info(
				"Helm release deletion requested",
				"release", app.Spec.Release.ReleaseName,
				"releaseNamespace", app.Spec.Release.TargetNamespace,
			)
		}
		r.event(app, corev1.EventTypeNormal, "HelmChartDeleted", fmt.Sprintf("HelmChart %s/%s deletion requested", helm.GetNamespace(), helm.GetName()))
		return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get deleting HelmChart: %w", err)
	}

	if usesProtectedSecrets(app) {
		pending, protectedErr := r.reconcileDeleteProtectedSecrets(ctx, app)
		if protectedErr != nil {
			var conflict *OwnershipConflictError
			if errors.As(protectedErr, &conflict) {
				return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
			}
			return ctrl.Result{}, protectedErr
		}
		if pending {
			return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
		}
		inputPending, inputErr := r.reconcileDeleteSecretInput(ctx, app)
		if inputErr != nil {
			return ctrl.Result{}, inputErr
		}
		if inputPending {
			return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
		}
	}

	if isRootApplication(app) {
		deleted, deleteErr := r.reconcileDeleteManagedResources(ctx, app)
		if deleteErr != nil {
			var conflict *OwnershipConflictError
			if errors.As(deleteErr, &conflict) {
				return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
			}
			return ctrl.Result{}, deleteErr
		}
		if deleted {
			return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
		}
	}

	if app.Spec.ExecutionMode == applicationv1.ExecutionModeExecute && containsString(app.Finalizers, applicationv1.ApplicationFinalizer) {
		retry, err := r.releaseDependencies(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		if retry {
			return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
		}
	}

	if err := r.removeApplicationFinalizer(ctx, app); err != nil {
		return ctrl.Result{}, err
	}
	ctrl.LoggerFrom(ctx).Info("application finalization completed")
	return ctrl.Result{}, nil
}

func (r *Reconciler) executePreUninstallActions(ctx context.Context, app *applicationv1.OneKSApplication) error {
	if app.Spec.Role != applicationv1.ApplicationRoleDependency || app.Spec.Uninstall == nil {
		return nil
	}
	for index, action := range app.Spec.Uninstall.PreActions {
		target := &unstructured.Unstructured{}
		target.SetAPIVersion(action.Resource.APIVersion)
		target.SetKind(action.Resource.Kind)
		target.SetNamespace(action.Resource.Namespace)
		target.SetName(action.Resource.Name)
		if err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
			return fmt.Errorf("get pre-uninstall action %d target %s %s/%s: %w", index, action.Resource.Kind, action.Resource.Namespace, action.Resource.Name, err)
		}
		patch := client.RawPatch(types.MergePatchType, []byte(action.PatchJSON))
		ctrl.LoggerFrom(ctx).V(1).Info(
			"executing pre-uninstall action",
			"action", index, "type", action.Type,
			"apiVersion", action.Resource.APIVersion, "kind", action.Resource.Kind,
			"resourceNamespace", action.Resource.Namespace, "name", action.Resource.Name,
		)
		if err := r.Patch(ctx, target, patch); err != nil {
			return fmt.Errorf("patch pre-uninstall action %d target %s %s/%s: %w", index, action.Resource.Kind, action.Resource.Namespace, action.Resource.Name, err)
		}
		ctrl.LoggerFrom(ctx).Info(
			"pre-uninstall action applied",
			"action", index, "type", action.Type,
			"apiVersion", action.Resource.APIVersion, "kind", action.Resource.Kind,
			"resourceNamespace", action.Resource.Namespace, "name", action.Resource.Name,
		)
	}
	return nil
}

func (r *Reconciler) removeApplicationFinalizer(ctx context.Context, app *applicationv1.OneKSApplication) error {
	_, err := r.patchApplicationFinalizers(
		ctx, app, removeString(app.Finalizers, applicationv1.ApplicationFinalizer),
	)
	if err != nil {
		current := &applicationv1.OneKSApplication{}
		getErr := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(app), current)
		if apierrors.IsNotFound(getErr) {
			return nil
		}
		if getErr == nil && app.UID != "" && current.UID != "" && current.UID != app.UID {
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("remove application finalizer: %w (authoritative verification failed: %v)", err, getErr)
		}
		return fmt.Errorf("remove application finalizer: %w", err)
	}
	return nil
}

func (r *Reconciler) patchApplicationFinalizers(
	ctx context.Context,
	app *applicationv1.OneKSApplication,
	finalizers []string,
) (*applicationv1.OneKSApplication, error) {
	updated := app.DeepCopy()
	updated.Finalizers = append([]string(nil), finalizers...)
	patch := client.MergeFromWithOptions(
		app, client.MergeFromWithOptimisticLock{},
	)
	if err := r.Patch(ctx, updated, patch); err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *Reconciler) recordTerminal(ctx context.Context, app *applicationv1.OneKSApplication, reason, message string, ownershipConflict bool) (ctrl.Result, error) {
	status := baseStatus(app, r.ControllerVersion)
	status.Phase = applicationv1.PhaseFailed
	status.Progress = applicationv1.ApplicationProgress{Total: applicationProgressTotal(app)}
	setLastError(&status, reason, message)
	planCondition := metav1.ConditionTrue
	if !ownershipConflict && reason != "TargetNamespaceMissing" && reason != "InputSecretInvalid" &&
		reason != "ExternalDependencyUnusable" && reason != "ExternalSelectionInvalid" {
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
	conflictCondition := metav1.ConditionFalse
	if ownershipConflict {
		conflictCondition = metav1.ConditionTrue
	}
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
		ctrl.LoggerFrom(ctx).V(1).Info(
			"application status unchanged",
			"phase", status.Phase,
			"observedGeneration", status.ObservedGeneration,
		)
		return nil
	}
	previous := *app.Status.DeepCopy()
	updated := app.DeepCopy()
	updated.Status = status
	if err := r.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("update OneKSApplication status: %w", err)
	}
	app.Status = status
	app.ResourceVersion = updated.ResourceVersion
	logStatusTransitions(ctx, app, previous, status)
	return nil
}

func (r *Reconciler) SetupWithManager(manager ctrl.Manager) error {
	if err := manager.GetFieldIndexer().IndexField(context.Background(), &applicationv1.OneKSApplication{}, dependencyNameIndex, directDependencyNames); err != nil {
		return fmt.Errorf("index direct application dependencies: %w", err)
	}
	helm := helmChartObject("")
	return ctrl.NewControllerManagedBy(manager).
		For(&applicationv1.OneKSApplication{}).
		Watches(&applicationv1.OneKSApplication{}, handler.EnqueueRequestsFromMapFunc(r.requestsForDependencyConsumers)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.requestsForOwnedChild)).
		Watches(helm, handler.EnqueueRequestsFromMapFunc(r.requestsForOwnedChild)).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.requestsForJob)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		Complete(r)
}

func (r *Reconciler) requestsForDependencyConsumers(ctx context.Context, object client.Object) []ctrl.Request {
	if object.GetName() == "" {
		return nil
	}
	applications := &applicationv1.OneKSApplicationList{}
	if err := r.List(
		ctx, applications,
		client.InNamespace(applicationv1.ApplicationNamespace),
		client.MatchingFields{dependencyNameIndex: object.GetName()},
	); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(applications.Items))
	changedKey := client.ObjectKeyFromObject(object)
	for index := range applications.Items {
		consumer := &applications.Items[index]
		if client.ObjectKeyFromObject(consumer) == changedKey {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(consumer)})
	}
	return requests
}

func (r *Reconciler) requestsForOwnedChild(_ context.Context, object client.Object) []ctrl.Request {
	labels := object.GetLabels()
	name := labels[LabelApplicationName]
	namespace := labels[LabelApplicationNamespace]
	if labels[LabelManagedBy] != ManagedByValue || name == "" || namespace != applicationv1.ApplicationNamespace {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

func (r *Reconciler) requestsForJob(ctx context.Context, _ client.Object) []ctrl.Request {
	applications := &applicationv1.OneKSApplicationList{}
	if err := r.List(ctx, applications, client.InNamespace(applicationv1.ApplicationNamespace)); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(applications.Items))
	for index := range applications.Items {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&applications.Items[index])})
	}
	return requests
}

func chartConditionTrue(chart *unstructured.Unstructured, conditionType string) bool {
	conditions, found, _ := unstructured.NestedSlice(chart.Object, "status", "conditions")
	if !found {
		return false
	}
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if ok && condition["type"] == conditionType && condition["status"] == string(corev1.ConditionTrue) {
			return true
		}
	}
	return false
}

func chartConditionMessage(chart *unstructured.Unstructured, conditionType, fallback string) string {
	conditions, _, _ := unstructured.NestedSlice(chart.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["type"] != conditionType || condition["status"] != string(corev1.ConditionTrue) {
			continue
		}
		return firstNonEmpty(fmt.Sprint(condition["message"]), fmt.Sprint(condition["reason"]), fallback)
	}
	return fallback
}

func conditionText(status metav1.ConditionStatus, positive, negative string) string {
	if status == metav1.ConditionTrue {
		return positive
	}
	return firstNonEmpty(negative, "Pending")
}

func clearLastError(status *applicationv1.OneKSApplicationStatus) {
	status.LastError = &applicationv1.ApplicationError{}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func removeString(values []string, removed string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != removed {
			result = append(result, value)
		}
	}
	return result
}

func deletePreconditions(object client.Object) []client.DeleteOption {
	uid := object.GetUID()
	resourceVersion := object.GetResourceVersion()
	if uid == "" && resourceVersion == "" {
		return nil
	}
	preconditions := metav1.Preconditions{}
	if uid != "" {
		preconditions.UID = &uid
	}
	if resourceVersion != "" {
		preconditions.ResourceVersion = &resourceVersion
	}
	return []client.DeleteOption{client.Preconditions(preconditions)}
}

func (r *Reconciler) requeueDuration() time.Duration {
	if r.RequeueAfter > 0 {
		return r.RequeueAfter
	}
	return defaultRequeue
}

func (r *Reconciler) event(object runtime.Object, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(object, eventType, truncate(reason, 128), truncate(message, 512))
	}
}
