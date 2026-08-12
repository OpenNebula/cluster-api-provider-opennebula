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

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
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
}

type observation struct {
	resources    []applicationv1.ResourceStatus
	helm         *unstructured.Unstructured
	helmReady    bool
	helmFailed   bool
	helmReason   string
	helmMessage  string
	current      string
	completed    int32
	allResources bool
}

func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	app := &applicationv1.OneKSApplication{}
	if err := r.Get(ctx, request.NamespacedName, app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

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

	if !deleting && !app.Spec.Release.CreateNamespace {
		if err := r.checkTargetNamespace(ctx, app.Spec.Release.TargetNamespace); err != nil {
			if apierrors.IsNotFound(err) {
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
			return ctrl.Result{}, fmt.Errorf("check target namespace: %w", err)
		}
	}

	if deleting {
		if err := r.preflightOwnership(ctx, app, true); err != nil {
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
		if err := r.preflightOwnership(ctx, app, false); err != nil {
			var conflict *OwnershipConflictError
			if errors.As(err, &conflict) {
				return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
			}
			return ctrl.Result{}, err
		}
		return r.reconcileStatus(ctx, app, true, dependencies)
	}

	if err := r.preflightOwnership(ctx, app, false); err != nil {
		var conflict *OwnershipConflictError
		if errors.As(err, &conflict) {
			return r.recordTerminal(ctx, app, "OwnershipConflict", conflict.Error(), true)
		}
		return ctrl.Result{}, err
	}

	if !containsString(app.Finalizers, applicationv1.ApplicationFinalizer) {
		updated := app.DeepCopy()
		updated.Finalizers = append(updated.Finalizers, applicationv1.ApplicationFinalizer)
		if err := r.Update(ctx, updated); err != nil {
			return ctrl.Result{}, fmt.Errorf("add application finalizer: %w", err)
		}
		r.event(updated, corev1.EventTypeNormal, "FinalizerAdded", "Application cleanup finalizer added")
		return ctrl.Result{Requeue: true}, nil
	}

	if app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha2 && app.Spec.Role == applicationv1.ApplicationRoleRoot {
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

	resourcesReady, err := r.reconcileConfigMaps(ctx, app)
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

func (r *Reconciler) preflightOwnership(ctx context.Context, app *applicationv1.OneKSApplication, deleting bool) error {
	reader := r.authoritativeReader()
	for _, resource := range app.Spec.Resources {
		if deleting && resource.DeletionPolicy == applicationv1.DeletionPolicyRetain {
			continue
		}
		object := &corev1.ConfigMap{}
		err := reader.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, object)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("preflight ConfigMap %s/%s: %w", resource.Namespace, resource.Name, err)
		}
		if !ownershipMatches(app, object) {
			return &OwnershipConflictError{Kind: "ConfigMap", Namespace: resource.Namespace, Name: resource.Name}
		}
	}

	if deleting && app.Spec.DeletionPolicy == applicationv1.DeletionPolicyRetain {
		return nil
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

func (r *Reconciler) reconcileConfigMaps(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	for _, resource := range app.Spec.Resources {
		desired := desiredConfigMap(app, resource)
		current := &corev1.ConfigMap{}
		err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(desired), current)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, desired, client.FieldOwner(applicationv1.FieldManager)); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return false, &OwnershipConflictError{Kind: "ConfigMap", Namespace: desired.Namespace, Name: desired.Name}
				}
				return false, fmt.Errorf("create ConfigMap %s/%s: %w", desired.Namespace, desired.Name, err)
			}
			r.event(app, corev1.EventTypeNormal, "ResourceCreated", fmt.Sprintf("ConfigMap %s/%s created", desired.Namespace, desired.Name))
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("get ConfigMap %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		if !ownershipMatches(app, current) {
			return false, &OwnershipConflictError{Kind: "ConfigMap", Namespace: desired.Namespace, Name: desired.Name}
		}
		if configMapNeedsApply(current, desired) {
			// SSA honors resourceVersion as an optimistic precondition.
			desired.ResourceVersion = current.ResourceVersion
			if err := r.Patch(ctx, desired, client.Apply, client.FieldOwner(applicationv1.FieldManager)); err != nil {
				return false, fmt.Errorf("apply ConfigMap %s/%s: %w", desired.Namespace, desired.Name, err)
			}
			r.event(app, corev1.EventTypeNormal, "ResourceApplied", fmt.Sprintf("ConfigMap %s/%s applied", desired.Namespace, desired.Name))
		}
	}
	return true, nil
}

func (r *Reconciler) reconcileHelmChart(ctx context.Context, app *applicationv1.OneKSApplication) error {
	desired := desiredHelmChart(app)
	current := helmChartObject(desired.GetName())
	err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired, client.FieldOwner(applicationv1.FieldManager)); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return &OwnershipConflictError{Kind: "HelmChart", Namespace: desired.GetNamespace(), Name: desired.GetName()}
			}
			return fmt.Errorf("create HelmChart %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)
		}
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
		if err := r.Patch(ctx, desired, client.Apply, client.FieldOwner(applicationv1.FieldManager)); err != nil {
			return fmt.Errorf("apply HelmChart %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)
		}
		r.event(app, corev1.EventTypeNormal, "HelmChartApplied", fmt.Sprintf("HelmChart %s/%s applied", desired.GetNamespace(), desired.GetName()))
	}
	return nil
}

func desiredConfigMap(app *applicationv1.OneKSApplication, resource applicationv1.ResourceSpec) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: resource.Namespace, Name: resource.Name,
			Labels: ownershipLabels(app),
		},
		Data: copyStringMap(resource.Data),
	}
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

func configMapNeedsApply(current, desired *corev1.ConfigMap) bool {
	return !reflect.DeepEqual(current.Data, desired.Data) || !ownedLabelsEqual(current.Labels, desired.Labels)
}

func helmChartNeedsApply(current, desired *unstructured.Unstructured) bool {
	currentSpec, _, _ := unstructured.NestedMap(current.Object, "spec")
	desiredSpec, _, _ := unstructured.NestedMap(desired.Object, "spec")
	return !reflect.DeepEqual(currentSpec, desiredSpec) ||
		!ownedLabelsEqual(current.GetLabels(), desired.GetLabels()) ||
		current.GetAnnotations()[ChartIDAnnotation] != desired.GetAnnotations()[ChartIDAnnotation]
}

func ownedLabelsEqual(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func (r *Reconciler) reconcileStatus(ctx context.Context, app *applicationv1.OneKSApplication, observeOnly bool, dependencies dependencyObservation) (ctrl.Result, error) {
	observed, err := r.observe(ctx, app)
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
	if observed.allResources {
		resourceCondition = metav1.ConditionTrue
	}
	setCondition(&status, app.Generation, ConditionResourcesReady, resourceCondition, conditionReason(resourceCondition, "ResourcesReady", "ResourcesPending"), conditionMessage(resourceCondition, "All ConfigMaps are ready", "ConfigMaps are not ready"))
	helmCondition := metav1.ConditionFalse
	if observed.helmReady {
		helmCondition = metav1.ConditionTrue
	}
	setCondition(&status, app.Generation, ConditionHelmReleaseReady, helmCondition, conditionReason(helmCondition, "HelmReleaseReady", observed.helmReason), conditionMessage(helmCondition, "Helm release is ready", observed.helmMessage))
	setCondition(&status, app.Generation, ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflict", "Managed children have exact OneKS ownership")

	ready := dependencies.ready && observed.allResources && observed.helmReady
	readyCondition := metav1.ConditionFalse
	if ready {
		readyCondition = metav1.ConditionTrue
	}
	readyReason := "ApplicationProgressing"
	readyMessage := "Application installation is in progress"
	if dependencies.enabled && !dependencies.ready {
		readyReason = dependencies.reason
		readyMessage = dependencies.message
	}
	setCondition(&status, app.Generation, ConditionReady, readyCondition, conditionReason(readyCondition, "ApplicationReady", readyReason), conditionMessage(readyCondition, "Application resources and Helm release are ready", readyMessage))
	clearLastError(&status)

	if observeOnly {
		status.Phase = applicationv1.PhaseObserving
	} else if dependencies.failed || dependencies.conflict {
		status.Phase = applicationv1.PhaseFailed
		setLastError(&status, dependencies.reason, dependencies.message)
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

func (r *Reconciler) observe(ctx context.Context, app *applicationv1.OneKSApplication) (observation, error) {
	result := observation{allResources: true, current: app.Spec.Release.ReleaseName}
	for _, resource := range app.Spec.Resources {
		object := &corev1.ConfigMap{}
		err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, object)
		status := applicationv1.ResourceStatus{ID: resource.ID, Phase: "Pending", Reason: "NotFound", Message: "ConfigMap is absent"}
		if err == nil {
			status.ResourceVersion = object.ResourceVersion
			if reflect.DeepEqual(object.Data, resource.Data) {
				status.Phase = "Ready"
				status.Reason = "Applied"
				status.Message = "ConfigMap is ready"
				result.completed++
			} else {
				status.Reason = "DataDrift"
				status.Message = "ConfigMap data differs from the compiled plan"
				result.allResources = false
				if result.current == app.Spec.Release.ReleaseName {
					result.current = resource.ID
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return result, fmt.Errorf("observe ConfigMap %s/%s: %w", resource.Namespace, resource.Name, err)
		} else {
			result.allResources = false
			if result.current == app.Spec.Release.ReleaseName {
				result.current = resource.ID
			}
		}
		result.resources = append(result.resources, status)
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
	clearLastError(&status)
	if err := r.updateStatus(ctx, app, status); err != nil {
		return ctrl.Result{}, err
	}

	helm := helmChartObject(app.Spec.Release.ReleaseName)
	err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(helm), helm)
	if err == nil && app.Spec.DeletionPolicy == applicationv1.DeletionPolicyDelete {
		if !ownershipMatches(app, helm) {
			conflict := (&OwnershipConflictError{Kind: "HelmChart", Namespace: helm.GetNamespace(), Name: helm.GetName()}).Error()
			return r.recordTerminal(ctx, app, "OwnershipConflict", conflict, true)
		}
		if err := r.Delete(ctx, helm, deletePreconditions(helm)...); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete HelmChart %s/%s: %w", helm.GetNamespace(), helm.GetName(), err)
		}
		r.event(app, corev1.EventTypeNormal, "HelmChartDeleted", fmt.Sprintf("HelmChart %s/%s deletion requested", helm.GetNamespace(), helm.GetName()))
		return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get deleting HelmChart: %w", err)
	}

	for index := len(app.Spec.Resources) - 1; index >= 0; index-- {
		resource := app.Spec.Resources[index]
		object := &corev1.ConfigMap{}
		err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, object)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get deleting ConfigMap %s/%s: %w", resource.Namespace, resource.Name, err)
		}
		if resource.DeletionPolicy == applicationv1.DeletionPolicyRetain {
			continue
		}
		if !ownershipMatches(app, object) {
			conflict := (&OwnershipConflictError{Kind: "ConfigMap", Namespace: object.Namespace, Name: object.Name}).Error()
			return r.recordTerminal(ctx, app, "OwnershipConflict", conflict, true)
		}
		if err := r.Delete(ctx, object, deletePreconditions(object)...); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete ConfigMap %s/%s: %w", resource.Namespace, resource.Name, err)
		}
		r.event(app, corev1.EventTypeNormal, "ResourceDeleted", fmt.Sprintf("ConfigMap %s/%s deletion requested", resource.Namespace, resource.Name))
		return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
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

	updated := app.DeepCopy()
	updated.Finalizers = removeString(updated.Finalizers, applicationv1.ApplicationFinalizer)
	if err := r.Update(ctx, updated); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove application finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) recordTerminal(ctx context.Context, app *applicationv1.OneKSApplication, reason, message string, ownershipConflict bool) (ctrl.Result, error) {
	status := baseStatus(app, r.ControllerVersion)
	status.Phase = applicationv1.PhaseFailed
	status.Progress = applicationv1.ApplicationProgress{Total: applicationProgressTotal(app)}
	setLastError(&status, reason, message)
	planCondition := metav1.ConditionTrue
	if !ownershipConflict && reason != "TargetNamespaceMissing" {
		planCondition = metav1.ConditionFalse
	}
	setCondition(&status, app.Generation, ConditionPlanValid, planCondition, reason, message)
	if app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha2 {
		if len(app.Spec.Dependencies) == 0 {
			setCondition(&status, app.Generation, ConditionDependenciesReady, metav1.ConditionTrue, "NoDependencies", "Application has no direct dependencies")
		} else {
			setCondition(&status, app.Generation, ConditionDependenciesReady, metav1.ConditionFalse, "DependenciesPending", "Direct dependencies have not been evaluated")
		}
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
		return nil
	}
	updated := app.DeepCopy()
	updated.Status = status
	if err := r.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("update OneKSApplication status: %w", err)
	}
	app.Status = status
	app.ResourceVersion = updated.ResourceVersion
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

func conditionReason(status metav1.ConditionStatus, positive, negative string) string {
	if status == metav1.ConditionTrue {
		return positive
	}
	return firstNonEmpty(negative, "Pending")
}

func conditionMessage(status metav1.ConditionStatus, positive, negative string) string {
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

func copyStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
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
