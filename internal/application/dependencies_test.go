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
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRootMaterializesFlatTransitiveDependencyGraph(t *testing.T) {
	ctx := context.Background()
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
	reconciler, _ := testReconciler(t, root)

	reconcileOnce(t, ctx, reconciler, root)
	storedRoot := getApplication(t, ctx, reconciler.Client, root)
	if !containsString(storedRoot.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("Root finalizer was not acquired before dependency materialization: %#v", storedRoot.Finalizers)
	}
	assertApplicationNotFound(t, ctx, reconciler.Client, d.Name)
	assertApplicationNotFound(t, ctx, reconciler.Client, e.Name)
	reconcileOnce(t, ctx, reconciler, root)
	for _, plan := range []applicationv1.DependencyPlan{d, e} {
		child := getDependencyApplication(t, ctx, reconciler.Client, plan.Name)
		if len(child.OwnerReferences) != 0 {
			t.Fatalf("dependency %s has ownerReferences: %#v", plan.Name, child.OwnerReferences)
		}
		if child.UID != "" || child.Status.Phase != "" || !containsString(child.Finalizers, applicationv1.ApplicationFinalizer) {
			t.Fatalf("dependency %s received creation-time lifecycle state: %#v", plan.Name, child)
		}
		if len(child.Spec.DependencyPlans) != 0 {
			t.Fatalf("dependency %s contains dependencyPlans: %#v", plan.Name, child.Spec.DependencyPlans)
		}
		if want := dependencyPlanChildSpec(root.Spec.ClusterID, plan); !reflectSpecsEqual(child.Spec, want) {
			t.Fatalf("dependency %s spec differs from child contract:\n got: %#v\nwant: %#v", plan.Name, child.Spec, want)
		}
		if !producerLabelsMatch(child) {
			t.Fatalf("dependency %s producer labels mismatch: %#v", plan.Name, child.Labels)
		}
	}
	storedD := getDependencyApplication(t, ctx, reconciler.Client, d.Name)
	if len(storedD.Spec.Dependencies) != 1 || storedD.Spec.Dependencies[0].Name != e.Name {
		t.Fatalf("D does not directly reference E: %#v", storedD.Spec.Dependencies)
	}
}

func TestDependencyCompatibilityAcceptsAPINormalizedEmptyDependencies(t *testing.T) {
	plan := dependencyPlanForTest(
		"shared-release",
		"shared-chart",
		[]applicationv1.DependencyReference{},
	)
	root := validRootPlanGraph(
		t,
		[]applicationv1.DependencyReference{dependencyReferenceForPlan(plan)},
		[]applicationv1.DependencyPlan{plan},
	)

	expected := expectedDependencyApplication(root, plan)
	if expected.Spec.Dependencies == nil {
		t.Fatal("test setup did not produce a non-nil empty dependencies slice")
	}

	// Reproduce the Kubernetes API JSON round-trip. Dependencies uses
	// json:",omitempty", so a non-nil empty slice is omitted on the wire and
	// decoded back as nil.
	payload, err := json.Marshal(expected)
	if err != nil {
		t.Fatalf("marshal expected dependency: %v", err)
	}

	existing := &applicationv1.OneKSApplication{}
	if err := json.Unmarshal(payload, existing); err != nil {
		t.Fatalf("unmarshal API-normalized dependency: %v", err)
	}

	// Simulate metadata assigned by the Kubernetes API server.
	existing.UID = types.UID("uid-" + plan.Name)

	if existing.Spec.Dependencies != nil {
		t.Fatalf("API round-trip retained empty dependencies unexpectedly: %#v", existing.Spec.Dependencies)
	}

	if conflict := dependencyIdentityError(existing, expected, root.Spec.ClusterID); conflict != nil {
		t.Fatalf("API-normalized empty dependencies were treated as conflicting: %v", conflict)
	}
}

func TestDependencyPreflightReusesCompatibleAndCreatesMissing(t *testing.T) {
	ctx := context.Background()
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	existing := existingDependencyForTest(root, d)
	existing.Annotations = map[string]string{"unrelated.example.test/kept": "true"}
	existing.Finalizers = nil
	reconciler, _ := testReconciler(t, root, existing)

	reconcileOnce(t, ctx, reconciler, root)
	reused := getDependencyApplication(t, ctx, reconciler.Client, d.Name)
	if reused.Annotations["unrelated.example.test/kept"] != "true" {
		t.Fatalf("compatible dependency metadata was mutated: %#v", reused.Annotations)
	}
	if containsString(reused.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("compatible dependency was mutated instead of reusing it unchanged: %#v", reused.Finalizers)
	}
	getDependencyApplication(t, ctx, reconciler.Client, e.Name)
	reconcileOnce(t, ctx, reconciler, reused)
	reused = getDependencyApplication(t, ctx, reconciler.Client, d.Name)
	if !containsString(reused.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("compatible dependency did not acquire its finalizer through normal reconciliation: %#v", reused.Finalizers)
	}
}

func TestDependencyPreflightConflictCreatesNothing(t *testing.T) {
	ctx := context.Background()
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	conflictingE := existingDependencyForTest(root, e)
	conflictingE.Spec.Release.Version = "different-version"
	reconciler, _ := testReconciler(t, root, conflictingE)

	reconcileOnce(t, ctx, reconciler, root)
	assertApplicationNotFound(t, ctx, reconciler.Client, d.Name)
	assertOwnEffectsAbsent(t, ctx, reconciler.Client, root)
	stored := getApplication(t, ctx, reconciler.Client, root)
	assertDependencyCondition(t, stored, metav1.ConditionFalse, "DependencyConflict")
	if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.LastError == nil || stored.Status.LastError.Reason != "DependencyConflict" {
		t.Fatalf("dependency conflict status mismatch: %#v", stored.Status)
	}
}

func TestDependencyPreflightUsesAuthoritativeReaderForConflicts(t *testing.T) {
	ctx := context.Background()
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	conflictingE := existingDependencyForTest(root, e)
	conflictingE.Spec.Release.Version = "conflicting-version"
	reconciler, _ := testReconciler(t, root)
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Client.Scheme()).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "catalogue-workloads"}},
		conflictingE,
	).Build()

	reconcileOnce(t, ctx, reconciler, root)
	assertApplicationNotFound(t, ctx, reconciler.Client, d.Name)
	assertApplicationNotFound(t, ctx, reconciler.Client, e.Name)
	stored := getApplication(t, ctx, reconciler.Client, root)
	assertDependencyCondition(t, stored, metav1.ConditionFalse, "DependencyConflict")
}

func TestDependencyPreflightReusesExactAuthoritativeObject(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("existing-release", "existing-chart", nil)
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	existing := existingDependencyForTest(root, plan)
	reconciler, _ := testReconciler(t, root)
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Client.Scheme()).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "catalogue-workloads"}},
		existing,
	).Build()

	reconcileOnce(t, ctx, reconciler, root)
	assertApplicationNotFound(t, ctx, reconciler.Client, plan.Name)
	stored := getApplication(t, ctx, reconciler.Client, root)
	assertDependencyCondition(t, stored, metav1.ConditionFalse, "DependencyMissing")
	if stored.Status.LastError != nil && stored.Status.LastError.Reason == "DependencyConflict" {
		t.Fatalf("exact authoritative dependency was treated as conflicting: %#v", stored.Status)
	}
}

func TestReconcileHelmChartUsesAuthoritativeExistingState(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	existing := desiredHelmChart(app)
	existing.SetUID(types.UID("helm-uid"))
	existing.SetResourceVersion("7")
	reconciler, recorder := testReconciler(t, app)
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Client.Scheme()).WithObjects(existing).Build()

	if err := reconciler.reconcileHelmChart(ctx, app); err != nil {
		t.Fatalf("reconcile authoritative existing HelmChart: %v", err)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("authoritative existing HelmChart caused a duplicate mutation: %#v", recorder.childWrites)
	}
	assertNotFound(t, ctx, reconciler.Client, helmChartObject(app.Spec.Release.ReleaseName))
}

func TestDeletionUsesAuthoritativeHelmChartState(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Spec.ManagedResources = nil
	refreshDigest(t, app)
	app.Labels = producerLabels(app)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	now := metav1.Now()
	app.DeletionTimestamp = &now
	existing := desiredHelmChart(app)
	existing.SetUID(types.UID("helm-uid"))
	existing.SetResourceVersion("7")
	reconciler, recorder := testReconciler(t, app)
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Client.Scheme()).WithObjects(existing).Build()

	result := reconcileOnce(t, ctx, reconciler, app)
	if result.RequeueAfter == 0 && !result.Requeue {
		t.Fatalf("authoritative HelmChart deletion did not requeue: %#v", result)
	}
	if len(recorder.childWrites) != 1 || recorder.childWrites[0] != "delete:HelmChart" {
		t.Fatalf("authoritative HelmChart was not deleted first: %#v", recorder.childWrites)
	}
	stored := getApplication(t, ctx, reconciler.Client, app)
	if !containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("application finalizer was removed before authoritative HelmChart deletion completed: %#v", stored.Finalizers)
	}
}

func TestDependencyPreflightRejectsIncompatibleExistingApplications(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplication)
	}{
		{"different plan digest", func(app *applicationv1.OneKSApplication) { app.Spec.PlanDigest = validDependencyDigest() }},
		{"wrong producer labels", func(app *applicationv1.OneKSApplication) { delete(app.Labels, LabelProducer) }},
		{"Root role", func(app *applicationv1.OneKSApplication) { app.Spec.Role = applicationv1.ApplicationRoleRoot }},
		{"owner reference", func(app *applicationv1.OneKSApplication) {
			app.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "owner-uid"}}
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			plan := dependencyPlanForTest("shared-release", "shared-chart", nil)
			root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
			root.Finalizers = []string{applicationv1.ApplicationFinalizer}
			existing := existingDependencyForTest(root, plan)
			test.mutate(existing)
			reconciler, _ := testReconciler(t, root, existing)

			reconcileOnce(t, ctx, reconciler, root)
			stored := getApplication(t, ctx, reconciler.Client, root)
			assertDependencyCondition(t, stored, metav1.ConditionFalse, "DependencyConflict")
			assertOwnEffectsAbsent(t, ctx, reconciler.Client, root)
		})
	}
}

func TestDependencyCreateAlreadyExistsRaceRequeuesWithoutAssumingCompatibility(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("raced-release", "raced-chart", nil)
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, _ := testReconciler(t, root)
	reconciler.Client = &alreadyExistsDependencyClient{Client: reconciler.Client, name: plan.Name}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(root)})
	if err != nil {
		t.Fatalf("reconcile create race: %v", err)
	}
	if result.RequeueAfter == 0 && !result.Requeue {
		t.Fatalf("create race was not requeued: %#v", result)
	}
	assertApplicationNotFound(t, ctx, reconciler.Client, plan.Name)
	assertOwnEffectsAbsent(t, ctx, reconciler.Client, root)
}

func TestSharedDependencyIsReusedAndConflictingPlanFailsClosed(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("shared-release", "shared-chart", nil)
	rootA := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	rootA.Name = "root-a"
	rootA.UID = "root-a-uid"
	rootA.Finalizers = []string{applicationv1.ApplicationFinalizer}
	rootB := rootA.DeepCopy()
	rootB.Name = "root-b"
	rootB.UID = "root-b-uid"
	reconciler, _ := testReconciler(t, rootA, rootB)

	reconcileOnce(t, ctx, reconciler, rootA)
	shared := getDependencyApplication(t, ctx, reconciler.Client, plan.Name)
	shared.UID = types.UID("shared-dependency-uid")
	if err := reconciler.Update(ctx, shared); err != nil {
		t.Fatalf("simulate API-assigned dependency UID: %v", err)
	}
	reconcileOnce(t, ctx, reconciler, rootB)
	storedB := getApplication(t, ctx, reconciler.Client, rootB)
	if storedB.Status.LastError != nil && storedB.Status.LastError.Reason == "DependencyConflict" {
		t.Fatalf("exact shared dependency was not reused: %#v", storedB.Status)
	}
	list := &applicationv1.OneKSApplicationList{}
	if err := reconciler.List(ctx, list, client.InNamespace(applicationv1.ApplicationNamespace)); err != nil {
		t.Fatalf("list applications: %v", err)
	}
	count := 0
	for index := range list.Items {
		if list.Items[index].Name == plan.Name {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared dependency count = %d, want 1", count)
	}

	conflictingPlan := plan
	conflictingPlan.Release.ValuesContent = "mode: conflicting\n"
	refreshDependencyPlanDigestForTest(rootA.Spec.ClusterID, &conflictingPlan)
	rootC := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(conflictingPlan)}, []applicationv1.DependencyPlan{conflictingPlan})
	rootC.Name = "root-c"
	rootC.UID = "root-c-uid"
	rootC.Finalizers = []string{applicationv1.ApplicationFinalizer}
	if err := reconciler.Create(ctx, rootC); err != nil {
		t.Fatalf("create conflicting Root: %v", err)
	}
	reconcileOnce(t, ctx, reconciler, rootC)
	stored := getApplication(t, ctx, reconciler.Client, rootC)
	assertDependencyCondition(t, stored, metav1.ConditionFalse, "DependencyConflict")
}

func TestDependencyReadinessGatesOwnEffects(t *testing.T) {
	states := []struct {
		name          string
		dependency    func(*applicationv1.OneKSApplication)
		wantReason    string
		wantPhase     applicationv1.ApplicationPhase
		wantOwnEffect bool
	}{
		{"missing", nil, "DependencyMissing", applicationv1.PhaseInstalling, false},
		{"installing", func(app *applicationv1.OneKSApplication) {
			setDependencyStatus(app, applicationv1.PhaseInstalling, false, "")
		}, "DependencyInstalling", applicationv1.PhaseInstalling, false},
		{"failed", func(app *applicationv1.OneKSApplication) {
			setDependencyStatus(app, applicationv1.PhaseFailed, false, "InstallerJobFailed")
		}, "DependencyFailed", applicationv1.PhaseFailed, false},
		{"ready", func(app *applicationv1.OneKSApplication) {
			setDependencyStatus(app, applicationv1.PhaseReady, true, "")
		}, "DependenciesReady", applicationv1.PhaseInstalling, true},
	}
	for _, test := range states {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			plan := dependencyPlanForTest("oneks-e", "chart-e", nil)
			consumer := dependencyConsumerForTest(t, "oneks-d", dependencyReferenceForPlan(plan))
			consumer.Finalizers = []string{applicationv1.ApplicationFinalizer}
			objects := []client.Object{consumer}
			if test.dependency != nil {
				dependency := existingDependencyForTest(consumer, plan)
				dependency.Generation = 1
				test.dependency(dependency)
				objects = append(objects, dependency)
			}
			reconciler, _ := testReconciler(t, objects...)

			reconcileOnce(t, ctx, reconciler, consumer)
			stored := getApplication(t, ctx, reconciler.Client, consumer)
			assertDependencyCondition(t, stored, map[bool]metav1.ConditionStatus{true: metav1.ConditionTrue, false: metav1.ConditionFalse}[test.wantOwnEffect], test.wantReason)
			if stored.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %s, want %s: %#v", stored.Status.Phase, test.wantPhase, stored.Status)
			}
			helm := helmChartObject(consumer.Spec.Release.ReleaseName)
			if test.wantOwnEffect {
				assertExists(t, ctx, reconciler.Client, helm, HelmChartNamespace, consumer.Spec.Release.ReleaseName)
			} else {
				assertOwnEffectsAbsent(t, ctx, reconciler.Client, consumer)
			}
		})
	}
}

func TestDependencyWithNoDirectDependenciesProceeds(t *testing.T) {
	ctx := context.Background()
	app := validDependencyPlanApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, _ := testReconciler(t, app)

	reconcileOnce(t, ctx, reconciler, app)
	assertExists(t, ctx, reconciler.Client, helmChartObject(app.Spec.Release.ReleaseName), HelmChartNamespace, app.Spec.Release.ReleaseName)
	stored := getApplication(t, ctx, reconciler.Client, app)
	assertDependencyCondition(t, stored, metav1.ConditionTrue, "NoDependencies")
}

func TestRootWaitsForDirectDependencyNotMerelyTransitiveDependency(t *testing.T) {
	ctx := context.Background()
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	dependencyD := existingDependencyForTest(root, d)
	dependencyD.Generation = 1
	setDependencyStatus(dependencyD, applicationv1.PhaseInstalling, false, "")
	dependencyE := existingDependencyForTest(root, e)
	dependencyE.Generation = 1
	setDependencyStatus(dependencyE, applicationv1.PhaseReady, true, "")
	reconciler, _ := testReconciler(t, root, dependencyD, dependencyE)

	reconcileOnce(t, ctx, reconciler, root)
	assertOwnEffectsAbsent(t, ctx, reconciler.Client, root)
	storedRoot := getApplication(t, ctx, reconciler.Client, root)
	if storedRoot.Status.Progress.Total != 2 || storedRoot.Status.Progress.Completed != 0 {
		t.Fatalf("Root progress counted transitive E: %#v", storedRoot.Status.Progress)
	}

	storedD := getDependencyApplication(t, ctx, reconciler.Client, d.Name)
	setDependencyStatus(storedD, applicationv1.PhaseReady, true, "")
	if err := reconciler.Status().Update(ctx, storedD); err != nil {
		t.Fatalf("mark D Ready: %v", err)
	}
	reconcileOnce(t, ctx, reconciler, root)
	assertExists(t, ctx, reconciler.Client, helmChartObject(root.Spec.Release.ReleaseName), HelmChartNamespace, root.Spec.Release.ReleaseName)
	storedRoot = getApplication(t, ctx, reconciler.Client, root)
	if storedRoot.Status.Progress.Total != 2 || storedRoot.Status.Progress.Completed != 1 {
		t.Fatalf("Root direct dependency progress mismatch: %#v", storedRoot.Status.Progress)
	}
}

func TestDirectDependencyProgressTotals(t *testing.T) {
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	d := dependencyConsumerForTest(t, "oneks-d", dependencyReferenceForPlan(e))
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(e)}, []applicationv1.DependencyPlan{e})
	if got := applicationProgressTotal(root); got != 2 {
		t.Fatalf("Root total = %d, want direct dependency + Helm = 2", got)
	}
	if got := applicationProgressTotal(d); got != 2 {
		t.Fatalf("Dependency total = %d, want direct dependency + Helm = 2", got)
	}
	eApp := validDependencyPlanApplication(t)
	if got := applicationProgressTotal(eApp); got != 1 {
		t.Fatalf("leaf total = %d, want Helm = 1", got)
	}
}

func TestObserveRootDoesNotMaterializeDependencies(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("observe-dependency", "observe-chart", nil)
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Spec.ExecutionMode = applicationv1.ExecutionModeObserve
	refreshPlanDigest(root)
	root.Labels = producerLabels(root)
	reconciler, _ := testReconciler(t, root)

	reconcileOnce(t, ctx, reconciler, root)
	assertApplicationNotFound(t, ctx, reconciler.Client, plan.Name)
	assertOwnEffectsAbsent(t, ctx, reconciler.Client, root)
	stored := getApplication(t, ctx, reconciler.Client, root)
	if stored.Status.Phase != applicationv1.PhaseObserving {
		t.Fatalf("Observe Root phase = %s, want Observing", stored.Status.Phase)
	}
	assertDependencyCondition(t, stored, metav1.ConditionFalse, "DependencyMissing")
}

func TestDeletingRootNeverCreatesMissingDependency(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("missing-dependency", "missing-chart", nil)
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	now := metav1.Now()
	root.DeletionTimestamp = &now
	reconciler, _ := testReconciler(t, root)

	reconcileOnce(t, ctx, reconciler, root)
	assertApplicationNotFound(t, ctx, reconciler.Client, plan.Name)
}

func TestDeletingOneOfTwoExecuteConsumersKeepsSharedDependency(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("shared-gc", "shared-chart", nil)
	rootA := deletingRootForTest(t, "root-a", plan)
	rootB := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	rootB.Name = "root-b"
	rootB.UID = "root-b-uid"
	dependency := existingDependencyForTest(rootA, plan)
	reconciler, _ := testReconciler(t, rootA, rootB, dependency)

	reconcileOnce(t, ctx, reconciler, rootA)
	stored := getDependencyApplication(t, ctx, reconciler.Client, plan.Name)
	if !stored.DeletionTimestamp.IsZero() {
		t.Fatalf("shared dependency was deleted while Root B still consumed it: %s", stored.DeletionTimestamp)
	}
}

func TestDeletingLastExecuteConsumerDeletesDependency(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("last-consumer", "last-chart", nil)
	root := deletingRootForTest(t, "root", plan)
	dependency := existingDependencyForTest(root, plan)
	reconciler, _ := testReconciler(t, root, dependency)

	reconcileOnce(t, ctx, reconciler, root)
	assertDependencyTerminating(t, ctx, reconciler.Client, plan.Name)
}

func TestDependencyWithoutFinalizerGetsOneBeforeLastConsumerDeletion(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("unfinalized-last-consumer", "unfinalized-chart", nil)
	root := deletingRootForTest(t, "root", plan)
	dependency := existingDependencyForTest(root, plan)
	dependency.Finalizers = nil
	dependency.Spec.Dependencies = []applicationv1.DependencyReference{}
	dependency.Spec.DependencyPlans = []applicationv1.DependencyPlan{}
	refreshDigest(t, dependency)
	wantSpec := dependency.DeepCopy().Spec
	wantSpecJSON, err := json.Marshal(wantSpec)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, _ := testReconciler(t, root, dependency)
	recorder := &applicationFinalizerMutationClient{
		Client: reconciler.Client, key: client.ObjectKeyFromObject(dependency),
	}
	reconciler.Client = recorder

	result := reconcileOnce(t, ctx, reconciler, root)
	if result.RequeueAfter == 0 && !result.Requeue {
		t.Fatalf("dependency finalizer installation did not requeue: %#v", result)
	}
	storedDependency := getDependencyApplication(t, ctx, reconciler.Client, plan.Name)
	if !containsString(storedDependency.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("dependency cleanup finalizer was not installed: %#v", storedDependency.Finalizers)
	}
	if !storedDependency.DeletionTimestamp.IsZero() {
		t.Fatalf("dependency was deleted while its finalizer was first installed: %s", storedDependency.DeletionTimestamp)
	}
	storedRoot := getApplication(t, ctx, reconciler.Client, root)
	if !containsString(storedRoot.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("consumer finalizer was removed before dependency deletion retry: %#v", storedRoot.Finalizers)
	}
	assertApplicationSpecJSONUnchanged(t, wantSpecJSON, storedDependency.Spec)
	assertMetadataOnlyApplicationPatches(t, recorder, 1)

	reconcileOnce(t, ctx, reconciler, root)
	assertDependencyTerminating(t, ctx, reconciler.Client, plan.Name)
}

func TestRetainPolicyDependencySurvivesLastConsumerDeletion(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("retained-gc", "retained-chart", nil)
	plan.DeletionPolicy = applicationv1.DeletionPolicyRetain
	refreshDependencyPlanDigestForTest("42", &plan)
	root := deletingRootForTest(t, "root", plan)
	dependency := existingDependencyForTest(root, plan)
	reconciler, _ := testReconciler(t, root, dependency)

	reconcileOnce(t, ctx, reconciler, root)
	stored := getDependencyApplication(t, ctx, reconciler.Client, plan.Name)
	if !stored.DeletionTimestamp.IsZero() {
		t.Fatalf("Retain dependency was deleted: %s", stored.DeletionTimestamp)
	}
}

func TestDeletingConsumersDoNotProtectDependencyGC(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("deleting-consumers", "shared-chart", nil)
	rootA := deletingRootForTest(t, "root-a", plan)
	rootB := deletingRootForTest(t, "root-b", plan)
	dependency := existingDependencyForTest(rootA, plan)
	reconciler, _ := testReconciler(t, rootA, rootB, dependency)

	reconcileOnce(t, ctx, reconciler, rootA)
	assertDependencyTerminating(t, ctx, reconciler.Client, plan.Name)
}

func TestObserveConsumersDoNotProtectDependencyGC(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("observe-consumer", "shared-chart", nil)
	root := deletingRootForTest(t, "root", plan)
	observer := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	observer.Name = "observer"
	observer.UID = "observer-uid"
	observer.Spec.ExecutionMode = applicationv1.ExecutionModeObserve
	refreshPlanDigest(observer)
	observer.Labels = producerLabels(observer)
	dependency := existingDependencyForTest(root, plan)
	reconciler, _ := testReconciler(t, root, observer, dependency)

	reconcileOnce(t, ctx, reconciler, root)
	assertDependencyTerminating(t, ctx, reconciler.Client, plan.Name)
}

func TestAuthoritativeConsumerLookupProtectsAgainstCachedFalseZero(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("authoritative-consumer", "shared-chart", nil)
	rootA := deletingRootForTest(t, "root-a", plan)
	rootB := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	rootB.Name = "root-b"
	rootB.UID = "root-b-uid"
	dependency := existingDependencyForTest(rootA, plan)
	reconciler, _ := testReconciler(t, rootA, dependency)
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Client.Scheme()).WithObjects(rootA, rootB, dependency).Build()

	reconcileOnce(t, ctx, reconciler, rootA)
	stored := getDependencyApplication(t, ctx, reconciler.Client, plan.Name)
	if !stored.DeletionTimestamp.IsZero() {
		t.Fatalf("authoritative live consumer did not protect dependency: %s", stored.DeletionTimestamp)
	}
}

func TestMismatchedDependencyIsNeverAutomaticallyDeleted(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("foreign-dependency", "shared-chart", nil)
	root := deletingRootForTest(t, "root", plan)
	foreign := existingDependencyForTest(root, plan)
	foreign.Spec.Role = applicationv1.ApplicationRoleRoot
	reconciler, _ := testReconciler(t, root, foreign)

	reconcileOnce(t, ctx, reconciler, root)
	stored := getDependencyApplication(t, ctx, reconciler.Client, plan.Name)
	if !stored.DeletionTimestamp.IsZero() {
		t.Fatalf("mismatched dependency was automatically deleted: %s", stored.DeletionTimestamp)
	}
}

func TestDependencyGCReleasesOnlyDirectEdges(t *testing.T) {
	ctx := context.Background()
	e := dependencyPlanForTest("oneks-e-gc", "chart-e", nil)
	d := dependencyPlanForTest("oneks-d-gc", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
	root.Name = "root"
	root.UID = "root-uid"
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	now := metav1.Now()
	root.DeletionTimestamp = &now
	dependencyD := existingDependencyForTest(root, d)
	dependencyD.Finalizers = nil
	dependencyE := existingDependencyForTest(root, e)
	reconciler, _ := testReconciler(t, root, dependencyD, dependencyE)

	reconcileOnce(t, ctx, reconciler, root)
	storedD := getDependencyApplication(t, ctx, reconciler.Client, d.Name)
	if !containsString(storedD.Finalizers, applicationv1.ApplicationFinalizer) || !storedD.DeletionTimestamp.IsZero() {
		t.Fatalf("D was not finalized before deletion: %#v", storedD.ObjectMeta)
	}
	storedE := getDependencyApplication(t, ctx, reconciler.Client, e.Name)
	if !storedE.DeletionTimestamp.IsZero() {
		t.Fatalf("E disappeared before D had a cleanup path: %s", storedE.DeletionTimestamp)
	}

	reconcileOnce(t, ctx, reconciler, root)
	assertDependencyTerminating(t, ctx, reconciler.Client, d.Name)
	storedE = getDependencyApplication(t, ctx, reconciler.Client, e.Name)
	if !storedE.DeletionTimestamp.IsZero() {
		t.Fatalf("Root directly deleted transitive E: %s", storedE.DeletionTimestamp)
	}

	reconcileOnce(t, ctx, reconciler, dependencyD)
	assertDependencyTerminating(t, ctx, reconciler.Client, e.Name)
}

func TestTerminatingDependencyIsNeverReady(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("terminating-dependency", "shared-chart", nil)
	consumer := dependencyConsumerForTest(t, "consumer", dependencyReferenceForPlan(plan))
	dependency := existingDependencyForTest(consumer, plan)
	dependency.Generation = 1
	setDependencyStatus(dependency, applicationv1.PhaseReady, true, "")
	now := metav1.Now()
	dependency.DeletionTimestamp = &now
	if dependencyStatusReady(dependency) {
		t.Fatal("terminating dependency satisfied the Ready predicate")
	}
	reconciler, _ := testReconciler(t, consumer, dependency)

	observed, err := reconciler.observeDependencies(ctx, consumer)
	if err != nil {
		t.Fatalf("observe terminating dependency: %v", err)
	}
	if observed.ready || observed.completed != 0 || observed.reason != "DependencyTerminating" {
		t.Fatalf("terminating dependency observation = %#v", observed)
	}
}

func TestRootWaitsForTerminatingDeterministicDependency(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("terminating-preflight", "shared-chart", nil)
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	dependency := existingDependencyForTest(root, plan)
	now := metav1.Now()
	dependency.DeletionTimestamp = &now
	reconciler, _ := testReconciler(t, root, dependency)

	reconcileOnce(t, ctx, reconciler, root)
	stored := getApplication(t, ctx, reconciler.Client, root)
	assertDependencyCondition(t, stored, metav1.ConditionFalse, "DependencyTerminating")
	if stored.Status.LastError != nil && stored.Status.LastError.Reason == "DependencyConflict" {
		t.Fatalf("terminating deterministic dependency was reported as conflict: %#v", stored.Status)
	}
}

func TestAuthoritativeTerminatingDependencyOverridesStaleCachedReady(t *testing.T) {
	ctx := context.Background()
	plan := dependencyPlanForTest("authoritative-terminating", "shared-chart", nil)
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	staleReady := existingDependencyForTest(root, plan)
	staleReady.Generation = 1
	setDependencyStatus(staleReady, applicationv1.PhaseReady, true, "")
	authoritativeTerminating := staleReady.DeepCopy()
	now := metav1.Now()
	authoritativeTerminating.DeletionTimestamp = &now
	reconciler, _ := testReconciler(t, root, staleReady)
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Client.Scheme()).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "catalogue-workloads"}},
		authoritativeTerminating,
	).Build()

	reconcileOnce(t, ctx, reconciler, root)
	stored := getApplication(t, ctx, reconciler.Client, root)
	assertDependencyCondition(t, stored, metav1.ConditionFalse, "DependencyTerminating")
	ready := meta.FindStatusCondition(stored.Status.Conditions, ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "DependencyTerminating" {
		t.Fatalf("Ready condition was overridden by stale cached dependency state: %#v", ready)
	}
	if stored.Status.Phase == applicationv1.PhaseReady || stored.Status.Progress.Completed != 0 {
		t.Fatalf("stale cached Ready dependency advanced Root status: %#v", stored.Status)
	}
	assertOwnEffectsAbsent(t, ctx, reconciler.Client, root)
}

func TestDependencyEventMapsOnlyDirectConsumersThroughIndex(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := applicationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add application scheme: %v", err)
	}
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	dPlan := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})
	d := expectedDependencyApplication(validRootPlan(t), dPlan)
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(dPlan)}, []applicationv1.DependencyPlan{dPlan, e})
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&applicationv1.OneKSApplication{}, dependencyNameIndex, directDependencyNames).
		WithObjects(d, root).Build()
	reconciler := &Reconciler{Client: base}
	changedE := &applicationv1.OneKSApplication{ObjectMeta: metav1.ObjectMeta{Namespace: applicationv1.ApplicationNamespace, Name: e.Name}}

	requests := reconciler.requestsForDependencyConsumers(context.Background(), changedE)
	if len(requests) != 1 || requests[0].Name != d.Name {
		t.Fatalf("E event requests = %#v, want only direct consumer D", requests)
	}
	changedD := &applicationv1.OneKSApplication{ObjectMeta: metav1.ObjectMeta{Namespace: applicationv1.ApplicationNamespace, Name: d.Name}}
	requests = reconciler.requestsForDependencyConsumers(context.Background(), changedD)
	if len(requests) != 1 || requests[0].Name != root.Name {
		t.Fatalf("D event requests = %#v, want Root", requests)
	}
}

type alreadyExistsDependencyClient struct {
	client.Client
	name string
}

func (c *alreadyExistsDependencyClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if _, ok := object.(*applicationv1.OneKSApplication); ok && object.GetName() == c.name {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: applicationv1.GroupVersion.Group, Resource: "oneksapplications"}, c.name)
	}
	return c.Client.Create(ctx, object, options...)
}

func dependencyConsumerForTest(t *testing.T, releaseName string, dependency applicationv1.DependencyReference) *applicationv1.OneKSApplication {
	t.Helper()
	app := validDependencyPlanApplication(t)
	app.Name = dependencyApplicationName(releaseName)
	app.Spec.Release.ReleaseName = releaseName
	app.Spec.Dependencies = []applicationv1.DependencyReference{dependency}
	refreshPlanDigest(app)
	app.Labels = producerLabels(app)
	return app
}

func existingDependencyForTest(root *applicationv1.OneKSApplication, plan applicationv1.DependencyPlan) *applicationv1.OneKSApplication {
	app := expectedDependencyApplication(root, plan)
	app.UID = types.UID("uid-" + plan.Name)
	return app
}

func deletingRootForTest(t *testing.T, name string, plan applicationv1.DependencyPlan) *applicationv1.OneKSApplication {
	t.Helper()
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Name = name
	root.UID = types.UID(name + "-uid")
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	now := metav1.Now()
	root.DeletionTimestamp = &now
	return root
}

func assertDependencyTerminating(t *testing.T, ctx context.Context, kubeClient client.Client, name string) {
	t.Helper()
	dependency := getDependencyApplication(t, ctx, kubeClient, name)
	if dependency.DeletionTimestamp.IsZero() {
		t.Fatalf("dependency %s is not terminating", name)
	}
}

func setDependencyStatus(app *applicationv1.OneKSApplication, phase applicationv1.ApplicationPhase, ready bool, failureReason string) {
	app.Status.ObservedGeneration = app.Generation
	app.Status.ObservedPlanDigest = app.Spec.PlanDigest
	app.Status.Phase = phase
	conditionStatus := metav1.ConditionFalse
	if ready {
		conditionStatus = metav1.ConditionTrue
	}
	app.Status.Conditions = []metav1.Condition{{
		Type: ConditionReady, Status: conditionStatus, ObservedGeneration: app.Generation,
		Reason: string(phase), Message: fmt.Sprintf("dependency is %s", phase),
	}}
	if failureReason != "" {
		app.Status.LastError = &applicationv1.ApplicationError{Reason: failureReason, Message: "dependency failed"}
	}
}

func getDependencyApplication(t *testing.T, ctx context.Context, kubeClient client.Client, name string) *applicationv1.OneKSApplication {
	t.Helper()
	app := &applicationv1.OneKSApplication{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: applicationv1.ApplicationNamespace, Name: name}, app); err != nil {
		t.Fatalf("get dependency %s: %v", name, err)
	}
	return app
}

func assertApplicationNotFound(t *testing.T, ctx context.Context, kubeClient client.Client, name string) {
	t.Helper()
	app := &applicationv1.OneKSApplication{}
	err := kubeClient.Get(ctx, types.NamespacedName{Namespace: applicationv1.ApplicationNamespace, Name: name}, app)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected dependency %s to be absent, got %v", name, err)
	}
}

func assertOwnEffectsAbsent(t *testing.T, ctx context.Context, kubeClient client.Client, app *applicationv1.OneKSApplication) {
	t.Helper()
	assertNotFound(t, ctx, kubeClient, helmChartObject(app.Spec.Release.ReleaseName))
}

func assertDependencyCondition(t *testing.T, app *applicationv1.OneKSApplication, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(app.Status.Conditions, ConditionDependenciesReady)
	if condition == nil || condition.Status != status || condition.Reason != reason {
		t.Fatalf("DependenciesReady = %#v, want status=%s reason=%s", condition, status, reason)
	}
}

func reflectSpecsEqual(first, second applicationv1.OneKSApplicationSpec) bool {
	return reflect.DeepEqual(first, second)
}
