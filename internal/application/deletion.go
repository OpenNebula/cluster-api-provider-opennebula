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

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type deleteStep func(context.Context, *applicationv1.OneKSApplication) (bool, error)

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

func deletingResourceStatus(id string, policy applicationv1.DeletionPolicy, kind string) applicationv1.ResourceStatus {
	if policy == applicationv1.DeletionPolicyRetain {
		return applicationv1.ResourceStatus{
			ID: id, Phase: "Retained", Reason: "RetainPolicy", Message: kind + " is retained by policy",
		}
	}
	return applicationv1.ResourceStatus{
		ID: id, Phase: "Deleting", Reason: "DeletionInProgress", Message: kind + " deletion is in progress",
	}
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
	if err := r.updateStatus(ctx, app, status); err != nil {
		return ctrl.Result{}, err
	}

	steps := []deleteStep{r.reconcileDeleteCleanupJob, r.reconcileDeleteHelmChart}
	if usesProtectedSecrets(app) {
		steps = append(steps, r.reconcileDeleteProtectedSecrets, r.reconcileDeleteSecretInput)
	}
	if isRootApplication(app) {
		steps = append(steps, r.reconcileDeleteManagedResources)
	}
	if controllerutil.ContainsFinalizer(app, applicationv1.ApplicationFinalizer) {
		steps = append(steps, r.releaseDependencies)
	}
	for _, step := range steps {
		pending, err := step(ctx, app)
		if err != nil {
			return r.handleOwnershipError(ctx, app, err)
		}
		if pending {
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
	_, err := r.patchApplicationFinalizers(ctx, app, updated.Finalizers)
	if err == nil {
		return nil
	}

	current := &applicationv1.OneKSApplication{}
	getErr := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(app), current)
	if apierrors.IsNotFound(getErr) || (getErr == nil && app.UID != "" && current.UID != "" && current.UID != app.UID) {
		return nil
	}
	if getErr != nil {
		return fmt.Errorf("remove application finalizer: %w (authoritative verification failed: %v)", err, getErr)
	}
	return fmt.Errorf("remove application finalizer: %w", err)
}
