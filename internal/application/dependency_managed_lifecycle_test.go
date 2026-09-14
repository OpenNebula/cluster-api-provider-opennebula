package application

import (
	"context"
	"reflect"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func managedEmbeddedPlanForTest() applicationv1.DependencyPlan {
	plan := dependencyPlanForTest("managed-child", "managed-chart", nil)
	plan.ManagedResources = []applicationv1.ManagedResourceSpec{managedConfigMap("settings", "monitoring", "settings", nil)}
	return plan
}

func TestDependencyManagedRootMaterializesResourcesAndDigest(t *testing.T) {
	ctx := context.Background()
	plan := managedEmbeddedPlanForTest()
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	r, _ := testReconciler(t, root)
	reconcileOnce(t, ctx, r, root)
	child := getDependencyApplication(t, ctx, r.Client, plan.Name)
	if !reflect.DeepEqual(child.Spec.ManagedResources, plan.ManagedResources) {
		t.Fatalf("materialized child lost managed resources: %#v", child.Spec)
	}
	// Fake clients do not allocate the UID required by runtime validation.
	child.UID = "materialized-child-uid"
	assertPlanValid(t, child)
	assertNotFound(t, ctx, r.Client, emptyManagedResource(plan.ManagedResources[0]))
}

func TestDependencyManagedMalformedEmbeddedPlanHasNoEffects(t *testing.T) {
	ctx := context.Background()
	plan := managedEmbeddedPlanForTest()
	plan.ManagedResources[0].ManifestJSON = "{"
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Finalizers = []string{applicationv1.ApplicationFinalizer}
	assertPlanError(t, root, "InvalidManagedManifest")
	r, recorder := testReconciler(t, root)
	reconcileOnce(t, ctx, r, root)
	assertApplicationNotFound(t, ctx, r.Client, plan.Name)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("invalid embedded resource caused writes: %v", recorder.childWrites)
	}
}

func TestDependencyManagedSharedResourceChangeConflicts(t *testing.T) {
	ctx := context.Background()
	original := managedEmbeddedPlanForTest()
	rootA := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(original)}, []applicationv1.DependencyPlan{original})
	existing := existingDependencyForTest(rootA, original)
	changed := *original.DeepCopy()
	changed.ManagedResources[0].ManifestJSON += " "
	rootB := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(changed)}, []applicationv1.DependencyPlan{changed})
	rootB.Finalizers = []string{applicationv1.ApplicationFinalizer}
	r, recorder := testReconciler(t, rootB, existing)
	reconcileOnce(t, ctx, r, rootB)
	assertDependencyCondition(t, getApplication(t, ctx, r.Client, rootB), metav1.ConditionFalse, "DependencyConflict")
	stored := getDependencyApplication(t, ctx, r.Client, original.Name)
	if !reflect.DeepEqual(stored.Spec, existing.Spec) || len(recorder.childWrites) != 0 {
		t.Fatal("conflict mutated the shared child or its resources")
	}
}

func TestDependencyManagedDirectDependenciesGateWrites(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "missing"
		if present {
			name = "unready"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			prerequisite := dependencyPlanForTest("prerequisite", "prerequisite-chart", nil)
			app := managedDependencyForTest(t)
			app.Spec.Dependencies = []applicationv1.DependencyReference{dependencyReferenceForPlan(prerequisite)}
			refreshOwnedPlan(t, app)
			objects := []client.Object{app}
			if present {
				objects = append(objects, existingDependencyForTest(app, prerequisite))
			}
			r, recorder := testReconciler(t, objects...)
			reconcileOnce(t, ctx, r, app)
			assertNotFound(t, ctx, r.Client, emptyManagedResource(app.Spec.ManagedResources[0]))
			assertOwnEffectsAbsent(t, ctx, r.Client, app)
			if len(recorder.childWrites) != 0 {
				t.Fatalf("blocked dependency caused writes: %v", recorder.childWrites)
			}
		})
	}
}

func TestDependencyManagedOwnershipConflictHasNoEffects(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		name := "install"
		if deleting {
			name = "delete"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			app := managedDependencyForTest(t)
			app.Spec.ManagedResources = append(app.Spec.ManagedResources, managedConfigMap("foreign", "monitoring", "foreign", nil))
			refreshOwnedPlan(t, app)
			if deleting {
				now := metav1.Now()
				app.DeletionTimestamp = &now
			}
			owned, err := desiredManagedResource(app, app.Spec.ManagedResources[0])
			if err != nil {
				t.Fatal(err)
			}
			foreign := emptyManagedResource(app.Spec.ManagedResources[1])
			foreign.Object["data"] = map[string]any{"keep": "untouched"}
			r, recorder := testReconciler(t, app, owned, foreign)
			reconcileOnce(t, ctx, r, app)
			if len(recorder.childWrites) != 0 {
				t.Fatalf("ownership conflict caused mutations: %v", recorder.childWrites)
			}
			assertExists(t, ctx, r.Client, &corev1.ConfigMap{}, "monitoring", "settings")
			stored := &corev1.ConfigMap{}
			assertExists(t, ctx, r.Client, stored, "monitoring", "foreign")
			if stored.Data["keep"] != "untouched" {
				t.Fatal("foreign resource was altered")
			}
		})
	}
}

func TestDependencyManagedCreatesTargetNamespace(t *testing.T) {
	ctx := context.Background()
	app := managedDependencyForTest(t)
	app.Spec.Release.CreateNamespace = false
	app.Spec.ManagedResources[0].DependsOn = []string{"namespace"}
	app.Spec.ManagedResources = append(app.Spec.ManagedResources, applicationv1.ManagedResourceSpec{
		ID: "namespace", Scope: applicationv1.ManagedResourceScopeCluster, APIVersion: "v1", Kind: "Namespace", Name: "monitoring",
		ManifestJSON: `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"monitoring"}}`,
		Readiness:    applicationv1.ManagedResourceReadiness{TimeoutSeconds: 60}, DeletionPolicy: applicationv1.DeletionPolicyRetain,
	})
	refreshOwnedPlan(t, app)
	r, _ := testReconciler(t, app)
	reconcileOnce(t, ctx, r, app)
	assertExists(t, ctx, r.Client, &corev1.Namespace{}, "", "monitoring")
	assertExists(t, ctx, r.Client, &corev1.ConfigMap{}, "monitoring", "settings")
}

func TestDependencyManagedLastRootGCCleansDeleteAndKeepsRetain(t *testing.T) {
	ctx := context.Background()
	plan := managedEmbeddedPlanForTest()
	retained := managedConfigMap("retained", "monitoring", "retained", nil)
	retained.DeletionPolicy = applicationv1.DeletionPolicyRetain
	plan.ManagedResources = append(plan.ManagedResources, retained)
	rootA := deletingRootForTest(t, "root-a", plan)
	rootB := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	rootB.Name = "root-b"
	rootB.UID = "root-b-uid"
	rootB.Finalizers = []string{applicationv1.ApplicationFinalizer}
	child := existingDependencyForTest(rootA, plan)
	deleted, err := desiredManagedResource(child, plan.ManagedResources[0])
	if err != nil {
		t.Fatal(err)
	}
	kept, err := desiredManagedResource(child, retained)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := testReconciler(t, rootA, rootB, child, deleted, kept)
	reconcileOnce(t, ctx, r, rootA)
	if !getDependencyApplication(t, ctx, r.Client, plan.Name).DeletionTimestamp.IsZero() {
		t.Fatal("shared child deleted with a live consumer")
	}
	assertExists(t, ctx, r.Client, &corev1.ConfigMap{}, "monitoring", "settings")
	if err := r.Delete(ctx, getApplication(t, ctx, r.Client, rootB)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, r, rootB)
	assertDependencyTerminating(t, ctx, r.Client, plan.Name)
	reconcileOnce(t, ctx, r, child)
	assertNotFound(t, ctx, r.Client, deleted)
	assertExists(t, ctx, r.Client, &corev1.ConfigMap{}, "monitoring", "retained")
	reconcileOnce(t, ctx, r, child)
	assertApplicationNotFound(t, ctx, r.Client, plan.Name)
	assertExists(t, ctx, r.Client, &corev1.ConfigMap{}, "monitoring", "retained")
}
