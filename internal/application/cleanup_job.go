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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const cleanupJobAnnotation = "applications.oneks.opennebula.io/cleanup-job"

type cleanupJobState struct {
	Digest    string `json:"digest"`
	Completed bool   `json:"completed,omitempty"`
}

func validateCleanupJob(spec applicationv1.CleanupJobSpec, path string) *PlanError {
	if spec.Image == "" || len(spec.Image) > 512 || strings.IndexFunc(spec.Image, unicode.IsSpace) >= 0 || placeholderPattern.MatchString(spec.Image) || len(validation.IsDNS1123Subdomain(spec.ServiceAccountName)) != 0 {
		return invalid("InvalidCleanupJob", "%s requires an image and an existing kube-system service account", path)
	}
	if len(spec.Command) == 0 || len(spec.Command) > 8 || spec.Command[0] == "" || len(spec.Args) > 16 || len(spec.Env) > 32 || spec.TimeoutSeconds != 0 && (spec.TimeoutSeconds < 60 || spec.TimeoutSeconds > 7200) {
		return invalid("InvalidCleanupJob", "%s command, argument, environment or timeout bounds are invalid", path)
	}
	for _, values := range []struct {
		items []string
		limit int
	}{{spec.Command, 256}, {spec.Args, 32768}} {
		for _, value := range values.items {
			if len(value) > values.limit || placeholderPattern.MatchString(value) {
				return invalid("InvalidCleanupJob", "%s contains an oversized command/argument or unresolved placeholder", path)
			}
		}
	}
	names := map[string]bool{}
	for _, env := range spec.Env {
		if len(validation.IsEnvVarName(env.Name)) != 0 || len(env.Name) > 253 || len(env.Value) > 2048 || names[env.Name] || strings.HasPrefix(env.Name, "ONEKS_APPLICATION_") || placeholderPattern.MatchString(env.Value) {
			return invalid("InvalidCleanupJob", "%s contains an invalid, duplicate or reserved environment variable", path)
		}
		names[env.Name] = true
	}
	return nil
}

func cleanupJobDigest(spec *applicationv1.CleanupJobSpec) string {
	data, _ := json.Marshal(spec)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func cleanupJob(app *applicationv1.OneKSApplication) *batchv1.Job {
	spec := app.Spec.Uninstall.CleanupJob
	digest := sha256.Sum256([]byte(string(app.UID) + "/" + cleanupJobDigest(spec)))
	deadline := spec.TimeoutSeconds
	if deadline == 0 {
		deadline = 1800
	}
	backoff := int32(2)
	env := make([]corev1.EnvVar, 0, len(spec.Env)+3)
	for _, variable := range spec.Env {
		env = append(env, corev1.EnvVar{Name: variable.Name, Value: variable.Value})
	}
	env = append(env,
		corev1.EnvVar{Name: "ONEKS_APPLICATION_UID", Value: string(app.UID)},
		corev1.EnvVar{Name: "ONEKS_APPLICATION_NAME", Value: app.Name},
		corev1.EnvVar{Name: "ONEKS_APPLICATION_NAMESPACE", Value: app.Namespace},
	)
	// The application is already deleting. An owner reference would allow GC to
	// remove the Job before cleanup completes, so use labels and explicit deletion.
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("oneks-cleanup-%x", digest[:12]), Namespace: HelmChartNamespace, Labels: ownershipLabels(app)},
		Spec: batchv1.JobSpec{BackoffLimit: &backoff, ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: ownershipLabels(app)}, Spec: corev1.PodSpec{
				ServiceAccountName: spec.ServiceAccountName, RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{Name: "cleanup", Image: spec.Image, Command: spec.Command, Args: spec.Args, Env: env}},
			}},
		},
	}
}

func (r *Reconciler) saveCleanupJobState(ctx context.Context, app *applicationv1.OneKSApplication, state cleanupJobState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	updated := app.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[cleanupJobAnnotation] = string(data)
	if err := r.Patch(ctx, updated, client.MergeFromWithOptions(app, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	app.Annotations, app.ResourceVersion = updated.Annotations, updated.ResourceVersion
	return nil
}

func (r *Reconciler) reconcileDeleteCleanupJob(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	if app.Spec.DeletionPolicy != applicationv1.DeletionPolicyDelete || !ownsHelmLifecycle(app) {
		return false, nil
	}
	if (app.Spec.Uninstall == nil || app.Spec.Uninstall.CleanupJob == nil) && app.Annotations[cleanupJobAnnotation] == "" {
		return false, nil
	}
	pending, cleanupErr := r.reconcileCleanupJob(ctx, app)
	if pending || cleanupErr != nil {
		status := baseStatus(app)
		status.Phase = applicationv1.PhaseDeleting
		status.Progress.Current = "CleanupJob"
		setCondition(&status, app.Generation, ConditionReady, metav1.ConditionFalse, "CleanupInProgress", "Waiting for catalogue cleanup before Helm uninstall")
		if cleanupErr != nil {
			setLastError(&status, "CleanupJobFailed", cleanupErr.Error())
			r.event(app, corev1.EventTypeWarning, "CleanupJobFailed", truncate(cleanupErr.Error(), 512))
		}
		if err := r.updateStatus(ctx, app, status); err != nil {
			if cleanupErr != nil {
				return pending, fmt.Errorf("%w (status update failed: %v)", cleanupErr, err)
			}
			return pending, err
		}
	}
	return pending, cleanupErr
}

func (r *Reconciler) reconcileCleanupJob(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	if app.Spec.Uninstall == nil || app.Spec.Uninstall.CleanupJob == nil {
		return false, fmt.Errorf("cleanupJob was removed after cleanup began")
	}
	spec := app.Spec.Uninstall.CleanupJob
	if err := validateCleanupJob(*spec, "uninstall.cleanupJob"); err != nil {
		return false, err
	}
	state := cleanupJobState{Digest: cleanupJobDigest(spec)}
	data := app.Annotations[cleanupJobAnnotation]
	if data == "" {
		return true, r.saveCleanupJobState(ctx, app, state)
	}
	if err := json.Unmarshal([]byte(data), &state); err != nil || state.Digest != cleanupJobDigest(spec) {
		return false, fmt.Errorf("cleanup Job snapshot is invalid or its plan changed during deletion")
	}
	job := cleanupJob(app)
	err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(job), job)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil && !ownershipMatches(app, job) {
		return false, &OwnershipConflictError{Kind: "Job", Namespace: job.Namespace, Name: job.Name}
	}
	if state.Completed {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if job.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, job, append(deletePreconditions(job), client.PropagationPolicy(metav1.DeletePropagationForeground))...); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		account := &corev1.ServiceAccount{}
		if err := r.authoritativeReader().Get(ctx, client.ObjectKey{Namespace: HelmChartNamespace, Name: spec.ServiceAccountName}, account); err != nil {
			return false, fmt.Errorf("verify cleanup service account: %w", err)
		}
		if err := r.Create(ctx, cleanupJob(app)); err != nil {
			return false, err
		}
		r.event(app, corev1.EventTypeNormal, "CleanupJobStarted", fmt.Sprintf("Started cleanup Job %s/%s", job.Namespace, job.Name))
		return true, nil
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type == batchv1.JobFailed {
			return false, fmt.Errorf("cleanup Job %s/%s failed; inspect its logs and delete the failed Job to retry", job.Namespace, job.Name)
		}
		if condition.Type == batchv1.JobComplete {
			state.Completed = true
			return true, r.saveCleanupJobState(ctx, app, state)
		}
	}
	return true, nil
}
