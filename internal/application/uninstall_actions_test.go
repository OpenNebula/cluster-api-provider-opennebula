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
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDependencyPlanUninstallMaterializesIntoV1Alpha5Child(t *testing.T) {
	plan := dependencyPlanForTest("oneks-longhorn", "longhorn", nil)
	plan.Release.TargetNamespace = "longhorn-system"
	plan.Uninstall = longhornUninstall()
	refreshDependencyPlanDigestForTest("42", &plan)
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})

	if err := ValidatePlan(root, ValidationConfig{ClusterID: root.Spec.ClusterID}); err != nil {
		t.Fatalf("valid Longhorn dependency rejected: %v", err)
	}
	reconciler, _ := testReconciler(t, root)
	if _, _, conflict, err := reconciler.materializeRootDependencies(context.Background(), root); err != nil || conflict != nil {
		t.Fatalf("materialize Longhorn dependency: conflict %#v, err %v", conflict, err)
	}
	child := &applicationv1.OneKSApplication{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: applicationv1.ApplicationNamespace, Name: plan.Name}, child); err != nil {
		t.Fatal(err)
	}
	if child.Spec.PlanVersion != applicationv1.PlanVersion || child.Spec.Role != applicationv1.ApplicationRoleDependency || child.Spec.Uninstall == nil {
		t.Fatalf("materialized dependency lacks v1alpha5 uninstall action: %#v", child.Spec)
	}
	if got := child.Spec.Uninstall.PreActions[0]; got.Resource.Namespace != "longhorn-system" || got.PatchJSON != `{"value":"true"}` {
		t.Fatalf("materialized Longhorn action = %#v", got)
	}
}

func TestLonghornDependencyChildDigestMatchesOneKSCompiler(t *testing.T) {
	plan := applicationv1.DependencyPlan{
		Name: "oneks-dep-oneks-longhorn-e705e7af33775af3fd13", CatalogueChartID: "e3a6dcfe-abca-406a-a73b-d173b75b143a",
		PlanDigest: "sha256-6dYpATgfPGBVslfmQFiq9ZJ9yANuUlbMu3FUqyR5vyo",
		Release: applicationv1.ReleaseSpec{
			ChartID: "e3a6dcfe-abca-406a-a73b-d173b75b143a", RepositoryURL: "https://charts.longhorn.io",
			Chart: "longhorn", Version: "v1.12.0", ReleaseName: "oneks-longhorn",
			TargetNamespace: "longhorn-system", CreateNamespace: true,
			ValuesContent: "persistence:\n  createStorageClass: true\n  defaultClass: true\n  defaultClassReplicaCount: 1\n  reclaimPolicy: Retain\n",
		},
		Resources: []applicationv1.ResourceSpec{}, Dependencies: []applicationv1.DependencyReference{},
		Uninstall: longhornUninstall(), DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
	canonical, err := canonicalPlan(dependencyPlanChildSpec("42", plan))
	if err != nil {
		t.Fatal(err)
	}
	if got := Digest(canonical); got != plan.PlanDigest {
		t.Fatalf("Longhorn child digest = %s, want OneKS compiler digest %s", got, plan.PlanDigest)
	}

	root := applicationv1.OneKSApplicationSpec{
		ClusterID: "42", CatalogueChartID: "d511b694-d868-4e40-8224-fdf6a0ca3383",
		PlanVersion: applicationv1.PlanVersion, ExecutionMode: applicationv1.ExecutionModeExecute,
		Release: applicationv1.ReleaseSpec{
			ChartID: "d511b694-d868-4e40-8224-fdf6a0ca3383", RepositoryURL: "https://prometheus-community.github.io/helm-charts",
			Chart: "kube-prometheus-stack", Version: "v87.12.2", ReleaseName: "oneks-root",
			TargetNamespace: "catalogue-workloads", CreateNamespace: false,
			ValuesContent: "grafana:\n  enabled: false\n",
		},
		Resources: []applicationv1.ResourceSpec{}, Role: applicationv1.ApplicationRoleRoot,
		Dependencies:    []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)},
		DependencyPlans: []applicationv1.DependencyPlan{plan}, DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
	rootCanonical, err := canonicalPlan(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := Digest(rootCanonical); got == "" {
		t.Fatal("Longhorn Root digest is empty")
	}
}

func TestGeneratedCRDBoundsDependencyUninstallActions(t *testing.T) {
	payload, err := os.ReadFile("../../config/crd/bases/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, required := range []string{
		"top-level uninstall is permitted only for Dependency applications",
		"dependency plans do not permit release.authSecret",
		"patchJSON:", "maxLength: 16384", "maxItems: 8",
		"- kubernetesPatch", "- merge",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("generated CRD lacks uninstall boundary %q", required)
		}
	}
	if strings.Count(text, "uninstall:") < 2 {
		t.Fatal("generated CRD does not expose uninstall on both DependencyPlan and the materialized child spec")
	}
}

func TestDependencyUninstallActionsAffectChildAndRootDigestsInOrder(t *testing.T) {
	base := dependencyPlanForTest("oneks-longhorn", "longhorn", nil)
	base.Release.TargetNamespace = "longhorn-system"
	base.Uninstall = longhornUninstall()
	refreshDependencyPlanDigestForTest("42", &base)
	baseRoot := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(base)}, []applicationv1.DependencyPlan{base})

	mutations := []func(*applicationv1.DependencyPlan){
		func(plan *applicationv1.DependencyPlan) { plan.Uninstall.PreActions[0].Resource.Name = "other-setting" },
		func(plan *applicationv1.DependencyPlan) { plan.Uninstall.PreActions[0].Type = "otherPatch" },
		func(plan *applicationv1.DependencyPlan) { plan.Uninstall.PreActions[0].PatchJSON = `{"value":"false"}` },
		func(plan *applicationv1.DependencyPlan) {
			plan.Uninstall.PreActions = append(plan.Uninstall.PreActions, plan.Uninstall.PreActions[0])
		},
	}
	for index, mutate := range mutations {
		changed := cloneDependencyPlan(t, base)
		mutate(&changed)
		refreshDependencyPlanDigestForTest("42", &changed)
		changedRoot := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(changed)}, []applicationv1.DependencyPlan{changed})
		if changed.PlanDigest == base.PlanDigest {
			t.Fatalf("mutation %d did not affect child digest", index)
		}
		if changedRoot.Spec.PlanDigest == baseRoot.Spec.PlanDigest {
			t.Fatalf("mutation %d did not affect Root digest", index)
		}
	}
}

func TestInvalidDependencyUninstallActionsAreRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*applicationv1.UninstallPreAction)
	}{
		{"type", func(action *applicationv1.UninstallPreAction) { action.Type = "exec" }},
		{"patch type", func(action *applicationv1.UninstallPreAction) { action.PatchType = "json" }},
		{"invalid identity", func(action *applicationv1.UninstallPreAction) { action.Resource.Name = "Invalid_Name" }},
		{"unresolved placeholder", func(action *applicationv1.UninstallPreAction) { action.PatchJSON = `{"value":"${unknown}"}` }},
		{"malformed patch", func(action *applicationv1.UninstallPreAction) { action.PatchJSON = `[` }},
		{"oversized patch", func(action *applicationv1.UninstallPreAction) {
			action.PatchJSON = `{"value":"` + strings.Repeat("x", maxMergePatchBytes) + `"}`
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := dependencyPlanForTest("oneks-longhorn", "longhorn", nil)
			plan.Release.TargetNamespace = "longhorn-system"
			plan.Uninstall = longhornUninstall()
			test.mutate(&plan.Uninstall.PreActions[0])
			refreshDependencyPlanDigestForTest("42", &plan)
			root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
			if err := ValidatePlan(root, ValidationConfig{ClusterID: root.Spec.ClusterID}); err == nil {
				t.Fatal("invalid uninstall action was accepted")
			}
		})
	}
}

func TestStructurallyValidNonLonghornUninstallActionIsAccepted(t *testing.T) {
	plan := dependencyPlanForTest("oneks-generic", "generic", nil)
	plan.Uninstall = &applicationv1.UninstallSpec{PreActions: []applicationv1.UninstallPreAction{{
		Type: applicationv1.UninstallPreActionKubernetesPatch,
		Resource: applicationv1.KubernetesPatchResource{
			APIVersion: "example.test/v1", Kind: "Widget", Namespace: "applications", Name: "generic-widget",
		},
		PatchType: applicationv1.KubernetesPatchTypeMerge,
		PatchJSON: `{"spec":{"enabled":true}}`,
	}}}
	refreshDependencyPlanDigestForTest("42", &plan)
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})

	if err := ValidatePlan(root, ValidationConfig{ClusterID: root.Spec.ClusterID}); err != nil {
		t.Fatalf("structurally valid generic uninstall action rejected: %v", err)
	}
}

func TestTopLevelUninstallIsOnlyValidForCurrentDependency(t *testing.T) {
	valid := validDependencyPlanApplication(t)
	valid.Spec.Uninstall = longhornUninstall()
	valid.Spec.Release.ReleaseName = "oneks-longhorn"
	valid.Name = dependencyApplicationName(valid.Spec.Release.ReleaseName)
	valid.Spec.Release.TargetNamespace = "longhorn-system"
	refreshPlanDigest(valid)
	valid.Labels = producerLabels(valid)
	if err := ValidatePlan(valid, ValidationConfig{ClusterID: valid.Spec.ClusterID}); err != nil {
		t.Fatalf("current Dependency uninstall rejected: %v", err)
	}

	invalid := []*applicationv1.OneKSApplication{
		goldenApplication(t), validRootPlan(t), validManagedRootPlan(t), validBoundProtectedRootPlan(t),
	}
	for _, app := range invalid {
		app.Spec.Uninstall = longhornUninstall()
		refreshDigest(t, app)
		app.Labels = producerLabels(app)
		if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "InvalidUninstall" {
			t.Fatalf("%s top-level uninstall error = %#v", app.Spec.PlanVersion, err)
		}
	}
}

func TestDependencyDeletionRunsPatchBeforeHelmChartDelete(t *testing.T) {
	ctx := context.Background()
	app, helm, setting := deletingLonghornDependency(t, applicationv1.DeletionPolicyDelete)
	reconciler, recorder := testReconciler(t, app, helm, setting)
	writes := &uninstallWriteClient{Client: recorder.Client}
	reconciler.Client = writes

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("delete reconcile = %#v, %v", result, err)
	}
	if got := strings.Join(writes.writes, ","); got != "patch:Setting,delete:HelmChart" {
		t.Fatalf("pre-uninstall write order = %s", got)
	}
}

func TestDependencyPreUninstallPatchFailureKeepsHelmAndFinalizer(t *testing.T) {
	ctx := context.Background()
	app, helm, setting := deletingLonghornDependency(t, applicationv1.DeletionPolicyDelete)
	reconciler, recorder := testReconciler(t, app, helm, setting)
	patchErr := errors.New("simulated patch failure")
	writes := &uninstallWriteClient{Client: recorder.Client, patchErr: patchErr}
	reconciler.Client = writes

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if !errors.Is(err, patchErr) {
		t.Fatalf("patch failure = %v, want %v", err, patchErr)
	}
	assertExists(t, ctx, reconciler.Client, helmChartObject(helm.GetName()), HelmChartNamespace, helm.GetName())
	stored := getApplication(t, ctx, reconciler.Client, app)
	if !containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) || strings.Join(writes.writes, ",") != "patch:Setting" {
		t.Fatalf("patch failure advanced deletion: finalizers=%#v writes=%#v", stored.Finalizers, writes.writes)
	}
}

func TestDependencyTerminatingHelmSkipsPreActionAndRepeatedDelete(t *testing.T) {
	ctx := context.Background()
	app, helm, _ := deletingLonghornDependency(t, applicationv1.DeletionPolicyDelete)
	helm.SetFinalizers([]string{"helmcharts.helm.cattle.io/uninstall"})
	now := metav1.Now()
	helm.SetDeletionTimestamp(&now)
	reconciler, recorder := testReconciler(t, app, helm)
	writes := &uninstallWriteClient{Client: recorder.Client, getErr: errors.New("preAction API is unavailable")}
	reconciler.Client = writes

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("terminating Helm reconcile = %#v, %v", result, err)
	}
	if writes.preActionGets != 0 || writes.helmDeletes != 0 || len(writes.writes) != 0 {
		t.Fatalf("terminating Helm retried preAction or Delete: gets=%d deletes=%d writes=%#v", writes.preActionGets, writes.helmDeletes, writes.writes)
	}
	stored := getApplication(t, ctx, reconciler.Client, app)
	if !containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("terminating Helm allowed finalizer removal: %#v", stored.Finalizers)
	}
}

func TestDependencyPreUninstallRunsOnceThenCleanupContinuesAfterHelmDisappears(t *testing.T) {
	ctx := context.Background()
	app, helm, setting := deletingLonghornDependency(t, applicationv1.DeletionPolicyDelete)
	helm.SetFinalizers([]string{"helmcharts.helm.cattle.io/uninstall"})
	reconciler, recorder := testReconciler(t, app, helm, setting)
	writes := &uninstallWriteClient{Client: recorder.Client}
	reconciler.Client = writes

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("initial Helm deletion reconcile = %#v, %v", result, err)
	}
	if got := strings.Join(writes.writes, ","); got != "patch:Setting,delete:HelmChart" {
		t.Fatalf("initial pre-uninstall write order = %s", got)
	}
	terminating := helmChartObject(helm.GetName())
	if err := recorder.Client.Get(ctx, client.ObjectKeyFromObject(terminating), terminating); err != nil {
		t.Fatalf("get terminating HelmChart: %v", err)
	}
	if timestamp := terminating.GetDeletionTimestamp(); timestamp == nil || timestamp.IsZero() {
		t.Fatalf("initial Delete did not leave a terminating HelmChart: %#v", terminating.Object)
	}

	if err := recorder.Client.Delete(ctx, setting); err != nil {
		t.Fatalf("remove preAction target: %v", err)
	}
	writes.getErr = errors.New("preAction API disappeared")
	result, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("terminating Helm retry = %#v, %v", result, err)
	}
	if writes.preActionGets != 1 || writes.helmDeletes != 1 || strings.Join(writes.writes, ",") != "patch:Setting,delete:HelmChart" {
		t.Fatalf("terminating retry repeated lifecycle effects: gets=%d deletes=%d writes=%#v", writes.preActionGets, writes.helmDeletes, writes.writes)
	}

	terminating.SetFinalizers(nil)
	if err := recorder.Client.Update(ctx, terminating); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("complete HelmChart finalization: %v", err)
	}
	remaining := helmChartObject(helm.GetName())
	if err := recorder.Client.Get(ctx, client.ObjectKeyFromObject(remaining), remaining); err == nil {
		if err := recorder.Client.Delete(ctx, remaining); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("remove finalized HelmChart: %v", err)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("verify finalized HelmChart: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)}); err != nil {
		t.Fatalf("continue cleanup after Helm disappearance: %v", err)
	}
	assertApplicationNotFound(t, ctx, reconciler.Client, app.Name)
}

func TestDependencyPreUninstallSkipsWhenHelmAbsentOrRetained(t *testing.T) {
	ctx := context.Background()
	t.Run("Helm absent", func(t *testing.T) {
		app, _, setting := deletingLonghornDependency(t, applicationv1.DeletionPolicyDelete)
		reconciler, recorder := testReconciler(t, app, setting)
		writes := &uninstallWriteClient{Client: recorder.Client}
		reconciler.Client = writes
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)}); err != nil {
			t.Fatal(err)
		}
		if len(writes.writes) != 0 {
			t.Fatalf("Helm absence ran preAction: %#v", writes.writes)
		}
	})
	t.Run("Retain", func(t *testing.T) {
		app, helm, setting := deletingLonghornDependency(t, applicationv1.DeletionPolicyRetain)
		reconciler, recorder := testReconciler(t, app, helm, setting)
		writes := &uninstallWriteClient{Client: recorder.Client}
		reconciler.Client = writes
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)}); err != nil {
			t.Fatal(err)
		}
		if len(writes.writes) != 0 {
			t.Fatalf("Retain ran preAction: %#v", writes.writes)
		}
		assertExists(t, ctx, reconciler.Client, helmChartObject(helm.GetName()), HelmChartNamespace, helm.GetName())
	})
}

func longhornUninstall() *applicationv1.UninstallSpec {
	return &applicationv1.UninstallSpec{PreActions: []applicationv1.UninstallPreAction{{
		Type: applicationv1.UninstallPreActionKubernetesPatch,
		Resource: applicationv1.KubernetesPatchResource{
			APIVersion: "longhorn.io/v1beta2", Kind: "Setting", Namespace: "longhorn-system", Name: "deleting-confirmation-flag",
		},
		PatchType: applicationv1.KubernetesPatchTypeMerge, PatchJSON: `{"value":"true"}`,
	}}}
}

func cloneDependencyPlan(t *testing.T, plan applicationv1.DependencyPlan) applicationv1.DependencyPlan {
	t.Helper()
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var cloned applicationv1.DependencyPlan
	if err := json.Unmarshal(payload, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func deletingLonghornDependency(t *testing.T, policy applicationv1.DeletionPolicy) (*applicationv1.OneKSApplication, *unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	app := validDependencyPlanApplication(t)
	app.Spec.Release.ReleaseName = "oneks-longhorn"
	app.Spec.Release.TargetNamespace = "longhorn-system"
	app.Name = dependencyApplicationName(app.Spec.Release.ReleaseName)
	app.Spec.Uninstall = longhornUninstall()
	app.Spec.DeletionPolicy = policy
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	now := metav1.NewTime(time.Now())
	app.DeletionTimestamp = &now
	refreshPlanDigest(app)
	app.Labels = producerLabels(app)
	helm := desiredHelmChart(app)
	helm.SetUID(types.UID("helm-uid"))
	helm.SetResourceVersion("1")
	setting := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Setting",
		"metadata": map[string]any{"namespace": "longhorn-system", "name": "deleting-confirmation-flag"},
		"value":    "false",
	}}
	return app, helm, setting
}

type uninstallWriteClient struct {
	client.Client
	writes        []string
	patchErr      error
	getErr        error
	preActionGets int
	helmDeletes   int
}

func (c *uninstallWriteClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if object.GetObjectKind().GroupVersionKind().Kind == "Setting" {
		c.preActionGets++
		if c.getErr != nil {
			return c.getErr
		}
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *uninstallWriteClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if object.GetObjectKind().GroupVersionKind().Kind == "Setting" {
		c.writes = append(c.writes, "patch:Setting")
		if c.patchErr != nil {
			return c.patchErr
		}
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c *uninstallWriteClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if object.GetObjectKind().GroupVersionKind().Kind == "HelmChart" {
		c.writes = append(c.writes, "delete:HelmChart")
		c.helmDeletes++
	}
	return c.Client.Delete(ctx, object, options...)
}
