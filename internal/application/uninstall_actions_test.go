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
	"strings"
	"testing"
	"time"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestDependencyPlanUninstallMaterializesIntoV1Beta1Child(t *testing.T) {
	plan := dependencyPlanForTest("oneks-longhorn", "longhorn", nil)
	plan.Release.TargetNamespace = "longhorn-system"
	plan.Uninstall = longhornUninstall()
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})

	assertPlanValid(t, root)
	reconciler, _ := testReconciler(t, root)
	materialized, err := reconciler.materializeRootDependencies(context.Background(), root)
	if err != nil || materialized.stop {
		t.Fatalf("materialize Longhorn dependency: result %#v, err %v", materialized, err)
	}
	child := &applicationv1.OneKSApplication{}
	requireNoError(t, reconciler.Get(ctx, types.NamespacedName{Namespace: applicationv1.ApplicationNamespace, Name: plan.Name}, child), "get materialized dependency")
	if child.Spec.PlanVersion != applicationv1.PlanVersion || child.Spec.Role != applicationv1.ApplicationRoleDependency || child.Spec.Uninstall == nil {
		t.Fatalf("materialized dependency lacks v1beta1 uninstall action: %#v", child.Spec)
	}
	if got := child.Spec.Uninstall.PreActions[0]; got.Resource.Namespace != "longhorn-system" || got.PatchJSON != `{"value":"true"}` {
		t.Fatalf("materialized Longhorn action = %#v", got)
	}
}

func TestInvalidDependencyUninstallActionsAreRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*applicationv1.UninstallPreAction)
	}{
		{"invalid identity", func(action *applicationv1.UninstallPreAction) { action.Resource.APIVersion = "invalid/group/version" }},
		{"unresolved placeholder", func(action *applicationv1.UninstallPreAction) { action.PatchJSON = `{"value":"${unknown}"}` }},
		{"malformed patch", func(action *applicationv1.UninstallPreAction) { action.PatchJSON = `[` }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := dependencyPlanForTest("oneks-longhorn", "longhorn", nil)
			plan.Release.TargetNamespace = "longhorn-system"
			plan.Uninstall = longhornUninstall()
			test.mutate(&plan.Uninstall.PreActions[0])
			root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
			if err := validatePlan(root, root.Spec.ClusterID); err == nil {
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
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})

	assertPlanValid(t, root)
}

func TestDependencyDeletionRunsPatchBeforeHelmChartDelete(t *testing.T) {
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
	if !controllerutil.ContainsFinalizer(stored, applicationv1.ApplicationFinalizer) || strings.Join(writes.writes, ",") != "patch:Setting" {
		t.Fatalf("patch failure advanced deletion: finalizers=%#v writes=%#v", stored.Finalizers, writes.writes)
	}
}

func TestDependencyTerminatingHelmSkipsPreActionAndRepeatedDelete(t *testing.T) {
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
	if !controllerutil.ContainsFinalizer(stored, applicationv1.ApplicationFinalizer) {
		t.Fatalf("terminating Helm allowed finalizer removal: %#v", stored.Finalizers)
	}
}

func TestDependencyPreUninstallRunsOnceThenCleanupContinuesAfterHelmDisappears(t *testing.T) {
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
	requireNoError(t, recorder.Client.Get(ctx, client.ObjectKeyFromObject(terminating), terminating), "get terminating HelmChart")
	if timestamp := terminating.GetDeletionTimestamp(); timestamp == nil || timestamp.IsZero() {
		t.Fatalf("initial Delete did not leave a terminating HelmChart: %#v", terminating.Object)
	}

	requireNoError(t, recorder.Client.Delete(ctx, setting), "remove preAction target")
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

	reconcileOnce(t, ctx, reconciler, app)
	assertApplicationNotFound(t, ctx, reconciler.Client, app.Name)
}

func TestDependencyPreUninstallSkipsWhenHelmAbsentOrRetained(t *testing.T) {
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

func deletingLonghornDependency(t *testing.T, policy applicationv1.DeletionPolicy) (*applicationv1.OneKSApplication, *unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	app := withApplicationFinalizer(validDependencyPlanApplication(t))
	app.Spec.Release.ReleaseName = "oneks-longhorn"
	app.Spec.Release.TargetNamespace = "longhorn-system"
	app.Name = dependencyApplicationName(app.Spec.Release.ReleaseName)
	app.Spec.Uninstall = longhornUninstall()
	app.Spec.DeletionPolicy = policy
	now := metav1.NewTime(time.Now())
	app.DeletionTimestamp = &now
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
