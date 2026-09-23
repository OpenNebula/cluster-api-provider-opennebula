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
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	HelmChartNamespace = "kube-system"
	ChartIDAnnotation  = "oneks.opennebula.io/chart-id"
)

var helmChartGVK = schema.GroupVersionKind{
	Group: "helm.cattle.io", Version: "v1", Kind: "HelmChart",
}

var kindPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)

func (r *Reconciler) preflightHelmOwnership(ctx context.Context, app *applicationv1.OneKSApplication) error {
	helm := helmChartObject(app.Spec.Release.ReleaseName)
	err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(helm), helm)
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
		if err := r.Create(ctx, desired, client.FieldOwner(applicationv1.FieldManager)); err != nil {
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
				result.helmState = componentObservation{ready: true, reason: "ExternalDependencyReady", message: detection.message}
				result.completed++
				return result, nil
			}
			reason, message := "ExternalDependencyUnusable", detection.message
			if selection == ExternalSelectionExternal {
				reason = "ExternalDependencyLost"
				message = "Previously selected external prerequisite is no longer usable: " + detection.message
			}
			result.helmState = componentObservation{failed: true, reason: reason, message: message}
			return result, nil
		}
	}
	base := result
	observed, err := r.observeManagedHelm(ctx, app, result, r.Client)
	if err != nil {
		return observed, err
	}
	if observed.helmState.failed && r.APIReader != nil {
		observed, err = r.observeManagedHelm(ctx, app, base, r.APIReader)
		if err != nil {
			return observed, fmt.Errorf("confirm Helm failure from API: %w", err)
		}
	}
	return observed, nil
}

func (r *Reconciler) observeManagedHelm(
	ctx context.Context,
	app *applicationv1.OneKSApplication,
	result observation,
	reader client.Reader,
) (observation, error) {
	helm := helmChartObject(app.Spec.Release.ReleaseName)
	if err := reader.Get(ctx, client.ObjectKeyFromObject(helm), helm); err != nil {
		if apierrors.IsNotFound(err) {
			result.helmState = componentObservation{reason: "HelmChartNotFound", message: "HelmChart is absent"}
			return result, nil
		}
		return result, fmt.Errorf("observe HelmChart %s/%s: %w", HelmChartNamespace, app.Spec.Release.ReleaseName, err)
	}
	result.helm = helm
	if failed, message := chartCondition(helm, "Failed", "HelmChart reported failure"); failed {
		result.helmState = componentObservation{failed: true, reason: "HelmChartFailed", message: message}
		return result, nil
	}

	jobName, _, _ := unstructured.NestedString(helm.Object, "status", "jobName")
	if strings.TrimSpace(jobName) == "" {
		result.helmState = componentObservation{reason: "InstallerJobPending", message: "HelmChart has not reported an installer Job"}
		return result, nil
	}
	job := &batchv1.Job{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: helm.GetNamespace(), Name: strings.TrimSpace(jobName)}, job); err != nil {
		if apierrors.IsNotFound(err) {
			if app.Status.Phase == applicationv1.PhaseReady {
				result.helmState = componentObservation{ready: true, reason: "PreviouslyReady", message: "Installer Job is gone after a previously ready release"}
				result.completed++
				return result, nil
			}
			result.helmState = componentObservation{reason: "InstallerJobNotFound", message: "Helm installer Job is absent"}
			return result, nil
		}
		return result, fmt.Errorf("observe Helm installer Job %s/%s: %w", helm.GetNamespace(), jobName, err)
	}
	completed := false
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type == batchv1.JobFailed {
			message := firstNonEmpty(condition.Message, condition.Reason, "Helm installer Job failed")
			result.helmState = componentObservation{failed: true, reason: "InstallerJobFailed", message: message}
			return result, nil
		}
		if condition.Type == batchv1.JobComplete {
			completed = true
		}
	}
	if completed {
		result.helmState = componentObservation{ready: true, reason: "InstallerJobComplete", message: "Helm installer Job completed"}
		result.completed++
		return result, nil
	}
	result.helmState = componentObservation{reason: "InstallerJobPending", message: "Helm installer Job is pending"}
	return result, nil
}

func chartCondition(chart *unstructured.Unstructured, conditionType, fallback string) (bool, string) {
	conditions, found, _ := unstructured.NestedSlice(chart.Object, "status", "conditions")
	if !found {
		return false, ""
	}
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if ok && condition["type"] == conditionType && condition["status"] == string(corev1.ConditionTrue) {
			return true, firstNonEmpty(fmt.Sprint(condition["message"]), fmt.Sprint(condition["reason"]), fallback)
		}
	}
	return false, ""
}

func validateUninstall(uninstall applicationv1.UninstallSpec, path string) *PlanError {
	if len(uninstall.PreActions) == 0 && uninstall.CleanupJob == nil {
		return invalid("InvalidUninstall", "%s requires preActions or cleanupJob", path)
	}
	if uninstall.CleanupJob != nil {
		if err := validateCleanupJob(*uninstall.CleanupJob, path+".cleanupJob"); err != nil {
			return err
		}
	}
	for index, action := range uninstall.PreActions {
		actionPath := fmt.Sprintf("%s.preActions[%d]", path, index)
		resource := action.Resource
		parts := strings.Split(resource.APIVersion, "/")
		validAPIVersion := len(parts) == 1 && len(validation.IsDNS1123Label(parts[0])) == 0 ||
			len(parts) == 2 && len(validation.IsDNS1123Subdomain(parts[0])) == 0 && len(validation.IsDNS1123Label(parts[1])) == 0
		if !validAPIVersion || !kindPattern.MatchString(resource.Kind) {
			return invalid("InvalidUninstallResourceIdentity", "%s.resource must contain a valid apiVersion and kind", actionPath)
		}
		if placeholderPattern.MatchString(resource.APIVersion) || placeholderPattern.MatchString(resource.Kind) ||
			placeholderPattern.MatchString(resource.Namespace) || placeholderPattern.MatchString(resource.Name) ||
			placeholderPattern.MatchString(action.PatchJSON) {
			return invalid("UnresolvedPlaceholder", "%s contains an unresolved placeholder", actionPath)
		}
		patch := map[string]any{}
		if err := json.Unmarshal([]byte(action.PatchJSON), &patch); err != nil || patch == nil || len(patch) == 0 {
			return invalid("InvalidUninstallPatch", "%s.patchJSON must be a non-empty JSON object", actionPath)
		}
	}
	return nil
}

func (r *Reconciler) reconcileDeleteHelmChart(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	if !ownsHelmLifecycle(app) {
		return false, nil
	}
	helm := helmChartObject(app.Spec.Release.ReleaseName)
	if err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(helm), helm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get deleting HelmChart: %w", err)
	}
	if app.Spec.DeletionPolicy != applicationv1.DeletionPolicyDelete {
		return false, nil
	}
	if !ownershipMatches(app, helm) {
		return false, &OwnershipConflictError{Kind: "HelmChart", Namespace: helm.GetNamespace(), Name: helm.GetName()}
	}
	if deletionTimestamp := helm.GetDeletionTimestamp(); deletionTimestamp != nil && !deletionTimestamp.IsZero() {
		return true, nil
	}
	if err := r.executePreUninstallActions(ctx, app); err != nil {
		return false, err
	}
	deleteErr := r.Delete(ctx, helm, deletePreconditions(helm)...)
	if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
		return false, fmt.Errorf("delete HelmChart %s/%s: %w", helm.GetNamespace(), helm.GetName(), deleteErr)
	}
	if deleteErr == nil {
		ctrl.LoggerFrom(ctx).Info(
			"Helm release deletion requested",
			"release", app.Spec.Release.ReleaseName,
			"releaseNamespace", app.Spec.Release.TargetNamespace,
		)
	}
	r.event(app, corev1.EventTypeNormal, "HelmChartDeleted", fmt.Sprintf("HelmChart %s/%s deletion requested", helm.GetNamespace(), helm.GetName()))
	return true, nil
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

func (r *Reconciler) requestsForJob(ctx context.Context, object client.Object) []ctrl.Request {
	if requests := r.requestsForOwnedChild(ctx, object); len(requests) != 0 {
		return requests
	}
	charts := &unstructured.UnstructuredList{}
	charts.SetGroupVersionKind(schema.GroupVersionKind{
		Group: helmChartGVK.Group, Version: helmChartGVK.Version, Kind: helmChartGVK.Kind + "List",
	})
	if err := r.List(ctx, charts, client.InNamespace(HelmChartNamespace)); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "unable to map Helm Job to an application", "job", object.GetName())
		return nil
	}
	for index := range charts.Items {
		chart := &charts.Items[index]
		jobName, _, _ := unstructured.NestedString(chart.Object, "status", "jobName")
		if strings.TrimSpace(jobName) == object.GetName() {
			return r.requestsForOwnedChild(ctx, chart)
		}
	}
	return nil
}
