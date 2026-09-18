/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.
Licensed under the Apache License, Version 2.0.
*/
package application

import (
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cleanupJobFixture(t *testing.T) (*applicationv1.OneKSApplication, *Reconciler, *recordingClient) {
	t.Helper()
	app := withApplicationFinalizer(goldenApplication(t))
	app.Spec.ManagedResources = nil
	now := metav1.Now()
	app.DeletionTimestamp = &now
	app.Spec.Uninstall = &applicationv1.UninstallSpec{CleanupJob: &applicationv1.CleanupJobSpec{
		Image: "example/cleanup:1", ServiceAccountName: "cleanup-runner", Command: []string{"/cleanup"},
		Env: []applicationv1.CleanupJobEnv{{Name: "ENDPOINT", Value: "https://application.example"}},
	}}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: HelmChartNamespace, Name: "cleanup-runner"}}
	r, recorder := testReconciler(t, app, desiredHelmChart(app), account)
	return app, r, recorder
}

func markCleanupJob(t *testing.T, r *Reconciler, app *applicationv1.OneKSApplication, condition batchv1.JobConditionType) *batchv1.Job {
	t.Helper()
	job := cleanupJob(app)
	requireNoError(t, r.Get(ctx, client.ObjectKeyFromObject(job), job), "get cleanup Job")
	job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}}
	requireNoError(t, r.Status().Update(ctx, job), "set cleanup Job status")
	return job
}

func TestCleanupJobFinishesAndDisappearsBeforeHelmDeletion(t *testing.T) {
	app, r, recorder := cleanupJobFixture(t)
	reconcileOnce(t, ctx, r, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("created Job before persisting cleanup state: %v", recorder.childWrites)
	}
	reconcileOnce(t, ctx, r, app)
	reconcileOnce(t, ctx, r, app)
	assertExists(t, ctx, r.Client, helmChartObject(app.Spec.Release.ReleaseName), HelmChartNamespace, app.Spec.Release.ReleaseName)
	job := markCleanupJob(t, r, app, batchv1.JobComplete)
	if len(job.OwnerReferences) != 0 {
		t.Fatal("Job can be garbage-collected with its deleting parent")
	}
	values := map[string]string{}
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		values[env.Name] = env.Value
	}
	if values["ONEKS_APPLICATION_UID"] != string(app.UID) || values["ENDPOINT"] != "https://application.example" {
		t.Fatalf("cleanup identity/environment missing: %v", values)
	}
	job.Finalizers = []string{"example.test/hold"}
	requireNoError(t, r.Update(ctx, job), "hold Job deletion")
	reconcileOnce(t, ctx, r, app) // persist completion
	reconcileOnce(t, ctx, r, app) // request Job/Pod deletion
	reconcileOnce(t, ctx, r, app) // wait for Job finalizer
	assertExists(t, ctx, r.Client, helmChartObject(app.Spec.Release.ReleaseName), HelmChartNamespace, app.Spec.Release.ReleaseName)
	requireNoError(t, r.Get(ctx, client.ObjectKeyFromObject(job), job), "get terminating Job")
	job.Finalizers = nil
	requireNoError(t, r.Update(ctx, job), "finish Job deletion")
	reconcileOnce(t, ctx, r, app)
	if err := r.Get(ctx, client.ObjectKey{Namespace: HelmChartNamespace, Name: app.Spec.Release.ReleaseName}, helmChartObject(app.Spec.Release.ReleaseName)); !apierrors.IsNotFound(err) {
		t.Fatalf("HelmChart remains: %v", err)
	}
}

func TestCleanupJobFailureBlocksHelmAndCanBeRetried(t *testing.T) {
	app, r, _ := cleanupJobFixture(t)
	reconcileOnce(t, ctx, r, app)
	reconcileOnce(t, ctx, r, app)
	job := markCleanupJob(t, r, app, batchv1.JobFailed)
	stored := getApplication(t, ctx, r.Client, app)
	_, err := r.reconcileDeleteCleanupJob(ctx, stored)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("Job failure was ignored: %v", err)
	}
	stored = getApplication(t, ctx, r.Client, app)
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "CleanupJobFailed" {
		t.Fatalf("failure absent from application status: %#v", stored.Status)
	}
	assertExists(t, ctx, r.Client, helmChartObject(app.Spec.Release.ReleaseName), HelmChartNamespace, app.Spec.Release.ReleaseName)
	requireNoError(t, r.Delete(ctx, job), "remove failed Job")
	reconcileOnce(t, ctx, r, app)
	assertExists(t, ctx, r.Client, &batchv1.Job{}, HelmChartNamespace, job.Name)
}

func TestCleanupJobRetainSkipsCleanup(t *testing.T) {
	app, r, recorder := cleanupJobFixture(t)
	app.Spec.DeletionPolicy = applicationv1.DeletionPolicyRetain
	pending, err := r.reconcileDeleteCleanupJob(ctx, app)
	if pending || err != nil {
		t.Fatalf("Retain requested cleanup: %v, %v", pending, err)
	}
	assertNoChildWrites(t, recorder)
}

func TestCleanupJobDoesNotAdoptForeignJob(t *testing.T) {
	app, r, recorder := cleanupJobFixture(t)
	reconcileOnce(t, ctx, r, app)
	job := cleanupJob(app)
	job.Labels[LabelApplicationUID] = "other-application"
	requireNoError(t, recorder.Client.Create(ctx, job), "create foreign Job")
	stored := getApplication(t, ctx, r.Client, app)
	if _, err := r.reconcileDeleteCleanupJob(ctx, stored); err == nil {
		t.Fatal("foreign Job adopted")
	}
	assertNoChildWrites(t, recorder)
}

func TestCleanupJobRequiresExistingAccountBeforeCreatingJob(t *testing.T) {
	app, r, recorder := cleanupJobFixture(t)
	reconcileOnce(t, ctx, r, app)
	requireNoError(t, recorder.Client.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: HelmChartNamespace, Name: "cleanup-runner"}}), "remove runner account")
	stored := getApplication(t, ctx, r.Client, app)
	if _, err := r.reconcileDeleteCleanupJob(ctx, stored); err == nil {
		t.Fatal("created Job with absent account")
	}
	assertNoChildWrites(t, recorder)
}

func TestCleanupJobValidatesGenericContractAndRootPreActionRestriction(t *testing.T) {
	app, _, _ := cleanupJobFixture(t)
	assertPlanValid(t, app)
	for _, mutate := range []func(*applicationv1.CleanupJobSpec){
		func(job *applicationv1.CleanupJobSpec) { job.Command = nil },
		func(job *applicationv1.CleanupJobSpec) { job.TimeoutSeconds = 1 },
		func(job *applicationv1.CleanupJobSpec) {
			job.Env = []applicationv1.CleanupJobEnv{{Name: "ONEKS_APPLICATION_UID", Value: "override"}}
		},
	} {
		invalid := app.DeepCopy()
		mutate(invalid.Spec.Uninstall.CleanupJob)
		if err := validatePlan(invalid, invalid.Spec.ClusterID); err == nil {
			t.Fatal("invalid cleanup Job accepted")
		}
	}
	app.Spec.Uninstall.PreActions = longhornUninstall().PreActions
	if err := validatePlan(app, app.Spec.ClusterID); err == nil {
		t.Fatal("Root preActions accepted")
	}
}
