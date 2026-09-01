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
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var dependencyProvidedBundleGVK = schema.GroupVersionKind{
	Group: "trust.cert-manager.io", Version: "v1alpha1", Kind: "Bundle",
}

func TestEarlyManagedNoMatchAllowsDependencyBootstrapAndGatesRootEffects(t *testing.T) {
	ctx := context.Background()
	root, plan := dependencyProvidedManagedRoot(t)
	root.Status.LastError = &applicationv1.ApplicationError{Reason: "SensitiveValuesContent", Message: "stale validation error"}
	root.Status.Conditions = []metav1.Condition{{
		Type: ConditionPlanValid, Status: metav1.ConditionFalse, Reason: "SensitiveValuesContent",
	}}
	reconciler, effects, gate := dependencyProvidedManagedReconciler(t, root)
	gate.unavailable = true

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(root)})
	if err != nil {
		t.Fatalf("reconcile dependency bootstrap: %v", err)
	}
	if !result.Requeue && result.RequeueAfter == 0 {
		t.Fatalf("unready dependency did not requeue: %#v", result)
	}
	getDependencyApplication(t, ctx, reconciler.Client, plan.Name)
	assertNoRootEffects(t, effects)
	if gate.gets != 1 {
		t.Fatalf("dependency-gated observation performed a managed API GET: %d GETs", gate.gets)
	}
	stored := getApplication(t, ctx, reconciler.Client, root)
	assertManagedDependencyPendingStatus(t, stored, plan.Name)

	effects.writes = nil
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(root)}); err != nil {
		t.Fatalf("reconcile unready dependency: %v", err)
	}
	assertNoRootEffects(t, effects)
	if gate.gets != 2 {
		t.Fatalf("dependency-gated observation performed a managed API GET on retry: %d GETs", gate.gets)
	}
}

func TestStrictManagedNoMatchBlocksEveryRootEffectAfterDependenciesReady(t *testing.T) {
	ctx := context.Background()
	root, plan := dependencyProvidedManagedRoot(t)
	dependency := readyDependencyForRoot(root, plan)
	reconciler, effects, gate := dependencyProvidedManagedReconciler(t, root, dependency)
	gate.unavailable = true

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(root)})
	if err == nil || !meta.IsNoMatchError(err) {
		t.Fatalf("strict managed preflight error = %v, want NoMatch", err)
	}
	assertNoRootEffects(t, effects)
}

func TestAvailableForeignManagedObjectConflictsBeforeRootEffects(t *testing.T) {
	ctx := context.Background()
	root, plan := dependencyProvidedManagedRoot(t)
	dependency := readyDependencyForRoot(root, plan)
	reconciler, effects, gate := dependencyProvidedManagedReconciler(t, root, dependency)
	gate.noMatchGets = 1
	gate.foreign = dependencyProvidedBundle(false, root)

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(root)}); err != nil {
		t.Fatalf("reconcile foreign managed object: %v", err)
	}
	assertNoRootEffects(t, effects)
	stored := getApplication(t, ctx, reconciler.Client, root)
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "OwnershipConflict" {
		t.Fatalf("foreign managed object status = %#v, want OwnershipConflict", stored.Status)
	}
	if gate.gets < 2 {
		t.Fatalf("strict preflight did not re-read managed API after early NoMatch: %d GETs", gate.gets)
	}
}

func TestAvailableAbsentManagedObjectProceedsAfterDependenciesReady(t *testing.T) {
	ctx := context.Background()
	root, plan := dependencyProvidedManagedRoot(t)
	dependency := readyDependencyForRoot(root, plan)
	reconciler, effects, gate := dependencyProvidedManagedReconciler(t, root, dependency)
	gate.noMatchGets = 1

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(root)}); err != nil {
		t.Fatalf("reconcile available managed API: %v", err)
	}
	if len(effects.writes) == 0 || effects.writes[0] != "create:Bundle" {
		t.Fatalf("managed reconciliation did not proceed: %#v", effects.writes)
	}
	if gate.gets < 2 {
		t.Fatalf("strict preflight did not re-read managed API after early NoMatch: %d GETs", gate.gets)
	}
}

func TestEarlyManagedPreflightDoesNotDeferOtherAPIErrors(t *testing.T) {
	ctx := context.Background()
	root, plan := dependencyProvidedManagedRoot(t)
	reconciler, effects, gate := dependencyProvidedManagedReconciler(t, root)
	apiErr := errors.New("simulated managed API failure")
	gate.err = apiErr

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(root)})
	if err == nil || !errors.Is(err, apiErr) {
		t.Fatalf("early managed preflight error = %v, want wrapped API error", err)
	}
	assertApplicationNotFound(t, ctx, reconciler.Client, plan.Name)
	assertNoRootEffects(t, effects)
}

func dependencyProvidedManagedRoot(t *testing.T) (*applicationv1.OneKSApplication, applicationv1.DependencyPlan) {
	t.Helper()
	plan := dependencyPlanForTest("trust-manager", "trust-manager", nil)
	root := validBoundProtectedRootPlan(t)
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	root.Spec.Dependencies = []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}
	root.Spec.DependencyPlans = []applicationv1.DependencyPlan{plan}
	root.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{{
		ID: "runai-ca-cert", Scope: applicationv1.ManagedResourceScopeCluster,
		APIVersion: dependencyProvidedBundleGVK.GroupVersion().String(), Kind: dependencyProvidedBundleGVK.Kind,
		APIResource: "bundles", Name: "runai-ca-cert",
		ManifestJSON: `{"apiVersion":"trust.cert-manager.io/v1alpha1","kind":"Bundle","metadata":{"name":"runai-ca-cert"},"spec":{"sources":[]}}`,
		Readiness: applicationv1.ManagedResourceReadiness{
			TimeoutSeconds: 60,
		},
		DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}}
	refreshOwnedPlan(t, root)
	return root, plan
}

func readyDependencyForRoot(root *applicationv1.OneKSApplication, plan applicationv1.DependencyPlan) *applicationv1.OneKSApplication {
	dependency := existingDependencyForTest(root, plan)
	setDependencyStatus(dependency, applicationv1.PhaseReady, true, "")
	return dependency
}

func dependencyProvidedManagedReconciler(t *testing.T, objects ...client.Object) (*Reconciler, *managedEffectClient, *managedAPIGateReader) {
	t.Helper()
	reconciler, _ := testReconciler(t, objects...)
	effects := &managedEffectClient{Client: reconciler.Client}
	gate := &managedAPIGateReader{Reader: effects, gvk: dependencyProvidedBundleGVK}
	reconciler.Client = effects
	reconciler.APIReader = gate
	return reconciler, effects, gate
}

func dependencyProvidedBundle(owned bool, app *applicationv1.OneKSApplication) *unstructured.Unstructured {
	object := emptyManagedResource(app.Spec.ManagedResources[0])
	object.SetResourceVersion("1")
	object.SetUID(types.UID("bundle-uid"))
	if owned {
		object.SetLabels(ownershipLabels(app))
	}
	return object
}

func assertNoRootEffects(t *testing.T, effects *managedEffectClient) {
	t.Helper()
	if len(effects.writes) != 0 {
		t.Fatalf("root effects were not gated: %#v", effects.writes)
	}
}

func assertManagedDependencyPendingStatus(t *testing.T, app *applicationv1.OneKSApplication, dependencyName string) {
	t.Helper()
	for conditionType, want := range map[string]struct {
		status metav1.ConditionStatus
		reason string
	}{
		ConditionPlanValid:         {metav1.ConditionTrue, "Validated"},
		ConditionDependenciesReady: {metav1.ConditionFalse, "DependencyPending"},
		ConditionResourcesReady:    {metav1.ConditionUnknown, "DependenciesPending"},
	} {
		condition := conditionByType(app.Status.Conditions, conditionType)
		if condition == nil || condition.Status != want.status || condition.Reason != want.reason {
			t.Fatalf("condition %s = %#v, want status=%s reason=%s", conditionType, condition, want.status, want.reason)
		}
	}
	if app.Status.LastError != nil && (app.Status.LastError.Reason != "" || app.Status.LastError.Message != "") {
		t.Fatalf("dependency-pending status retained stale validation error: %#v", app.Status.LastError)
	}
	if app.Status.Progress.Current != dependencyName {
		t.Fatalf("dependency-pending progress current = %q, want %q", app.Status.Progress.Current, dependencyName)
	}
	status := resourceStatusByID(app.Status.Resources, "runai-ca-cert")
	if status == nil || status.Phase != "Pending" || status.Reason != "DependenciesPending" ||
		status.Message != "Managed resource readiness is gated by direct dependencies" || status.ReadinessStartedAt != nil {
		t.Fatalf("gated managed resource status = %#v", status)
	}
}

type managedAPIGateReader struct {
	client.Reader
	gvk         schema.GroupVersionKind
	unavailable bool
	noMatchGets int
	gets        int
	err         error
	foreign     *unstructured.Unstructured
}

func (r *managedAPIGateReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if object.GetObjectKind().GroupVersionKind() == r.gvk {
		r.gets++
		switch {
		case r.err != nil:
			return r.err
		case r.unavailable || r.gets <= r.noMatchGets:
			return &meta.NoKindMatchError{GroupKind: r.gvk.GroupKind(), SearchedVersions: []string{r.gvk.Version}}
		case r.foreign != nil:
			current, ok := object.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("managed target %T is not unstructured", object)
			}
			current.Object = r.foreign.DeepCopy().Object
			return nil
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

type managedEffectClient struct {
	client.Client
	writes []string
}

func (c *managedEffectClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if app, ok := object.(*applicationv1.OneKSApplication); ok && app.UID == "" {
		app.UID = types.UID("uid-" + app.Name)
	}
	c.record("create", object)
	return c.Client.Create(ctx, object, options...)
}

func (c *managedEffectClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	c.record("patch", object)
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c *managedEffectClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	c.record("update", object)
	return c.Client.Update(ctx, object, options...)
}

func (c *managedEffectClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	c.record("delete", object)
	return c.Client.Delete(ctx, object, options...)
}

func (c *managedEffectClient) record(verb string, object client.Object) {
	if _, application := object.(*applicationv1.OneKSApplication); application {
		return
	}
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
	c.writes = append(c.writes, verb+":"+kind)
}

var _ client.Client = &managedEffectClient{}
var _ client.Reader = &managedAPIGateReader{}
