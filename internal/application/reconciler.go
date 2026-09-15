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
	"time"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func isRootApplication(app *applicationv1.OneKSApplication) bool {
	return app.Spec.Role == applicationv1.ApplicationRoleRoot
}

func contextWithApplicationLogger(ctx context.Context, app *applicationv1.OneKSApplication) context.Context {
	logger := ctrl.LoggerFrom(ctx).WithValues(
		"application", app.Name,
		"namespace", app.Namespace,
		"generation", app.Generation,
		"planVersion", app.Spec.PlanVersion,
		"role", app.Spec.Role,
		"releaseName", app.Spec.Release.ReleaseName,
		"targetNamespace", app.Spec.Release.TargetNamespace,
		"createNamespace", app.Spec.Release.CreateNamespace,
	)
	return logr.NewContext(ctx, logger)
}

func (r *Reconciler) patchApplicationFinalizers(
	ctx context.Context,
	app *applicationv1.OneKSApplication,
	finalizers []string,
) (*applicationv1.OneKSApplication, error) {
	updated := app.DeepCopy()
	updated.Finalizers = append([]string(nil), finalizers...)
	patch := client.MergeFromWithOptions(app, client.MergeFromWithOptimisticLock{})
	if err := r.Patch(ctx, updated, patch); err != nil {
		return nil, err
	}
	return updated, nil
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
	if !app.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(app, applicationv1.ApplicationFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.preflightDeleteOwnership(ctx, app); err != nil {
			return r.handleOwnershipError(ctx, app, err)
		}
		return r.reconcileDelete(ctx, app)
	}
	if validationError := validatePlan(app, r.ClusterID); validationError != nil {
		return r.recordFailure(ctx, app, validationError.Reason, validationError.Message, failureInvalidPlan)
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
		return r.handleExecutionError(ctx, app, err)
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
	external, err := r.resolveExternalLifecycle(ctx, app)
	if err != nil {
		return ctrl.Result{}, err
	}
	if external.failureReason != "" {
		result, statusErr := r.recordFailure(ctx, app, external.failureReason, external.failureMessage, failureExecution)
		if statusErr == nil {
			result.RequeueAfter = r.requeueDuration()
		}
		return result, statusErr
	}

	if external.mode != ExternalSelectionExternal && external.selection != ExternalSelectionExternal {
		if err := r.preflightInstallOwnership(ctx, app, managedAPIsMayBeUnavailable); err != nil {
			return r.handleOwnershipError(ctx, app, err)
		}
	}

	if external.selection != "" {
		updated, err := r.patchExternalSelection(ctx, app, external.selection)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("persist external dependency selection: %w", err)
		}
		ctrl.LoggerFrom(ctx).Info("external dependency lifecycle selected", "selection", external.selection)
		r.event(updated, corev1.EventTypeNormal, "ExternalDependencySelected", fmt.Sprintf("Dependency selected %s lifecycle", external.selection))
		return ctrl.Result{Requeue: true}, nil
	}

	dependencies, err := r.reconcileDependencies(ctx, app)
	if err != nil {
		return ctrl.Result{}, err
	}
	if dependencies.stop || !dependencies.ready || external.mode == ExternalSelectionExternal {
		return r.reconcileStatus(ctx, app, dependencies)
	}
	if isRootApplication(app) {
		if err := r.preflightInstallOwnership(ctx, app, managedAPIsRequired); err != nil {
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
		protectedApplied, err = r.applyProtectedSecrets(ctx, app)
		if err != nil {
			return r.handleExecutionError(ctx, app, err)
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

func (r *Reconciler) handleExecutionError(ctx context.Context, app *applicationv1.OneKSApplication, err error) (ctrl.Result, error) {
	var conflict *OwnershipConflictError
	if errors.As(err, &conflict) {
		return r.recordFailure(ctx, app, "OwnershipConflict", conflict.Error(), failureOwnership)
	}
	var invalidInput *InputSecretValidationError
	if errors.As(err, &invalidInput) {
		return r.recordFailure(ctx, app, "InputSecretInvalid", invalidInput.Error(), failureExecution)
	}
	return ctrl.Result{}, err
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
