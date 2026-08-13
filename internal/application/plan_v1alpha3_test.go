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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEarlierPlansRejectManagedResourcesWithoutChangingCanonicalContracts(t *testing.T) {
	for _, version := range []string{applicationv1.PlanVersionV1Alpha1, applicationv1.PlanVersionV1Alpha2} {
		t.Run(version, func(t *testing.T) {
			var app *applicationv1.OneKSApplication
			if version == applicationv1.PlanVersionV1Alpha1 {
				app = goldenApplication(t)
			} else {
				app = validPlanV1Alpha2Dependency(t)
			}
			app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{}
			err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
			if err == nil || !strings.Contains(err.Reason, "InvalidPlanV1Alpha") {
				t.Fatalf("managedResources error = %#v", err)
			}
		})
	}
}

func TestPlanV1Alpha3ValidManagedResourceDAG(t *testing.T) {
	app := validPlanV1Alpha3(t)
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
		t.Fatalf("valid plan-v1alpha3 rejected: %v", err)
	}
	canonical, err := CanonicalPlan(app.Spec)
	if err != nil || !strings.Contains(string(canonical), `"managedResources"`) {
		t.Fatalf("v3 canonicalization = %s, %v", canonical, err)
	}
}

func TestPlanV1Alpha3IsRootOnlyAndForbidsLegacyResources(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		mutate func(*applicationv1.OneKSApplication)
	}{
		{"dependency role", "InvalidApplicationRole", func(app *applicationv1.OneKSApplication) { app.Spec.Role = applicationv1.ApplicationRoleDependency }},
		{"empty role", "InvalidApplicationRole", func(app *applicationv1.OneKSApplication) { app.Spec.Role = "" }},
		{"legacy resources", "InvalidPlanV1Alpha3Resources", func(app *applicationv1.OneKSApplication) { app.Spec.Resources = goldenApplication(t).Spec.Resources }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := validPlanV1Alpha3(t)
			test.mutate(app)
			refreshV3(t, app)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != test.reason {
				t.Fatalf("error = %#v, want %s", err, test.reason)
			}
		})
	}
}

func TestGeneratedCRDEnforcesPlanV1Alpha3SliceBoundaries(t *testing.T) {
	payload, err := os.ReadFile("../../config/crd/bases/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, required := range []string{
		"plan-v1alpha3 supports only Root applications",
		"self.role == 'Root'",
		"plan-v1alpha3 does not permit legacy resources",
		"size(self.resources)",
		"readinessStartedAt:",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("generated CRD lacks v1alpha3 boundary %q", required)
		}
	}
}

func TestPlanV1Alpha3RootMaterializesUnchangedV1Alpha2DependencyChild(t *testing.T) {
	plan := dependencyPlanForTest("dependency", "dependency-chart", nil)
	root := validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Spec.PlanVersion = applicationv1.PlanVersionV1Alpha3
	root.Spec.Resources = nil
	root.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{managedConfigMap("settings", "runai", "settings", nil)}
	refreshV3(t, root)

	reconciler, _ := testReconciler(t, root)
	raced, terminating, conflict, err := reconciler.materializeRootDependencies(context.Background(), root)
	if err != nil || raced || terminating != "" || conflict != nil {
		t.Fatalf("materialize v1alpha3 Root dependency = raced %v, terminating %q, conflict %#v, err %v", raced, terminating, conflict, err)
	}
	child := &applicationv1.OneKSApplication{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: applicationv1.ApplicationNamespace, Name: plan.Name}, child); err != nil {
		t.Fatalf("get materialized child: %v", err)
	}
	if child.Spec.PlanVersion != applicationv1.PlanVersionV1Alpha2 || child.Spec.Role != applicationv1.ApplicationRoleDependency {
		t.Fatalf("materialized child contract changed: %#v", child.Spec)
	}
	canonical, err := canonicalPlanV1Alpha2(child.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := Digest(canonical); got != plan.PlanDigest {
		t.Fatalf("v1alpha3 Root child digest = %s, want existing v1alpha2 digest %s", got, plan.PlanDigest)
	}
}

func TestPlanV1Alpha3ManagedFieldsAffectDigest(t *testing.T) {
	base := validPlanV1Alpha3(t).Spec
	base.ManagedResources[0].DependsOn = []string{"anchor"}
	base.ManagedResources[0].Readiness.Conditions = []applicationv1.ManagedResourceCondition{{Type: "Ready", Status: "True"}}
	base.ManagedResources[0].Readiness.RequiredResources = []applicationv1.ManagedResourceReference{{
		APIVersion: "v1", Kind: "Secret", APIResource: "secrets", Namespace: "runai", Name: "license",
	}}
	base.ManagedResources[0].Readiness.Checks = []applicationv1.ManagedResourceCheck{{
		Type: applicationv1.ManagedResourceCheckDNSMatchesService, Hostname: "backend.runai.svc",
		Service: applicationv1.ManagedResourceServiceReference{Namespace: "runai", Name: "backend"},
	}}
	base.ManagedResources = append(base.ManagedResources, managedConfigMap("anchor", "runai", "anchor", nil))
	baseDigest := digestSpec(t, base)
	mutations := map[string]func(*applicationv1.ManagedResourceSpec){
		"id":                   func(r *applicationv1.ManagedResourceSpec) { r.ID += "-x" },
		"scope":                func(r *applicationv1.ManagedResourceSpec) { r.Scope = applicationv1.ManagedResourceScopeCluster },
		"apiVersion":           func(r *applicationv1.ManagedResourceSpec) { r.APIVersion = "v2" },
		"kind":                 func(r *applicationv1.ManagedResourceSpec) { r.Kind = "Other" },
		"apiResource":          func(r *applicationv1.ManagedResourceSpec) { r.APIResource = "widgets" },
		"namespace":            func(r *applicationv1.ManagedResourceSpec) { r.Namespace = "other" },
		"name":                 func(r *applicationv1.ManagedResourceSpec) { r.Name += "-x" },
		"manifestJSON":         func(r *applicationv1.ManagedResourceSpec) { r.ManifestJSON += " " },
		"dependsOn":            func(r *applicationv1.ManagedResourceSpec) { r.DependsOn[0] = "other" },
		"condition type":       func(r *applicationv1.ManagedResourceSpec) { r.Readiness.Conditions[0].Type = "Available" },
		"condition status":     func(r *applicationv1.ManagedResourceSpec) { r.Readiness.Conditions[0].Status = "False" },
		"required apiVersion":  func(r *applicationv1.ManagedResourceSpec) { r.Readiness.RequiredResources[0].APIVersion = "v2" },
		"required kind":        func(r *applicationv1.ManagedResourceSpec) { r.Readiness.RequiredResources[0].Kind = "ConfigMap" },
		"required apiResource": func(r *applicationv1.ManagedResourceSpec) { r.Readiness.RequiredResources[0].APIResource = "other" },
		"required namespace":   func(r *applicationv1.ManagedResourceSpec) { r.Readiness.RequiredResources[0].Namespace = "other" },
		"required name":        func(r *applicationv1.ManagedResourceSpec) { r.Readiness.RequiredResources[0].Name = "other" },
		"check type": func(r *applicationv1.ManagedResourceSpec) {
			r.Readiness.Checks[0].Type = applicationv1.ManagedResourceCheckType("Other")
		},
		"check hostname":    func(r *applicationv1.ManagedResourceSpec) { r.Readiness.Checks[0].Hostname = "other.example" },
		"service namespace": func(r *applicationv1.ManagedResourceSpec) { r.Readiness.Checks[0].Service.Namespace = "other" },
		"service name":      func(r *applicationv1.ManagedResourceSpec) { r.Readiness.Checks[0].Service.Name = "other" },
		"timeout":           func(r *applicationv1.ManagedResourceSpec) { r.Readiness.TimeoutSeconds++ },
		"deletionPolicy":    func(r *applicationv1.ManagedResourceSpec) { r.DeletionPolicy = applicationv1.DeletionPolicyRetain },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copy := cloneSpecV3(t, base)
			mutate(&copy.ManagedResources[0])
			if got := digestSpec(t, copy); got == baseDigest {
				t.Fatalf("changing %s did not change digest", name)
			}
		})
	}
}

func TestPlanV1Alpha3RejectsInvalidManagedContracts(t *testing.T) {
	tests := map[string]struct {
		reason string
		mutate func(*applicationv1.OneKSApplication)
	}{
		"malformed manifest": {"InvalidManagedManifest", func(a *applicationv1.OneKSApplication) { a.Spec.ManagedResources[0].ManifestJSON = `{` }},
		"embedded identity":  {"ManagedResourceIdentityMismatch", func(a *applicationv1.OneKSApplication) { a.Spec.ManagedResources[0].Name = "different" }},
		"Secret":             {"UnsupportedManagedSecret", func(a *applicationv1.OneKSApplication) { a.Spec.ManagedResources[0].Kind = "Secret" }},
		"duplicate id": {"DuplicateManagedResourceID", func(a *applicationv1.OneKSApplication) {
			a.Spec.ManagedResources = append(a.Spec.ManagedResources, a.Spec.ManagedResources[0])
			a.Spec.ManagedResources[1].Name = "second"
			a.Spec.ManagedResources[1].ManifestJSON = managedConfigMapManifest("runai", "second")
		}},
		"duplicate identity": {"DuplicateManagedResourceIdentity", func(a *applicationv1.OneKSApplication) {
			second := a.Spec.ManagedResources[0]
			second.ID = "second"
			a.Spec.ManagedResources = append(a.Spec.ManagedResources, second)
		}},
		"unknown dependency": {"UnknownManagedDependency", func(a *applicationv1.OneKSApplication) { a.Spec.ManagedResources[0].DependsOn = []string{"missing"} }},
		"self dependency": {"ManagedResourceSelfDependency", func(a *applicationv1.OneKSApplication) {
			a.Spec.ManagedResources[0].DependsOn = []string{a.Spec.ManagedResources[0].ID}
		}},
		"cycle": {"ManagedResourceCycle", func(a *applicationv1.OneKSApplication) {
			a.Spec.ManagedResources = append(a.Spec.ManagedResources, managedConfigMap("second", "runai", "second", []string{"settings"}))
			a.Spec.ManagedResources[0].DependsOn = []string{"second"}
		}},
		"reserved label": {"ReservedOwnershipLabel", func(a *applicationv1.OneKSApplication) {
			a.Spec.ManagedResources[0].ManifestJSON = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"namespace":"runai","name":"settings","labels":{"applications.oneks.opennebula.io/managed-by":"x"}}}`
		}},
		"top-level status": {"UnsupportedManagedManifestField", func(a *applicationv1.OneKSApplication) {
			a.Spec.ManagedResources[0].ManifestJSON = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"namespace":"runai","name":"settings"},"status":{}}`
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			app := validPlanV1Alpha3(t)
			test.mutate(app)
			refreshV3(t, app)
			err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
			if err == nil || err.Reason != test.reason {
				t.Fatalf("error = %#v, want %s", err, test.reason)
			}
		})
	}
}

func TestPlanV1Alpha3RejectsControllerOwnedManifestMetadata(t *testing.T) {
	for _, field := range []string{
		"ownerReferences", "finalizers", "managedFields", "resourceVersion", "uid", "generation",
		"creationTimestamp", "deletionTimestamp",
	} {
		t.Run(field, func(t *testing.T) {
			app := validPlanV1Alpha3(t)
			app.Spec.ManagedResources[0].ManifestJSON = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"namespace":"runai","name":"settings","` + field + `":[]}}`
			refreshV3(t, app)
			err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
			if err == nil || err.Reason != "UnsupportedManagedManifestField" {
				t.Fatalf("metadata.%s error = %#v", field, err)
			}
		})
	}
}

func TestPlanV1Alpha3SupportsNamespacedAndClusterScopedResources(t *testing.T) {
	app := validPlanV1Alpha3(t)
	app.Spec.ManagedResources = append(app.Spec.ManagedResources, applicationv1.ManagedResourceSpec{
		ID: "namespace", Scope: applicationv1.ManagedResourceScopeCluster, APIVersion: "v1", Kind: "Namespace", Name: "runai-extra",
		ManifestJSON: `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"runai-extra"}}`,
		Readiness:    applicationv1.ManagedResourceReadiness{TimeoutSeconds: 60}, DeletionPolicy: applicationv1.DeletionPolicyDelete,
	})
	refreshV3(t, app)
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
		t.Fatalf("mixed scopes rejected: %v", err)
	}
}

func TestPlanV1Alpha3CreatesInTopologicalOrderAndGatesHelm(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha3(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{
		managedConfigMap("second", "runai", "second", []string{"first"}),
		managedConfigMap("first", "runai", "first", nil),
	}
	refreshV3(t, app)
	reconciler, recorder := testReconciler(t, app)
	reconcileOnce(t, ctx, reconciler, app)
	if got := strings.Join(recorder.childWrites, ","); got != "create:ConfigMap,create:ConfigMap,create:HelmChart" {
		t.Fatalf("topological writes = %s", got)
	}

	blocked := validPlanV1Alpha3(t)
	blocked.Finalizers = []string{applicationv1.ApplicationFinalizer}
	blocked.Spec.ManagedResources[0].Readiness.Conditions = []applicationv1.ManagedResourceCondition{{Type: "Ready", Status: "True"}}
	refreshV3(t, blocked)
	object, _ := desiredManagedResource(blocked, blocked.Spec.ManagedResources[0])
	object.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	reconciler, recorder = testReconciler(t, blocked, object)
	reconcileOnce(t, ctx, reconciler, blocked)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("readiness gate allowed effects: %#v", recorder.childWrites)
	}
}

func TestPlanV1Alpha3OwnershipPreflightAndObserveAreWriteSafe(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha3(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources = append(app.Spec.ManagedResources, managedConfigMap("foreign", "runai", "foreign", nil))
	refreshV3(t, app)
	foreign := emptyManagedResource(app.Spec.ManagedResources[1])
	foreign.Object["data"] = map[string]any{"x": "y"}
	reconciler, recorder := testReconciler(t, app, foreign)
	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("preflight conflict wrote an earlier object: %#v", recorder.childWrites)
	}

	observe := validPlanV1Alpha3(t)
	observe.Spec.ExecutionMode = applicationv1.ExecutionModeObserve
	refreshV3(t, observe)
	reconciler, recorder = testReconciler(t, observe)
	reconcileOnce(t, ctx, reconciler, observe)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("Observe mutated generic children: %#v", recorder.childWrites)
	}
}

func TestPlanV1Alpha3RepairsOwnedDriftWithNonForcedSSA(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha3(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources[0].ManifestJSON = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"namespace":"runai","name":"settings","labels":{"catalogue":"desired"},"annotations":{"catalogue.example/value":"desired"}},"data":{"value":"desired"}}`
	refreshV3(t, app)
	object, _ := desiredManagedResource(app, app.Spec.ManagedResources[0])
	object.Object["data"] = map[string]any{"drifted": "true"}
	labels := object.GetLabels()
	labels["catalogue"] = "drifted"
	object.SetLabels(labels)
	object.SetAnnotations(map[string]string{"catalogue.example/value": "drifted"})
	reconciler, recorder := testReconciler(t, app, object)
	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.patchForces) != 1 || recorder.patchForces[0] {
		t.Fatalf("managed SSA force options = %#v", recorder.patchForces)
	}
	if len(recorder.patchResourceVersions) != 1 || recorder.patchResourceVersions[0] == "" {
		t.Fatalf("managed SSA omitted resourceVersion: %#v", recorder.patchResourceVersions)
	}
	stored := emptyManagedResource(app.Spec.ManagedResources[0])
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}
	if stored.GetLabels()["catalogue"] != "desired" || stored.GetAnnotations()["catalogue.example/value"] != "desired" {
		t.Fatalf("catalogue metadata drift was not repaired: labels=%#v annotations=%#v", stored.GetLabels(), stored.GetAnnotations())
	}
}

func TestPlanV1Alpha3RequiredSecretAndDNSGateHelm(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha3(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	resource := &app.Spec.ManagedResources[0]
	resource.Readiness.RequiredResources = []applicationv1.ManagedResourceReference{{APIVersion: "v1", Kind: "Secret", Namespace: "runai", Name: "license"}}
	resource.Readiness.Checks = []applicationv1.ManagedResourceCheck{{Type: "DNSMatchesService", Hostname: "backend.runai.svc", Service: applicationv1.ManagedResourceServiceReference{Namespace: "runai", Name: "backend"}}}
	refreshV3(t, app)
	object, _ := desiredManagedResource(app, *resource)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "runai", Name: "backend"}, Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.8", ClusterIPs: []string{"10.0.0.8"}}}
	reconciler, recorder := testReconciler(t, app, object, service)
	reconciler.APIReader = &metadataSecretReader{Reader: reconciler.Client, secretExists: false}
	reconciler.DNSLookup = func(context.Context, string) ([]string, error) { return []string{"10.0.0.8"}, nil }
	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("missing required Secret did not gate Helm: %#v", recorder.childWrites)
	}

	reconciler.APIReader = &metadataSecretReader{Reader: reconciler.Client, secretExists: true}
	reconciler.DNSLookup = func(context.Context, string) ([]string, error) { return []string{"10.0.0.9"}, nil }
	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("DNS mismatch did not gate Helm: %#v", recorder.childWrites)
	}
	reconciler.DNSLookup = func(context.Context, string) ([]string, error) { return []string{"10.0.0.8"}, nil }
	reconcileOnce(t, ctx, reconciler, app)
	if got := strings.Join(recorder.childWrites, ","); got != "create:HelmChart" {
		t.Fatalf("satisfied readiness writes = %s", got)
	}
}

func TestDNSMatchesServiceClassifiesNotFoundAndAPIErrors(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha3(t)
	check := applicationv1.ManagedResourceCheck{
		Type: applicationv1.ManagedResourceCheckDNSMatchesService, Hostname: "backend.runai.svc",
		Service: applicationv1.ManagedResourceServiceReference{Namespace: "runai", Name: "backend"},
	}
	object, _ := desiredManagedResource(app, app.Spec.ManagedResources[0])
	reconciler, _ := testReconciler(t, app, object)
	ready, err := reconciler.dnsMatchesService(ctx, check)
	if err != nil || ready {
		t.Fatalf("Service NotFound = ready %v, err %v; want pending", ready, err)
	}

	apiErr := apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, check.Service.Name, errors.New("denied"))
	reconciler.APIReader = &serviceErrorReader{Reader: reconciler.Client, err: apiErr}
	ready, err = reconciler.dnsMatchesService(ctx, check)
	if ready || !errors.Is(err, apiErr) {
		t.Fatalf("Service API failure = ready %v, err %v; want propagated Forbidden", ready, err)
	}

	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "runai", Name: "backend"}, Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.8"}}
	reconciler, _ = testReconciler(t, app, object, service)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	reconciler.DNSLookup = func(ctx context.Context, _ string) ([]string, error) { return nil, ctx.Err() }
	if _, err := reconciler.dnsMatchesService(cancelled, check); !errors.Is(err, context.Canceled) {
		t.Fatalf("DNS context cancellation was not propagated: %v", err)
	}
}

func TestManagedCreateAlreadyExistsRaceRechecksOwnership(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha3(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, recorder := testReconciler(t, app)
	target := app.Spec.ManagedResources[0]
	foreign := emptyManagedResource(target)
	foreign.Object["data"] = map[string]any{"foreign": "true"}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	authoritative := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: WorkloadNamespace}}, foreign,
	).Build()
	reconciler.APIReader = &managedRaceReader{Reader: authoritative, target: client.ObjectKeyFromObject(foreign)}
	reconciler.Client = &managedCreateRaceClient{Client: reconciler.Client, target: client.ObjectKeyFromObject(foreign)}

	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "OwnershipConflict" {
		t.Fatalf("AlreadyExists foreign object was not terminal OwnershipConflict: %#v", stored.Status)
	}
	conflictCondition := conditionByType(stored.Status.Conditions, ConditionOwnershipConflict)
	if conflictCondition == nil || conflictCondition.Status != metav1.ConditionTrue {
		t.Fatalf("AlreadyExists race reported inconsistent ownership condition: %#v", stored.Status.Conditions)
	}
	if len(recorder.patchForces) != 0 {
		t.Fatalf("foreign raced object was patched: %#v", recorder.patchForces)
	}
	current := emptyManagedResource(target)
	if err := authoritative.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if len(current.GetLabels()) != 0 {
		t.Fatalf("foreign raced object was mutated: %#v", current.Object)
	}
}

func TestPlanV1Alpha3ReadinessTimeoutSurvivesStoredStatus(t *testing.T) {
	ctx := context.Background()
	fixedNow := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	app := validPlanV1Alpha3(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources[0].Readiness.Conditions = []applicationv1.ManagedResourceCondition{{Type: "Ready", Status: "True"}}
	refreshV3(t, app)
	started := metav1.NewTime(fixedNow.Add(-2 * time.Minute))
	app.Status.Resources = []applicationv1.ResourceStatus{{
		ID: app.Spec.ManagedResources[0].ID, Phase: "Applying", Reason: "ConditionPending", ReadinessStartedAt: &started,
	}}
	object, _ := desiredManagedResource(app, app.Spec.ManagedResources[0])
	reconciler, _ := testReconciler(t, app, object)
	reconciler.Now = func() time.Time { return fixedNow }
	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.LastError == nil || stored.Status.LastError.Reason != "ReadinessTimeout" {
		t.Fatalf("stored timeout origin did not fail readiness: %#v", stored.Status)
	}
	firstOrigin := stored.Status.Resources[0].ReadinessStartedAt
	reconcileOnce(t, ctx, reconciler, app)
	stored = getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.Resources[0].Reason != "ReadinessTimeout" || stored.Status.Resources[0].ReadinessStartedAt == nil || !stored.Status.Resources[0].ReadinessStartedAt.Equal(firstOrigin) {
		t.Fatalf("timeout was not sticky on the next reconcile: %#v", stored.Status)
	}
}

func TestManagedReadinessTimeoutRemainsStickyWhenObjectDisappears(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha3(t)
	origin := metav1.NewTime(time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC))
	app.Status.Resources = []applicationv1.ResourceStatus{{
		ID: app.Spec.ManagedResources[0].ID, Phase: "Failed", Reason: "ReadinessTimeout",
		Message: "original timeout", ReadinessStartedAt: &origin,
	}}
	reconciler, recorder := testReconciler(t, app)
	observed, err := reconciler.observeManagedResources(ctx, app, true)
	if err != nil {
		t.Fatalf("observe absent timed-out resource: %v", err)
	}
	if len(observed.resources) != 1 || observed.resources[0].Phase != "Failed" || observed.resources[0].Reason != "ReadinessTimeout" || observed.resources[0].Message != "original timeout" || observed.resources[0].ReadinessStartedAt == nil || !observed.resources[0].ReadinessStartedAt.Equal(&origin) || !observed.resourcesFailed {
		t.Fatalf("absent resource lost sticky timeout: observation=%#v status=%#v", observed, observed.resources)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("observation mutated children: %#v", recorder.childWrites)
	}
}

func TestManagedReadinessOriginRoundTripAndReasonChanges(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	app := validPlanV1Alpha3(t)
	resource := &app.Spec.ManagedResources[0]
	resource.Readiness.RequiredResources = []applicationv1.ManagedResourceReference{{APIVersion: "v1", Kind: "Secret", Namespace: "runai", Name: "license"}}
	resource.Readiness.Checks = []applicationv1.ManagedResourceCheck{{
		Type: applicationv1.ManagedResourceCheckDNSMatchesService, Hostname: "backend.runai.svc",
		Service: applicationv1.ManagedResourceServiceReference{Namespace: "runai", Name: "backend"},
	}}
	refreshV3(t, app)
	object, _ := desiredManagedResource(app, *resource)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "runai", Name: "backend"}, Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.8", ClusterIPs: []string{"10.0.0.8"}}}
	reconciler, _ := testReconciler(t, app, object, service)
	reconciler.APIReader = &metadataSecretReader{Reader: reconciler.Client, secretExists: false}
	reconciler.Now = func() time.Time { return start }
	reconciler.DNSLookup = func(context.Context, string) ([]string, error) { return []string{"10.0.0.9"}, nil }
	first, err := reconciler.observeManagedResources(ctx, app, true)
	if err != nil || first.resources[0].ReadinessStartedAt == nil || first.resources[0].Reason != "RequiredResourceMissing" {
		t.Fatalf("first pending observation = %#v, %v", first.resources, err)
	}
	payload, err := json.Marshal(first.resources)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip []applicationv1.ResourceStatus
	if err := json.Unmarshal(payload, &roundTrip); err != nil {
		t.Fatal(err)
	}
	app.Status.Resources = roundTrip
	restarted, _ := testReconciler(t, app, object, service)
	restarted.APIReader = &metadataSecretReader{Reader: restarted.Client, secretExists: true}
	restarted.DNSLookup = func(context.Context, string) ([]string, error) { return []string{"10.0.0.9"}, nil }
	restarted.Now = func() time.Time { return start.Add(30 * time.Second) }
	second, err := restarted.observeManagedResources(ctx, app, true)
	if err != nil || second.resources[0].Reason != "DNSCheckPending" || !second.resources[0].ReadinessStartedAt.Equal(first.resources[0].ReadinessStartedAt) {
		t.Fatalf("reason change reset readiness origin: first=%#v second=%#v err=%v", first.resources[0], second.resources[0], err)
	}
}

func TestManagedReadinessTimeoutsAreIndependent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 13, 10, 0, 20, 0, time.UTC)
	app := validPlanV1Alpha3(t)
	app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{
		managedConfigMap("short", "runai", "short", nil), managedConfigMap("long", "runai", "long", nil),
	}
	app.Spec.ManagedResources[0].Readiness.TimeoutSeconds = 10
	app.Spec.ManagedResources[1].Readiness.TimeoutSeconds = 100
	for index := range app.Spec.ManagedResources {
		app.Spec.ManagedResources[index].Readiness.Conditions = []applicationv1.ManagedResourceCondition{{Type: "Ready", Status: "True"}}
	}
	refreshV3(t, app)
	started := metav1.NewTime(now.Add(-20 * time.Second))
	app.Status.Resources = []applicationv1.ResourceStatus{
		{ID: "short", Phase: "Applying", ReadinessStartedAt: &started},
		{ID: "long", Phase: "Applying", ReadinessStartedAt: &started},
	}
	short, _ := desiredManagedResource(app, app.Spec.ManagedResources[0])
	long, _ := desiredManagedResource(app, app.Spec.ManagedResources[1])
	reconciler, _ := testReconciler(t, app, short, long)
	reconciler.Now = func() time.Time { return now }
	observed, err := reconciler.observeManagedResources(ctx, app, true)
	if err != nil || observed.resources[0].Reason != "ReadinessTimeout" || observed.resources[1].Phase != "Applying" {
		t.Fatalf("independent timeouts = %#v, %v", observed.resources, err)
	}
}

func TestManagedReadinessReadyRegressionStartsNewIntervalAndDependencyWaitDoesNotCount(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	app := validPlanV1Alpha3(t)
	app.Spec.ManagedResources[0].Readiness.Conditions = []applicationv1.ManagedResourceCondition{{Type: "Ready", Status: "True"}}
	refreshV3(t, app)
	app.Status.Resources = []applicationv1.ResourceStatus{{ID: "settings", Phase: "Ready", Reason: "ReadinessSatisfied"}}
	object, _ := desiredManagedResource(app, app.Spec.ManagedResources[0])
	object.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	reconciler, _ := testReconciler(t, app, object)
	reconciler.Now = func() time.Time { return now }
	regressed, err := reconciler.observeManagedResources(ctx, app, true)
	if err != nil || regressed.resources[0].ReadinessStartedAt == nil || !regressed.resources[0].ReadinessStartedAt.Time.Equal(now) {
		t.Fatalf("Ready regression did not start a new interval: %#v, %v", regressed.resources, err)
	}

	app.Status.Resources = nil
	reconciler.Now = func() time.Time { return now.Add(time.Hour) }
	gated, err := reconciler.observeManagedResources(ctx, app, false)
	if err != nil || gated.resources[0].ReadinessStartedAt != nil {
		t.Fatalf("dependency-pending time started readiness: %#v, %v", gated.resources, err)
	}
	app.Status.Resources = gated.resources
	reconciler.Now = func() time.Time { return now.Add(2 * time.Hour) }
	ungated, err := reconciler.observeManagedResources(ctx, app, true)
	if err != nil || ungated.resources[0].ReadinessStartedAt == nil || !ungated.resources[0].ReadinessStartedAt.Time.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("readiness did not start after dependency gate: %#v, %v", ungated.resources, err)
	}
}

func TestPlanV1Alpha3DeletionIsReverseTopologicalAndRetainSafe(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha3(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{
		managedConfigMap("second", "runai", "second", []string{"first"}),
		managedConfigMap("first", "runai", "first", nil),
		managedConfigMap("retained", "runai", "retained", nil),
	}
	app.Spec.ManagedResources[2].DeletionPolicy = applicationv1.DeletionPolicyRetain
	refreshV3(t, app)
	now := metav1.Now()
	app.DeletionTimestamp = &now
	first, _ := desiredManagedResource(app, app.Spec.ManagedResources[1])
	second, _ := desiredManagedResource(app, app.Spec.ManagedResources[0])
	retained, _ := desiredManagedResource(app, app.Spec.ManagedResources[2])
	reconciler, _ := testReconciler(t, app, first, second, retained)
	reconcileOnce(t, ctx, reconciler, app)
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(second), emptyManagedResource(app.Spec.ManagedResources[0])); !apierrors.IsNotFound(err) {
		t.Fatalf("dependent resource was not deleted first: %v", err)
	}
	assertExists(t, ctx, reconciler.Client, emptyManagedResource(app.Spec.ManagedResources[1]), "runai", "first")
	reconcileOnce(t, ctx, reconciler, app)
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(first), emptyManagedResource(app.Spec.ManagedResources[1])); !apierrors.IsNotFound(err) {
		t.Fatalf("dependency resource was not deleted second: %v", err)
	}
	assertExists(t, ctx, reconciler.Client, emptyManagedResource(app.Spec.ManagedResources[2]), "runai", "retained")
}

func TestPlanV1Alpha3DeletionConflictDeletesNothing(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha3(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources = append(app.Spec.ManagedResources, managedConfigMap("foreign", "runai", "foreign", nil))
	refreshV3(t, app)
	now := metav1.Now()
	app.DeletionTimestamp = &now
	owned, _ := desiredManagedResource(app, app.Spec.ManagedResources[0])
	foreign := emptyManagedResource(app.Spec.ManagedResources[1])
	reconciler, recorder := testReconciler(t, app, owned, foreign)
	reconcileOnce(t, ctx, reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("deletion preflight conflict allowed mutation: %#v", recorder.childWrites)
	}
	assertExists(t, ctx, reconciler.Client, emptyManagedResource(app.Spec.ManagedResources[0]), "runai", "settings")
}

func TestPlanV1Alpha3DependenciesGateManagedEffects(t *testing.T) {
	plan := dependencyPlanForTest("dependency", "dependency-chart", nil)
	app := validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	app.Spec.PlanVersion = applicationv1.PlanVersionV1Alpha3
	app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{managedConfigMap("settings", "runai", "settings", nil)}
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	refreshV3(t, app)
	reconciler, recorder := testReconciler(t, app)
	reconcileOnce(t, context.Background(), reconciler, app)
	if len(recorder.childWrites) != 0 {
		t.Fatalf("unready dependency allowed managed effects: %#v", recorder.childWrites)
	}
}

func validPlanV1Alpha3(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := goldenApplication(t)
	app.Spec.PlanVersion = applicationv1.PlanVersionV1Alpha3
	app.Spec.Role = applicationv1.ApplicationRoleRoot
	app.Spec.Resources = nil
	app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{managedConfigMap("settings", "runai", "settings", nil)}
	refreshV3(t, app)
	return app
}

func managedConfigMap(id, namespace, name string, dependencies []string) applicationv1.ManagedResourceSpec {
	return applicationv1.ManagedResourceSpec{
		ID: id, Scope: applicationv1.ManagedResourceScopeNamespaced, APIVersion: "v1", Kind: "ConfigMap", APIResource: "configmaps",
		Namespace: namespace, Name: name, ManifestJSON: managedConfigMapManifest(namespace, name), DependsOn: dependencies,
		Readiness: applicationv1.ManagedResourceReadiness{TimeoutSeconds: 60}, DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
}

func managedConfigMapManifest(namespace, name string) string {
	return `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"namespace":"` + namespace + `","name":"` + name + `"},"data":{"token-shaped-name-is-allowed":"trusted"}}`
}

func refreshV3(t *testing.T, app *applicationv1.OneKSApplication) {
	t.Helper()
	refreshDigest(t, app)
	app.Labels = producerLabels(app)
}

func digestSpec(t *testing.T, spec applicationv1.OneKSApplicationSpec) string {
	t.Helper()
	canonical, err := CanonicalPlan(spec)
	if err != nil {
		t.Fatal(err)
	}
	return Digest(canonical)
}

func cloneSpecV3(t *testing.T, spec applicationv1.OneKSApplicationSpec) applicationv1.OneKSApplicationSpec {
	t.Helper()
	payload, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var copy applicationv1.OneKSApplicationSpec
	if err := json.Unmarshal(payload, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

type metadataSecretReader struct {
	client.Reader
	secretExists bool
}

func (r *metadataSecretReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if metadata, ok := object.(*metav1.PartialObjectMetadata); ok && metadata.GroupVersionKind().Kind == "Secret" {
		if !r.secretExists {
			return apierrors.NewNotFound(schemaGroupResource("secrets"), key.Name)
		}
		metadata.SetNamespace(key.Namespace)
		metadata.SetName(key.Name)
		metadata.SetUID(types.UID("secret-uid"))
		return nil
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func schemaGroupResource(resource string) schema.GroupResource {
	return schema.GroupResource{Resource: resource}
}

func conditionByType(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}

type serviceErrorReader struct {
	client.Reader
	err error
}

func (r *serviceErrorReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Service); ok {
		return r.err
	}
	return r.Reader.Get(ctx, key, object, options...)
}

type managedRaceReader struct {
	client.Reader
	target client.ObjectKey
	gets   int
}

func (r *managedRaceReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if key == r.target {
		r.gets++
		if r.gets <= 2 {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

type managedCreateRaceClient struct {
	client.Client
	target client.ObjectKey
}

func (c *managedCreateRaceClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if client.ObjectKeyFromObject(object) == c.target {
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, object.GetName())
	}
	return c.Client.Create(ctx, object, options...)
}
