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
	"strings"
	"testing"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExecuteCreatesConfigMapBeforeHelmChartAndBecomesReady(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, recorder := testReconciler(t, app)

	reconcileOnce(t, ctx, reconciler, app)
	assertExists(t, ctx, reconciler.Client, &corev1.ConfigMap{}, WorkloadNamespace, app.Spec.Resources[0].Name)
	assertNotFound(t, ctx, reconciler.Client, helmChartObject(app.Spec.Release.ReleaseName))

	reconcileOnce(t, ctx, reconciler, app)
	helm := helmChartObject(app.Spec.Release.ReleaseName)
	assertExists(t, ctx, reconciler.Client, helm, HelmChartNamespace, app.Spec.Release.ReleaseName)
	if got := strings.Join(recorder.childWrites, ","); got != "create:ConfigMap,create:HelmChart" {
		t.Fatalf("unexpected child write order: %s", got)
	}

	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(helm), helm); err != nil {
		t.Fatalf("get HelmChart: %v", err)
	}
	helm.Object["status"] = map[string]any{"jobName": "helm-install-oneks-prometheus"}
	if err := reconciler.Update(ctx, helm); err != nil {
		t.Fatalf("set HelmChart status: %v", err)
	}
	job := completedJob("helm-install-oneks-prometheus")
	if err := reconciler.Create(ctx, job); err != nil {
		t.Fatalf("create installer Job: %v", err)
	}
	reconcileOnce(t, ctx, reconciler, app)

	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseReady {
		t.Fatalf("expected Ready, got %#v", stored.Status)
	}
	if stored.Status.HelmChartRef == nil || stored.Status.HelmChartRef.Name != app.Spec.Release.ReleaseName {
		t.Fatalf("missing HelmChart status reference: %#v", stored.Status.HelmChartRef)
	}
	if len(stored.Status.Resources) != 1 || stored.Status.Resources[0].Phase != "Ready" {
		t.Fatalf("unexpected resource status: %#v", stored.Status.Resources)
	}
}

func TestExecuteAddsFinalizerBeforeCreatingChildren(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	reconciler, recorder := testReconciler(t, app)

	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if !containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("controller finalizer was not added: %#v", stored.Finalizers)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("children were written before the finalizer: %#v", recorder.childWrites)
	}
}

func TestObserveReportsWithoutFinalizerOrChildMutation(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Spec.ExecutionMode = applicationv1.ExecutionModeObserve
	refreshDigest(t, app)
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	helm := desiredHelmChart(app)
	helm.Object["status"] = map[string]any{"jobName": "helm-install-oneks-prometheus"}
	reconciler, recorder := testReconciler(t, app, configMap, helm, completedJob("helm-install-oneks-prometheus"))

	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseObserving {
		t.Fatalf("expected Observing, got %s", stored.Status.Phase)
	}
	if len(stored.Finalizers) != 0 {
		t.Fatalf("Observe added a finalizer: %#v", stored.Finalizers)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("Observe mutated children: %#v", recorder.childWrites)
	}
}

func TestObserveReportsConfigMapDataDriftWithoutMutation(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Spec.ExecutionMode = applicationv1.ExecutionModeObserve
	refreshDigest(t, app)
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	configMap.Data = map[string]string{"owner": "drifted"}
	helm := desiredHelmChart(app)
	helm.Object["status"] = map[string]any{"jobName": "helm-install-oneks-prometheus"}
	reconciler, recorder := testReconciler(t, app, configMap, helm, completedJob("helm-install-oneks-prometheus"))

	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseObserving {
		t.Fatalf("expected Observing, got %s", stored.Status.Phase)
	}
	if len(stored.Status.Resources) != 1 || stored.Status.Resources[0].Phase != "Pending" || stored.Status.Resources[0].Reason != "DataDrift" {
		t.Fatalf("ConfigMap drift was not reported: %#v", stored.Status.Resources)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("Observe mutated drifted ConfigMap: %#v", recorder.childWrites)
	}
	observed := &corev1.ConfigMap{}
	assertExists(t, ctx, reconciler.Client, observed, WorkloadNamespace, configMap.Name)
	if observed.Data["owner"] != "drifted" {
		t.Fatalf("Observe repaired ConfigMap data: %#v", observed.Data)
	}
}

func TestOwnershipConflictHasNoChildOrNamespaceSideEffect(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	conflict := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: WorkloadNamespace, Name: app.Spec.Resources[0].Name,
	}}
	reconciler, recorder := testReconciler(t, app, conflict)

	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.LastError == nil || stored.Status.LastError.Reason != "OwnershipConflict" {
		t.Fatalf("ownership conflict was not terminal: %#v", stored.Status)
	}
	if len(stored.Finalizers) != 0 {
		t.Fatalf("conflict added a finalizer: %#v", stored.Finalizers)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("conflict caused a child or namespace side effect: %#v", recorder.childWrites)
	}
}

func TestOwnershipPreflightChecksEveryChildBeforeCreatingAny(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	second := app.Spec.Resources[0].DeepCopy()
	second.ID = "second-config"
	second.Name = "second-config"
	app.Spec.Resources = append(app.Spec.Resources, *second)
	refreshDigest(t, app)
	conflict := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: WorkloadNamespace, Name: second.Name,
	}}
	reconciler, recorder := testReconciler(t, app, conflict)

	reconcileOnce(t, ctx, reconciler, app)
	assertNotFound(t, ctx, reconciler.Client, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: WorkloadNamespace, Name: app.Spec.Resources[0].Name},
	})
	if len(recorder.childWrites) != 0 {
		t.Fatalf("preflight conflict allowed an earlier child write: %#v", recorder.childWrites)
	}
}

func TestMissingTargetNamespacePollsAndProgressesWhenRestored(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	reconciler, recorder := testReconciler(t, app)
	availableClient := reconciler.Client
	reconciler.Client = &namespaceErrorClient{
		Client: reconciler.Client,
		err: apierrors.NewNotFound(
			schema.GroupResource{Resource: "namespaces"}, WorkloadNamespace,
		),
	}

	result := reconcileOnce(t, ctx, reconciler, app)
	if result.RequeueAfter != reconciler.RequeueAfter {
		t.Fatalf("missing namespace requeue = %s, want %s", result.RequeueAfter, reconciler.RequeueAfter)
	}
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "TargetNamespaceMissing" {
		t.Fatalf("missing namespace status mismatch: %#v", stored.Status)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("missing namespace caused a side effect: %#v", recorder.childWrites)
	}

	reconciler.Client = availableClient
	reconcileOnce(t, ctx, reconciler, app)
	stored = getApplication(t, ctx, reconciler.Client, app)
	if !containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("restored namespace did not resume finalizer progression: %#v", stored.Finalizers)
	}
	reconcileOnce(t, ctx, reconciler, app)
	assertExists(t, ctx, reconciler.Client, &corev1.ConfigMap{}, WorkloadNamespace, app.Spec.Resources[0].Name)
}

func TestTargetNamespaceAPIErrorIsRetriedWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	reconciler, recorder := testReconciler(t, app)
	reconciler.Client = &namespaceErrorClient{
		Client: reconciler.Client,
		err: apierrors.NewForbidden(
			schema.GroupResource{Resource: "namespaces"}, WorkloadNamespace,
			fmt.Errorf("fake authorization failure"),
		),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if err == nil || !strings.Contains(err.Error(), "check target namespace") {
		t.Fatalf("expected transient namespace API error, got %v", err)
	}
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != "" || stored.Status.LastError != nil {
		t.Fatalf("transient API error wrote terminal status: %#v", stored.Status)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("namespace API error caused a side effect: %#v", recorder.childWrites)
	}
}

func TestRestartReconcilesExistingChildrenIdempotently(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	helm := desiredHelmChart(app)
	helm.Object["status"] = map[string]any{"jobName": "helm-install-oneks-prometheus"}
	reconciler, recorder := testReconciler(t, app, configMap, helm, completedJob("helm-install-oneks-prometheus"))

	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseReady {
		t.Fatalf("restart did not recover Ready: %#v", stored.Status)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("idempotent restart rewrote children: %#v", recorder.childWrites)
	}
}

func TestExactOwnedConfigMapDriftIsRepairedWithServerSideApply(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	configMap.Data = map[string]string{"owner": "drifted"}
	helm := desiredHelmChart(app)
	reconciler, recorder := testReconciler(t, app, configMap, helm)

	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 1 || recorder.childWrites[0] != "patch:ConfigMap" {
		t.Fatalf("expected ConfigMap SSA repair, got %#v", recorder.childWrites)
	}
	if len(recorder.patchForces) != 1 || recorder.patchForces[0] {
		t.Fatalf("SSA repair forced field ownership: %#v", recorder.patchForces)
	}
	if len(recorder.patchResourceVersions) != 1 || recorder.patchResourceVersions[0] == "" {
		t.Fatalf("SSA repair lacked a resourceVersion precondition: %#v", recorder.patchResourceVersions)
	}
	stored := &corev1.ConfigMap{}
	assertExists(t, ctx, reconciler.Client, stored, WorkloadNamespace, configMap.Name)
	if fmt.Sprint(stored.Data) != fmt.Sprint(app.Spec.Resources[0].Data) {
		t.Fatalf("ConfigMap data was not repaired: %#v", stored.Data)
	}
}

func TestServerSideApplyConflictIsRetryableAndDoesNotForceOwnership(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	configMap.Data = map[string]string{"owner": "other-manager"}
	helm := desiredHelmChart(app)
	reconciler, recorder := testReconciler(t, app, configMap, helm)
	recorder.patchError = apierrors.NewConflict(
		schema.GroupResource{Resource: "configmaps"}, configMap.Name,
		fmt.Errorf("fake field manager conflict"),
	)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if !apierrors.IsConflict(err) && (err == nil || !strings.Contains(err.Error(), "field manager conflict")) {
		t.Fatalf("expected retryable apply conflict, got %v", err)
	}
	if len(recorder.patchForces) != 1 || recorder.patchForces[0] {
		t.Fatalf("conflicting apply forced ownership: %#v", recorder.patchForces)
	}
	stored := &corev1.ConfigMap{}
	assertExists(t, ctx, reconciler.Client, stored, WorkloadNamespace, configMap.Name)
	if stored.Data["owner"] != "other-manager" {
		t.Fatalf("apply conflict overwrote data: %#v", stored.Data)
	}
}

func TestHelmChartFailurePrecedesCompletedJob(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	helm := desiredHelmChart(app)
	helm.Object["status"] = map[string]any{
		"jobName": "helm-install-oneks-prometheus",
		"conditions": []any{map[string]any{
			"type": "Failed", "status": "True", "reason": "ChartError", "message": "fake chart failure",
		}},
	}
	reconciler, _ := testReconciler(t, app, configMap, helm, completedJob("helm-install-oneks-prometheus"))

	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.LastError == nil || stored.Status.LastError.Reason != "HelmChartFailed" {
		t.Fatalf("HelmChart failure did not win: %#v", stored.Status)
	}
}

func TestInstallerJobFailurePrecedenceIsOrderIndependent(t *testing.T) {
	complete := batchv1.JobCondition{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		Reason: "Complete", Message: "fake completion",
	}
	failed := batchv1.JobCondition{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: "BackoffLimitExceeded", Message: "fake failure",
	}
	for _, test := range []struct {
		name       string
		conditions []batchv1.JobCondition
	}{
		{name: "complete before failed", conditions: []batchv1.JobCondition{complete, failed}},
		{name: "failed before complete", conditions: []batchv1.JobCondition{failed, complete}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			app := goldenApplication(t)
			app.Finalizers = []string{applicationv1.ApplicationFinalizer}
			configMap := desiredConfigMap(app, app.Spec.Resources[0])
			helm := desiredHelmChart(app)
			helm.Object["status"] = map[string]any{"jobName": "helm-install-oneks-prometheus"}
			job := completedJob("helm-install-oneks-prometheus")
			job.Status.Conditions = test.conditions
			reconciler, _ := testReconciler(t, app, configMap, helm, job)

			reconcileOnce(t, ctx, reconciler, app)
			stored := getApplication(t, ctx, reconciler.Client, app)
			if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.LastError == nil || stored.Status.LastError.Reason != "InstallerJobFailed" {
				t.Fatalf("Job failure did not win for %s: %#v", test.name, stored.Status)
			}
		})
	}
}

func TestPreviouslyReadyReleaseDoesNotRegressWhenInstallerJobDisappears(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Status.Phase = applicationv1.PhaseReady
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	helm := desiredHelmChart(app)
	helm.Object["status"] = map[string]any{"jobName": "removed-installer-job"}
	reconciler, _ := testReconciler(t, app, configMap, helm)

	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseReady {
		t.Fatalf("ready release regressed: %#v", stored.Status)
	}
}

func TestDeletionRemovesHelmFirstThenDeletePolicyResources(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	now := metav1.NewTime(time.Now())
	app.DeletionTimestamp = &now
	retained := app.Spec.Resources[0].DeepCopy()
	retained.ID = "retained-config"
	retained.Name = "retained-config"
	retained.DeletionPolicy = applicationv1.DeletionPolicyRetain
	app.Spec.Resources = append(app.Spec.Resources, *retained)
	refreshDigest(t, app)
	deletedConfig := desiredConfigMap(app, app.Spec.Resources[0])
	retainedConfig := desiredConfigMap(app, app.Spec.Resources[1])
	helm := desiredHelmChart(app)
	reconciler, recorder := testReconciler(t, app, deletedConfig, retainedConfig, helm)

	reconcileOnce(t, ctx, reconciler, app)
	if got := strings.Join(recorder.childWrites, ","); got != "delete:HelmChart" {
		t.Fatalf("HelmChart was not deleted first: %s", got)
	}
	reconcileOnce(t, ctx, reconciler, app)
	if got := strings.Join(recorder.childWrites, ","); got != "delete:HelmChart,delete:ConfigMap" {
		t.Fatalf("ConfigMap cleanup order mismatch: %s", got)
	}
	reconcileOnce(t, ctx, reconciler, app)

	assertNotFound(t, ctx, reconciler.Client, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: WorkloadNamespace, Name: deletedConfig.Name}})
	assertExists(t, ctx, reconciler.Client, &corev1.ConfigMap{}, WorkloadNamespace, retainedConfig.Name)
	stored := &applicationv1.OneKSApplication{}
	err := reconciler.Get(ctx, client.ObjectKeyFromObject(app), stored)
	if err == nil && containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("application finalizer remains: %#v", stored.Finalizers)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get deleted application: %v", err)
	}
}

func TestDeletionSkipsNamespaceCheckAndRetainsTopLevelHelmChart(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.DeletionPolicy = applicationv1.DeletionPolicyRetain
	app.Spec.Resources[0].DeletionPolicy = applicationv1.DeletionPolicyRetain
	refreshDigest(t, app)
	now := metav1.NewTime(time.Now())
	app.DeletionTimestamp = &now
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	helm := desiredHelmChart(app)
	reconciler, recorder := testReconciler(t, app, configMap, helm)
	reconciler.Client = &namespaceErrorClient{
		Client: reconciler.Client,
		err: apierrors.NewNotFound(
			schema.GroupResource{Resource: "namespaces"}, WorkloadNamespace,
		),
	}

	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("Retain cleanup mutated children: %#v", recorder.childWrites)
	}
	assertExists(t, ctx, reconciler.Client, helmChartObject(helm.GetName()), HelmChartNamespace, helm.GetName())
	assertExists(t, ctx, reconciler.Client, &corev1.ConfigMap{}, WorkloadNamespace, configMap.Name)
}

func TestDriftedRetainedChildrenDoNotBlockFinalizerRemoval(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.DeletionPolicy = applicationv1.DeletionPolicyRetain
	app.Spec.Resources[0].DeletionPolicy = applicationv1.DeletionPolicyRetain
	refreshDigest(t, app)
	now := metav1.NewTime(time.Now())
	app.DeletionTimestamp = &now
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	configMap.Labels = nil
	helm := desiredHelmChart(app)
	helm.SetLabels(nil)
	reconciler, recorder := testReconciler(t, app, configMap, helm)

	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("drifted retained children were mutated: %#v", recorder.childWrites)
	}
	assertExists(t, ctx, reconciler.Client, &corev1.ConfigMap{}, WorkloadNamespace, configMap.Name)
	assertExists(t, ctx, reconciler.Client, helmChartObject(helm.GetName()), HelmChartNamespace, helm.GetName())
	stored := &applicationv1.OneKSApplication{}
	err := reconciler.Get(ctx, client.ObjectKeyFromObject(app), stored)
	if err == nil && containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("retained drift blocked finalizer removal: %#v", stored.Finalizers)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get application after retained cleanup: %v", err)
	}
}

func TestDeletionConflictDoesNotDeleteAnyChild(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	now := metav1.NewTime(time.Now())
	app.DeletionTimestamp = &now
	configMap := desiredConfigMap(app, app.Spec.Resources[0])
	helm := desiredHelmChart(app)
	helm.SetLabels(nil)
	reconciler, recorder := testReconciler(t, app, configMap, helm)

	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("deletion conflict mutated children: %#v", recorder.childWrites)
	}
	stored := getApplication(t, ctx, reconciler.Client, app)
	if !containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) || stored.Status.LastError == nil || stored.Status.LastError.Reason != "OwnershipConflict" {
		t.Fatalf("deletion conflict did not preserve ownership boundary: %#v", stored)
	}
}

func TestDeletingOwnedApplicationIgnoresRootLabelAndControllerConfigDrift(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Labels[LabelProducer] = "drifted-producer"
	now := metav1.NewTime(time.Now())
	app.DeletionTimestamp = &now
	helm := desiredHelmChart(app)
	helm.SetUID(types.UID("helm-uid"))
	helm.SetResourceVersion("7")
	reconciler, recorder := testReconciler(t, app, helm)
	reconciler.ClusterID = "replacement-controller-cluster-id"

	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 1 || recorder.childWrites[0] != "delete:HelmChart" {
		t.Fatalf("metadata/config drift wedged deletion: %#v", recorder.childWrites)
	}
	if len(recorder.deletePreconditions) != 1 || !recorder.deletePreconditions[0] {
		t.Fatalf("delete lacked UID/resourceVersion preconditions: %#v", recorder.deletePreconditions)
	}
}

func TestDeletingInvalidApplicationWithoutOurFinalizerNeverCleansChildren(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Spec.PlanDigest = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	app.Labels[LabelPlanDigest] = app.Spec.PlanDigest
	app.Finalizers = []string{"example.test/hold"}
	now := metav1.NewTime(time.Now())
	app.DeletionTimestamp = &now
	helm := desiredHelmChart(app)
	reconciler, recorder := testReconciler(t, app, helm)

	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("invalid unfinalized application cleaned children: %#v", recorder.childWrites)
	}
	assertExists(t, ctx, reconciler.Client, helmChartObject(helm.GetName()), HelmChartNamespace, helm.GetName())
}

func TestOwnershipRaceBeforeDeleteBlocksDeletion(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	now := metav1.NewTime(time.Now())
	app.DeletionTimestamp = &now
	helm := desiredHelmChart(app)
	helm.SetUID(types.UID("helm-uid"))
	reconciler, recorder := testReconciler(t, app, helm)
	reconciler.Client = &ownershipRaceClient{Client: reconciler.Client, helmName: helm.GetName()}

	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("ownership race deleted a child: %#v", recorder.childWrites)
	}
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "OwnershipConflict" {
		t.Fatalf("ownership race was not reported: %#v", stored.Status)
	}
}

func completedJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: HelmChartNamespace, Name: name},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Complete", Message: "fake completion",
		}}},
	}
}

type recordingClient struct {
	client.Client
	childWrites           []string
	patchForces           []bool
	patchResourceVersions []string
	deletePreconditions   []bool
	patchError            error
}

func (c *recordingClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	c.record("create", object)
	return c.Client.Create(ctx, object, options...)
}

func (c *recordingClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	c.record("patch", object)
	patchOptions := (&client.PatchOptions{}).ApplyOptions(options)
	c.patchForces = append(c.patchForces, patchOptions.Force != nil && *patchOptions.Force)
	c.patchResourceVersions = append(c.patchResourceVersions, object.GetResourceVersion())
	if c.patchError != nil {
		return c.patchError
	}
	if patch.Type() == types.ApplyPatchType {
		current, ok := object.DeepCopyObject().(client.Object)
		if !ok {
			return fmt.Errorf("deep copy %T is not a client object", object)
		}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
			return err
		}
		object.SetResourceVersion(current.GetResourceVersion())
		return c.Client.Update(ctx, object)
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c *recordingClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	c.record("update", object)
	return c.Client.Update(ctx, object, options...)
}

func (c *recordingClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	c.record("delete", object)
	deleteOptions := (&client.DeleteOptions{}).ApplyOptions(options)
	hasPreconditions := deleteOptions.Preconditions != nil &&
		deleteOptions.Preconditions.UID != nil &&
		deleteOptions.Preconditions.ResourceVersion != nil
	c.deletePreconditions = append(c.deletePreconditions, hasPreconditions)
	return c.Client.Delete(ctx, object, options...)
}

func (c *recordingClient) record(verb string, object client.Object) {
	kind := object.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		switch object.(type) {
		case *corev1.ConfigMap:
			kind = "ConfigMap"
		case *corev1.Namespace:
			kind = "Namespace"
		case *corev1.Secret:
			kind = "Secret"
		}
	}
	if kind == "ConfigMap" || kind == "HelmChart" || kind == "Namespace" || kind == "Secret" {
		c.childWrites = append(c.childWrites, fmt.Sprintf("%s:%s", verb, kind))
	}
}

type namespaceErrorClient struct {
	client.Client
	err error
}

type ownershipRaceClient struct {
	client.Client
	helmName string
	helmGets int
}

func (c *ownershipRaceClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	err := c.Client.Get(ctx, key, object, options...)
	if err != nil {
		return err
	}
	if chart, ok := object.(*unstructured.Unstructured); ok && key.Namespace == HelmChartNamespace && key.Name == c.helmName {
		c.helmGets++
		if c.helmGets == 2 {
			labels := chart.GetLabels()
			labels[LabelApplicationUID] = "different-owner"
			chart.SetLabels(labels)
		}
	}
	return nil
}

func (c *namespaceErrorClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Namespace); ok && key.Name == WorkloadNamespace {
		return c.err
	}
	return c.Client.Get(ctx, key, object, options...)
}

func testReconciler(t *testing.T, objects ...client.Object) (*Reconciler, *recordingClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add batch scheme: %v", err)
	}
	if err := applicationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add application scheme: %v", err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&applicationv1.OneKSApplication{}).
		WithObjects(append(objects, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: WorkloadNamespace},
		})...).Build()
	recorder := &recordingClient{Client: base}
	return &Reconciler{
		Client: recorder, Scheme: scheme, ClusterID: "42",
		ControllerVersion: "test", RequeueAfter: time.Millisecond,
	}, recorder
}

func reconcileOnce(t *testing.T, ctx context.Context, reconciler *Reconciler, app *applicationv1.OneKSApplication) ctrl.Result {
	t.Helper()
	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return result
}

func getApplication(t *testing.T, ctx context.Context, kubeClient client.Client, app *applicationv1.OneKSApplication) *applicationv1.OneKSApplication {
	t.Helper()
	stored := &applicationv1.OneKSApplication{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(app), stored); err != nil {
		t.Fatalf("get application: %v", err)
	}
	return stored
}

func assertExists(t *testing.T, ctx context.Context, kubeClient client.Client, object client.Object, namespace, name string) {
	t.Helper()
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, object); err != nil {
		t.Fatalf("expected %T %s/%s: %v", object, namespace, name, err)
	}
}

func assertNotFound(t *testing.T, ctx context.Context, kubeClient client.Client, object client.Object) {
	t.Helper()
	err := kubeClient.Get(ctx, client.ObjectKeyFromObject(object), object)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected %T %s to be absent, got %v", object, client.ObjectKeyFromObject(object), err)
	}
}

func setHelmCondition(chart *unstructured.Unstructured, conditionType, status, reason, message string) {
	chart.Object["status"] = map[string]any{"conditions": []any{map[string]any{
		"type": conditionType, "status": status, "reason": reason, "message": message,
	}}}
}
