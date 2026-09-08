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
	"strings"
	"time"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const defaultRequeue = 15 * time.Second

type Reconciler struct {
	client.Client
	APIReader    client.Reader
	Recorder     record.EventRecorder
	ClusterID    string
	RequeueAfter time.Duration
	DNSLookup    func(context.Context, string) ([]string, error)
	Now          func() time.Time
}

func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, reconcileErr error) {
	app := &applicationv1.OneKSApplication{}
	if err := r.Get(ctx, request.NamespacedName, app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctx = contextWithApplicationLogger(ctx, app)
	ctrl.LoggerFrom(ctx).V(1).Info(
		"application reconciliation started",
		"observedGeneration", app.Status.ObservedGeneration,
		"phase", app.Status.Phase,
	)
	defer func() {
		ctrl.LoggerFrom(ctx).V(1).Info(
			"application reconciliation completed",
			"state", app.Status.Phase,
			"requeue", result.Requeue,
			"requeueAfter", result.RequeueAfter,
			"success", reconcileErr == nil,
		)
	}()
	deleting := !app.DeletionTimestamp.IsZero()
	hasCleanupFinalizer := controllerutil.ContainsFinalizer(app, applicationv1.ApplicationFinalizer)
	var validationError *PlanError
	if deleting && hasCleanupFinalizer {
		validationError = ValidateDeletionPlan(app)
	} else {
		validationError = ValidatePlan(app, ValidationConfig{ClusterID: r.ClusterID})
	}
	if validationError != nil {
		return r.recordFailure(ctx, app, validationError.Reason, validationError.Message, failureInvalidPlan)
	}
	if deleting {
		if err := r.preflightOwnership(ctx, app, true, managedAPIsRequired); err != nil {
			return r.handleOwnershipError(ctx, app, err)
		}
		return r.reconcileDelete(ctx, app)
	}
	return r.reconcileNormal(ctx, app)
}

func (r *Reconciler) reconcileNormal(ctx context.Context, app *applicationv1.OneKSApplication) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(app, applicationv1.ApplicationFinalizer) {
		withFinalizer := app.DeepCopy()
		controllerutil.AddFinalizer(withFinalizer, applicationv1.ApplicationFinalizer)
		updated, err := r.patchApplicationFinalizers(
			ctx, app, withFinalizer.Finalizers,
		)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("add application finalizer: %w", err)
		}
		ctrl.LoggerFrom(ctx).Info("application finalizer acquired before input binding")
		r.event(updated, corev1.EventTypeNormal, "FinalizerAdded", "Application cleanup finalizer added")
		return ctrl.Result{Requeue: true}, nil
	}
	bound, err := r.bindSecretInput(ctx, app)
	if err != nil {
		var invalidInput *InputSecretValidationError
		if errors.As(err, &invalidInput) {
			return r.recordFailure(ctx, app, "InputSecretInvalid", invalidInput.Error(), failureExecution)
		}
		return ctrl.Result{}, err
	}
	if bound {
		return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
	}

	if !app.Spec.Release.CreateNamespace {
		err := r.checkTargetNamespace(ctx, app.Spec.Release.TargetNamespace)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("check target namespace: %w", err)
		}
		if apierrors.IsNotFound(err) && !managesTargetNamespace(app) {
			result, statusErr := r.recordFailure(
				ctx, app, "TargetNamespaceMissing",
				fmt.Sprintf("target namespace %s is missing", app.Spec.Release.TargetNamespace), failureExecution,
			)
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			result.RequeueAfter = r.requeueDuration()
			return result, nil
		}
	}

	return r.reconcileExecute(ctx, app)
}

func (r *Reconciler) reconcileExecute(ctx context.Context, app *applicationv1.OneKSApplication) (ctrl.Result, error) {
	externalMode := ""
	externalSelectionToPersist := ""
	if usesExternalDetection(app) {
		var selectionErr error
		externalMode, selectionErr = externalSelection(app)
		if selectionErr != nil {
			result, statusErr := r.recordFailure(ctx, app, "ExternalSelectionInvalid", selectionErr.Error(), failureExecution)
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
				result, statusErr := r.recordFailure(ctx, app, "ExternalDependencyUnusable", detection.message, failureExecution)
				if statusErr == nil {
					result.RequeueAfter = r.requeueDuration()
				}
				return result, statusErr
			}
		}
	}

	if externalMode != ExternalSelectionExternal && externalSelectionToPersist != ExternalSelectionExternal {
		if err := r.preflightOwnership(ctx, app, false, managedAPIsMayBeUnavailable); err != nil {
			return r.handleOwnershipError(ctx, app, err)
		}
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
		materialized, err := r.materializeRootDependencies(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		if materialized.terminating != "" {
			dependencies := dependencyObservation{
				current: materialized.terminating,
				reason:  "DependencyTerminating", message: fmt.Sprintf("Dependency application %s is terminating", materialized.terminating),
			}
			return r.reconcileStatus(ctx, app, dependencies)
		}
		if materialized.conflict != nil {
			dependencies := dependencyObservation{
				terminal: true,
				reason:   "DependencyConflict", message: materialized.conflict.Error(), current: materialized.conflict.Name,
			}
			r.event(app, corev1.EventTypeWarning, dependencies.reason, dependencies.message)
			return r.reconcileStatus(ctx, app, dependencies)
		}
		if materialized.raced {
			dependencies, observeErr := r.observeDependencies(ctx, app)
			if observeErr != nil {
				return ctrl.Result{}, observeErr
			}
			return r.reconcileStatus(ctx, app, dependencies)
		}
	}

	dependencies, err := r.observeDependencies(ctx, app)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !dependencies.ready {
		return r.reconcileStatus(ctx, app, dependencies)
	}
	if externalMode == ExternalSelectionExternal {
		return r.reconcileStatus(ctx, app, dependencies)
	}
	if isRootApplication(app) {
		if err := r.preflightOwnership(ctx, app, false, managedAPIsRequired); err != nil {
			return r.handleOwnershipError(ctx, app, err)
		}
	}

	observed := observation{allResources: true, current: app.Spec.Release.ReleaseName}
	if isRootApplication(app) {
		applied, err := r.applyManagedResources(ctx, app)
		if err != nil {
			return r.handleOwnershipError(ctx, app, err)
		}
		if !applied {
			return r.reconcileStatus(ctx, app, dependencies)
		}
		observed, err = r.observeManagedResources(ctx, app, true)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	protectedApplied := true
	if observed.allResources && usesProtectedSecrets(app) {
		var protectedErr error
		protectedApplied, protectedErr = r.applyProtectedSecrets(ctx, app)
		if protectedErr != nil {
			var conflict *OwnershipConflictError
			if errors.As(protectedErr, &conflict) {
				return r.handleOwnershipError(ctx, app, protectedErr)
			}
			var invalidInput *InputSecretValidationError
			if errors.As(protectedErr, &invalidInput) {
				return r.recordFailure(ctx, app, "InputSecretInvalid", invalidInput.Error(), failureExecution)
			}
			return ctrl.Result{}, protectedErr
		}
	}
	observed, err = r.observeProtected(ctx, app, observed)
	if err != nil {
		return ctrl.Result{}, err
	}
	if observed.allResources && protectedApplied {
		if err := r.reconcileHelmChart(ctx, app); err != nil {
			return r.handleOwnershipError(ctx, app, err)
		}
	}
	observed, err = r.observeHelm(ctx, app, observed)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.recordObservedStatus(ctx, app, dependencies, observed)
}

func (r *Reconciler) handleOwnershipError(
	ctx context.Context,
	app *applicationv1.OneKSApplication,
	err error,
) (ctrl.Result, error) {
	var conflict *OwnershipConflictError
	if errors.As(err, &conflict) {
		return r.recordFailure(ctx, app, "OwnershipConflict", conflict.Error(), failureOwnership)
	}
	return ctrl.Result{}, err
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

func (r *Reconciler) reconcileDelete(ctx context.Context, app *applicationv1.OneKSApplication) (ctrl.Result, error) {
	status := baseStatus(app)
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

	pending, err := r.reconcileDeleteHelmChart(ctx, app)
	if err != nil {
		return r.handleOwnershipError(ctx, app, err)
	}
	if pending {
		return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
	}

	if usesProtectedSecrets(app) {
		pending, protectedErr := r.reconcileDeleteProtectedSecrets(ctx, app)
		if protectedErr != nil {
			return r.handleOwnershipError(ctx, app, protectedErr)
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
			return r.handleOwnershipError(ctx, app, deleteErr)
		}
		if deleted {
			return ctrl.Result{RequeueAfter: r.requeueDuration()}, nil
		}
	}

	if controllerutil.ContainsFinalizer(app, applicationv1.ApplicationFinalizer) {
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

func (r *Reconciler) removeApplicationFinalizer(ctx context.Context, app *applicationv1.OneKSApplication) error {
	updated := app.DeepCopy()
	controllerutil.RemoveFinalizer(updated, applicationv1.ApplicationFinalizer)
	_, err := r.patchApplicationFinalizers(
		ctx, app, updated.Finalizers,
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" && value != "<nil>" {
			return value
		}
	}
	return ""
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
