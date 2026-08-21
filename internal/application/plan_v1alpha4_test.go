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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	planV1Alpha4Digest = "sha256-XmdLlLaKq1PnxhIvgPUGpnvHtB7WsplCx9CXwn9EPL8"
	v2RegressionDigest = "sha256-8JPjVX4Cad7Mia5UH8uSEGO1-6OYtq8R9I2BWgiO4mA"
	v3RegressionDigest = "sha256-bq07BdFtI7pAux7dNoIBRUFk3ztXHWCD4nhoAW_FCB4"
)

func TestPlanV1Alpha4CanonicalFixtureAndEarlierDigests(t *testing.T) {
	app := validPlanV1Alpha4(t)
	canonical, err := CanonicalPlan(app.Spec)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/oneks_application_plan_v1alpha4.canonical.json")
	if err != nil {
		t.Fatal(err)
	}
	want = bytes.TrimSuffix(want, []byte("\n"))
	if !bytes.Equal(canonical, want) {
		t.Fatalf("v4 canonical mismatch:\n got: %s\nwant: %s", canonical, want)
	}
	if got := Digest(canonical); got != planV1Alpha4Digest {
		t.Fatalf("v4 digest = %s, want %s", got, planV1Alpha4Digest)
	}

	v2 := validPlanV1Alpha2Root(t)
	if got := digestSpec(t, v2.Spec); got != v2RegressionDigest {
		t.Fatalf("v2 regression digest = %s, want %s", got, v2RegressionDigest)
	}
	v3 := validPlanV1Alpha3(t)
	if got := digestSpec(t, v3.Spec); got != v3RegressionDigest {
		t.Fatalf("v3 regression digest = %s, want %s", got, v3RegressionDigest)
	}
	// The existing v1 fixture has its byte-for-byte and fixed-digest assertion
	// in TestPlanV1Alpha1CanonicalFixtureRemainsUnchanged.
}

func TestGeneratedCRDEnforcesPlanV1Alpha4Boundaries(t *testing.T) {
	payload, err := os.ReadFile("../../config/crd/bases/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, required := range []string{
		"oneks.opennebula.io/plan-v1alpha4",
		"plan-v1alpha4 supports only Root applications",
		"plan-v1alpha4 requires secretInputRef and protectedSecrets",
		"plan-v1alpha4 permits at most 16 combined managedResources",
		"basicAuthSecret requires username and passwordInputKey",
		"opaqueSecret requires opaqueData only",
		"dockerConfigJsonSecret requires registry, username, passwordInputKey,",
		"protected Secret target identities must be unique",
		"protected Secret IDs must not collide with managed resource",
		"only plan-v1alpha4 permits release.authSecret",
		"plan-v1alpha4 release.authSecret requires an HTTPS repositoryURL",
		"release.authSecret must match exactly one protected basicAuthSecret",
		"enum:\n                    - oneks-system",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("generated CRD lacks v4 boundary %q", required)
		}
	}
}

func TestPlanV1Alpha4RejectsCrossCollectionResourceIDCollisions(t *testing.T) {
	t.Run("cross collection", func(t *testing.T) {
		app := validPlanV1Alpha4(t)
		app.Spec.ProtectedSecrets[0].ID = app.Spec.ManagedResources[0].ID
		refreshV4(t, app)
		err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
		if err == nil || err.Reason != "ProtectedSecretManagedResourceIDCollision" {
			t.Fatalf("cross-collection collision error = %#v", err)
		}
	})

	t.Run("managed collection", func(t *testing.T) {
		app := validPlanV1Alpha4(t)
		app.Spec.ManagedResources = append(app.Spec.ManagedResources, app.Spec.ManagedResources[0])
		refreshV4(t, app)
		err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
		if err == nil || err.Reason != "DuplicateManagedResourceID" {
			t.Fatalf("managed duplicate error = %#v", err)
		}
	})

	t.Run("protected collection", func(t *testing.T) {
		app := validPlanV1Alpha4(t)
		app.Spec.ProtectedSecrets = append(app.Spec.ProtectedSecrets, app.Spec.ProtectedSecrets[0])
		refreshV4(t, app)
		err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
		if err == nil || err.Reason != "DuplicateProtectedSecretID" {
			t.Fatalf("protected duplicate error = %#v", err)
		}
	})
}

func TestManagedTargetNamespaceBootstrapUsesNormalV3V4DAG(t *testing.T) {
	for _, version := range []string{applicationv1.PlanVersionV1Alpha3, applicationv1.PlanVersionV1Alpha4} {
		t.Run(version, func(t *testing.T) {
			var app *applicationv1.OneKSApplication
			if version == applicationv1.PlanVersionV1Alpha3 {
				app = validPlanV1Alpha3(t)
			} else {
				app = validPlanV1Alpha4(t)
			}
			app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{managedTargetNamespaceResource()}
			if version == applicationv1.PlanVersionV1Alpha3 {
				refreshV3(t, app)
			} else {
				refreshV4(t, app)
			}

			ctx := context.Background()
			reconciler, recorder := testReconciler(t, app)
			if err := reconciler.Client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: WorkloadNamespace}}); err != nil {
				t.Fatal(err)
			}
			recorder.childWrites = nil

			reconcileOnce(t, ctx, reconciler, app)
			stored := getApplication(t, ctx, reconciler.Client, app)
			if !containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
				t.Fatalf("managed namespace bootstrap did not progress to finalizer: %#v", stored.Finalizers)
			}
			if stored.Status.LastError != nil && stored.Status.LastError.Reason == "TargetNamespaceMissing" {
				t.Fatalf("managed namespace bootstrap was rejected: %#v", stored.Status.LastError)
			}

			reconcileOnce(t, ctx, reconciler, app)
			assertExists(t, ctx, reconciler.Client, &corev1.Namespace{}, "", WorkloadNamespace)
			if !containsWrite(recorder.childWrites, "create:Namespace") {
				t.Fatalf("normal managed DAG did not create target namespace: %#v", recorder.childWrites)
			}
		})
	}
}

func TestMissingTargetNamespaceWithoutManagedTargetPreservesFailure(t *testing.T) {
	for _, version := range []string{applicationv1.PlanVersionV1Alpha3, applicationv1.PlanVersionV1Alpha4} {
		t.Run(version, func(t *testing.T) {
			var app *applicationv1.OneKSApplication
			if version == applicationv1.PlanVersionV1Alpha3 {
				app = validPlanV1Alpha3(t)
			} else {
				app = validPlanV1Alpha4(t)
			}
			ctx := context.Background()
			reconciler, recorder := testReconciler(t, app)
			if err := reconciler.Client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: WorkloadNamespace}}); err != nil {
				t.Fatal(err)
			}
			recorder.childWrites = nil

			reconcileOnce(t, ctx, reconciler, app)
			stored := getApplication(t, ctx, reconciler.Client, app)
			if stored.Status.LastError == nil || stored.Status.LastError.Reason != "TargetNamespaceMissing" {
				t.Fatalf("missing target namespace status = %#v", stored.Status)
			}
			if containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) || len(recorder.childWrites) != 0 {
				t.Fatalf("missing unmanaged namespace caused progression: finalizers=%#v writes=%#v", stored.Finalizers, recorder.childWrites)
			}
		})
	}
}

func TestProtectedSecretWaitsForManagedTargetNamespace(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha4(t)
	app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{managedTargetNamespaceResource()}
	refreshV4(t, app)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_NAMESPACE_BOOTSTRAP")})
	reconciler, recorder := testReconciler(t, app, input)
	if err := reconciler.Client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: WorkloadNamespace}}); err != nil {
		t.Fatal(err)
	}
	recorder.childWrites = nil

	reconcileOnce(t, ctx, reconciler, app)
	assertNotFound(t, ctx, reconciler.Client, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: WorkloadNamespace}})
	assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: WorkloadNamespace, Name: "protected"}})

	recorder.childWrites = nil
	reconcileOnce(t, ctx, reconciler, app)
	assertExists(t, ctx, reconciler.Client, &corev1.Namespace{}, "", WorkloadNamespace)
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, WorkloadNamespace, "protected")
	writes := strings.Join(recorder.childWrites, ",")
	if !strings.HasPrefix(writes, "create:Namespace,create:Secret") {
		t.Fatalf("protected Secret was not sequenced after managed namespace readiness: %s", writes)
	}
}

func TestInputSecretInvalidKeepsPlanValid(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha4(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources = nil
	refreshV4(t, app)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_INVALID_RUNTIME")})
	input.UID = "replacement-uid"
	reconciler, _ := testReconciler(t, app, input)

	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.LastError == nil || stored.Status.LastError.Reason != "InputSecretInvalid" {
		t.Fatalf("runtime input failure status = %#v", stored.Status)
	}
	for conditionType, want := range map[string]metav1.ConditionStatus{
		ConditionPlanValid:             metav1.ConditionTrue,
		ConditionProtectedSecretsReady: metav1.ConditionFalse,
		ConditionReady:                 metav1.ConditionFalse,
	} {
		condition := conditionByType(stored.Status.Conditions, conditionType)
		if condition == nil || condition.Status != want {
			t.Fatalf("condition %s = %#v, want %s", conditionType, condition, want)
		}
	}
}

func TestPlanV1Alpha4ValidatesStructuredProtectedSecretContract(t *testing.T) {
	app := validPlanV1Alpha4(t)
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
		t.Fatalf("valid v4 rejected: %v", err)
	}

	tests := []struct {
		name   string
		reason string
		mutate func(*applicationv1.OneKSApplication)
	}{
		{"root only", "InvalidApplicationRole", func(app *applicationv1.OneKSApplication) { app.Spec.Role = applicationv1.ApplicationRoleDependency }},
		{"legacy resources", "InvalidPlanV1Alpha4Resources", func(app *applicationv1.OneKSApplication) { app.Spec.Resources = goldenApplication(t).Spec.Resources }},
		{"missing input ref", "MissingSecretInputRef", func(app *applicationv1.OneKSApplication) { app.Spec.SecretInputRef = nil }},
		{"wrong input namespace", "InvalidSecretInputNamespace", func(app *applicationv1.OneKSApplication) { app.Spec.SecretInputRef.Namespace = "other" }},
		{"empty input UID", "InvalidSecretInputUID", func(app *applicationv1.OneKSApplication) { app.Spec.SecretInputRef.UID = "" }},
		{"duplicate id", "DuplicateProtectedSecretID", func(app *applicationv1.OneKSApplication) {
			app.Spec.ProtectedSecrets = append(app.Spec.ProtectedSecrets, app.Spec.ProtectedSecrets[0])
		}},
		{"duplicate identity", "DuplicateProtectedSecretIdentity", func(app *applicationv1.OneKSApplication) {
			copy := app.Spec.ProtectedSecrets[0]
			copy.ID = "other"
			app.Spec.ProtectedSecrets = append(app.Spec.ProtectedSecrets, copy)
		}},
		{"source target collision", "ProtectedSecretInputIdentityCollision", func(app *applicationv1.OneKSApplication) {
			app.Spec.ProtectedSecrets[0].Namespace = app.Spec.SecretInputRef.Namespace
			app.Spec.ProtectedSecrets[0].Name = app.Spec.SecretInputRef.Name
		}},
		{"bad opaque key", "InvalidOpaqueSecretData", func(app *applicationv1.OneKSApplication) { app.Spec.ProtectedSecrets[0].OpaqueData[0].Key = "bad/key" }},
		{"extraneous opaque field", "InvalidOpaqueSecret", func(app *applicationv1.OneKSApplication) { app.Spec.ProtectedSecrets[0].Username = "unexpected" }},
		{"unsupported builder", "UnsupportedProtectedSecretBuilder", func(app *applicationv1.OneKSApplication) { app.Spec.ProtectedSecrets[0].BuilderType = "arbitrary" }},
		{"combined bound", "TooManyCombinedResources", func(app *applicationv1.OneKSApplication) {
			app.Spec.ManagedResources = make([]applicationv1.ManagedResourceSpec, 16)
			for index := range app.Spec.ManagedResources {
				app.Spec.ManagedResources[index] = managedConfigMap(string(rune('a'+index)), "runai", "resource-"+string(rune('a'+index)), nil)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := validPlanV1Alpha4(t)
			test.mutate(candidate)
			refreshV4(t, candidate)
			err := ValidatePlan(candidate, ValidationConfig{ClusterID: candidate.Spec.ClusterID})
			if err == nil || err.Reason != test.reason {
				t.Fatalf("error = %#v, want %s", err, test.reason)
			}
		})
	}
}

func TestEarlierPlansRejectProtectedSecretFields(t *testing.T) {
	for _, version := range []string{applicationv1.PlanVersionV1Alpha1, applicationv1.PlanVersionV1Alpha2, applicationv1.PlanVersionV1Alpha3} {
		t.Run(version, func(t *testing.T) {
			var app *applicationv1.OneKSApplication
			switch version {
			case applicationv1.PlanVersionV1Alpha1:
				app = goldenApplication(t)
			case applicationv1.PlanVersionV1Alpha2:
				app = validPlanV1Alpha2Dependency(t)
			default:
				app = validPlanV1Alpha3(t)
			}
			app.Spec.SecretInputRef = &applicationv1.SecretInputReference{Namespace: applicationv1.ApplicationNamespace, Name: "inputs", UID: "uid"}
			app.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{opaqueProtectedSecret("protected", WorkloadNamespace, "protected")}
			refreshDigest(t, app)
			app.Labels = producerLabels(app)
			err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
			if err == nil || !strings.HasPrefix(err.Reason, "InvalidPlanV1Alpha") {
				t.Fatalf("protected fields error = %#v", err)
			}
		})
	}
}

func TestReleaseAuthSecretValidationBoundaries(t *testing.T) {
	t.Run("valid matching basic auth", func(t *testing.T) {
		app := runAIPlanV1Alpha4(t)
		if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
			t.Fatalf("valid authSecret plan rejected: %v", err)
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(*applicationv1.OneKSApplication)
		reason string
	}{
		{"no matching protected Secret", func(app *applicationv1.OneKSApplication) {
			app.Spec.ProtectedSecrets = app.Spec.ProtectedSecrets[1:]
		}, "InvalidAuthSecretProtectedSecret"},
		{"wrong namespace", func(app *applicationv1.OneKSApplication) {
			app.Spec.ProtectedSecrets[0].Namespace = WorkloadNamespace
		}, "InvalidAuthSecretProtectedSecret"},
		{"wrong builder", func(app *applicationv1.OneKSApplication) {
			secret := &app.Spec.ProtectedSecrets[0]
			secret.BuilderType = applicationv1.ProtectedSecretBuilderOpaque
			secret.Username = ""
			secret.PasswordInputKey = ""
			secret.OpaqueData = []applicationv1.ProtectedSecretDataMapping{{Key: "TOKEN", InputKey: "ngcApiKey"}}
		}, "InvalidAuthSecretProtectedSecret"},
		{"different name", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.AuthSecret.Name = "other-repository-credentials"
		}, "InvalidAuthSecretProtectedSecret"},
		{"OCI repository", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.RepositoryURL = ""
			app.Spec.Release.Chart = "oci://registry.example.test/runai"
		}, "InvalidAuthSecretRepository"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := runAIPlanV1Alpha4(t)
			test.mutate(app)
			refreshV4(t, app)
			err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
			if err == nil || err.Reason != test.reason {
				t.Fatalf("authSecret validation error = %#v, want %s", err, test.reason)
			}
		})
	}
}

func TestEarlierPlansAndDependencyPlansRejectReleaseAuthSecret(t *testing.T) {
	for _, version := range []string{applicationv1.PlanVersionV1Alpha1, applicationv1.PlanVersionV1Alpha2, applicationv1.PlanVersionV1Alpha3} {
		t.Run(version, func(t *testing.T) {
			var app *applicationv1.OneKSApplication
			switch version {
			case applicationv1.PlanVersionV1Alpha1:
				app = goldenApplication(t)
			case applicationv1.PlanVersionV1Alpha2:
				app = validPlanV1Alpha2Dependency(t)
			default:
				app = validPlanV1Alpha3(t)
			}
			app.Spec.Release.AuthSecret = &applicationv1.HelmAuthSecretReference{Name: "repository-credentials"}
			refreshDigest(t, app)
			app.Labels = producerLabels(app)
			err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
			if err == nil || !strings.HasPrefix(err.Reason, "InvalidPlanV1Alpha") {
				t.Fatalf("pre-v4 authSecret error = %#v", err)
			}
		})
	}

	t.Run("dependency plan", func(t *testing.T) {
		plan := dependencyPlanForTest("dependency", "dependency-chart", nil)
		plan.Release.AuthSecret = &applicationv1.HelmAuthSecretReference{Name: "repository-credentials"}
		root := validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
		err := ValidatePlan(root, ValidationConfig{ClusterID: root.Spec.ClusterID})
		if err == nil || err.Reason != "InvalidDependencyAuthSecret" {
			t.Fatalf("dependency authSecret error = %#v", err)
		}
	})
}

func TestPlanV1Alpha4ProtectedFieldsAffectDigestWithoutSorting(t *testing.T) {
	base := runAIPlanV1Alpha4(t).Spec
	baseDigest := digestSpec(t, base)
	mutations := []func(*applicationv1.OneKSApplicationSpec){
		func(spec *applicationv1.OneKSApplicationSpec) { spec.SecretInputRef.Name = "other-input" },
		func(spec *applicationv1.OneKSApplicationSpec) { spec.SecretInputRef.UID = "other-uid" },
		func(spec *applicationv1.OneKSApplicationSpec) { spec.Release.AuthSecret.Name = "other-auth-secret" },
		func(spec *applicationv1.OneKSApplicationSpec) { spec.ProtectedSecrets[0].Username = "other-user" },
		func(spec *applicationv1.OneKSApplicationSpec) {
			spec.ProtectedSecrets[1].OpaqueData[0].InputKey = "other-input-key"
		},
		func(spec *applicationv1.OneKSApplicationSpec) {
			spec.ProtectedSecrets[2].Registry = "https://registry.example.com"
		},
		func(spec *applicationv1.OneKSApplicationSpec) { spec.ProtectedSecrets[2].Email = "other@example.com" },
		func(spec *applicationv1.OneKSApplicationSpec) {
			spec.ProtectedSecrets[0], spec.ProtectedSecrets[1] = spec.ProtectedSecrets[1], spec.ProtectedSecrets[0]
		},
	}
	for index, mutate := range mutations {
		copy := cloneSpecV3(t, base)
		mutate(&copy)
		if got := digestSpec(t, copy); got == baseDigest {
			t.Fatalf("mutation %d did not affect v4 digest", index)
		}
	}
}

func TestPlanV1Alpha4RootMaterializesUnchangedV1Alpha2DependencyChild(t *testing.T) {
	plan := dependencyPlanForTest("dependency", "dependency-chart", nil)
	root := validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.Spec.PlanVersion = applicationv1.PlanVersionV1Alpha4
	root.Spec.Resources = nil
	root.Spec.ManagedResources = nil
	root.Spec.SecretInputRef = &applicationv1.SecretInputReference{Namespace: applicationv1.ApplicationNamespace, Name: "inputs", UID: "input-uid"}
	root.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{opaqueProtectedSecret("protected", WorkloadNamespace, "protected")}
	refreshV4(t, root)
	reconciler, _ := testReconciler(t, root)
	_, _, conflict, err := reconciler.materializeRootDependencies(context.Background(), root)
	if err != nil || conflict != nil {
		t.Fatalf("materialize v4 dependency: conflict %#v, err %v", conflict, err)
	}
	child := &applicationv1.OneKSApplication{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: applicationv1.ApplicationNamespace, Name: plan.Name}, child); err != nil {
		t.Fatal(err)
	}
	if child.Spec.PlanVersion != applicationv1.PlanVersionV1Alpha2 || child.Spec.SecretInputRef != nil || child.Spec.ProtectedSecrets != nil {
		t.Fatalf("dependency child contract changed: %#v", child.Spec)
	}
	canonical, err := canonicalPlanV1Alpha2(child.Spec)
	if err != nil || Digest(canonical) != plan.PlanDigest {
		t.Fatalf("dependency child digest changed: %s, %v", Digest(canonical), err)
	}
}

func TestPlanV1Alpha4InputSecretPendingAndInvalidFailClosed(t *testing.T) {
	ctx := context.Background()
	missing := validPlanV1Alpha4(t)
	missing.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, recorder := testReconciler(t, missing)
	reconcileOnce(t, ctx, reconciler, missing)
	stored := getApplication(t, ctx, reconciler.Client, missing)
	assertConditionReason(t, stored, ConditionProtectedSecretsReady, "InputSecretMissing")
	assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: WorkloadNamespace, Name: "protected"}})
	assertNotFound(t, ctx, reconciler.Client, helmChartObject(missing.Spec.Release.ReleaseName))
	if containsWrite(recorder.childWrites, "Secret") || containsWrite(recorder.childWrites, "HelmChart") {
		t.Fatalf("missing input caused protected or Helm writes: %#v", recorder.childWrites)
	}

	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{"wrong UID", func(secret *corev1.Secret) { secret.UID = "replacement" }},
		{"mutable", func(secret *corev1.Secret) { secret.Immutable = nil }},
		{"wrong type", func(secret *corev1.Secret) { secret.Type = corev1.SecretTypeBasicAuth }},
		{"wrong keys", func(secret *corev1.Secret) { secret.Data["extra"] = []byte("not-reported") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := validPlanV1Alpha4(t)
			app.Finalizers = []string{applicationv1.ApplicationFinalizer}
			input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_INVALID")})
			test.mutate(input)
			reconciler, _ := testReconciler(t, app, input)
			reconcileOnce(t, ctx, reconciler, app)
			stored := getApplication(t, ctx, reconciler.Client, app)
			if stored.Status.LastError == nil || stored.Status.LastError.Reason != "InputSecretInvalid" || strings.Contains(stored.Status.LastError.Message, "SENTINEL") {
				t.Fatalf("invalid input status leaked or was not terminal: %#v", stored.Status)
			}
			assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: WorkloadNamespace, Name: "protected"}})
			assertNotFound(t, ctx, reconciler.Client, helmChartObject(app.Spec.Release.ReleaseName))
		})
	}
}

func TestPlanV1Alpha4RunAIProtectedSecretsMaterializeWithoutLeakingValues(t *testing.T) {
	ctx := context.Background()
	app := runAIPlanV1Alpha4(t)
	adminValue := "SENTINEL_ADMIN_5ebf88b7"
	ngcValue := "SENTINEL_NGC_20d58fc1"
	input := inputSecretFor(app, map[string][]byte{
		"adminPassword": []byte(adminValue),
		"ngcApiKey":     []byte(ngcValue),
	})
	reconciler, recorder := testReconciler(t, app, input)
	events := record.NewFakeRecorder(100)
	reconciler.Recorder = events

	reconcileOnce(t, ctx, reconciler, app) // finalizer first
	if strings.Contains(strings.Join(recorder.childWrites, ","), "Secret") {
		t.Fatalf("protected Secret was created before finalizer: %#v", recorder.childWrites)
	}
	reconcileOnce(t, ctx, reconciler, app)
	if got := strings.Join(recorder.childWrites, ","); got != "create:Secret,create:Secret,create:Secret,create:Secret,create:HelmChart" {
		t.Fatalf("protected credentials were not created before HelmChart: %s", got)
	}

	for _, resource := range app.Spec.ProtectedSecrets {
		secret := &corev1.Secret{}
		assertExists(t, ctx, reconciler.Client, secret, resource.Namespace, resource.Name)
		if !ownershipMatches(app, secret) {
			t.Fatalf("protected Secret %s lacks exact ownership", resource.ID)
		}
	}
	repository := getSecret(t, ctx, reconciler.Client, HelmChartNamespace, "runai-test-helm-repo-creds")
	if string(repository.Data[corev1.BasicAuthUsernameKey]) != "$oauthtoken" || string(repository.Data[corev1.BasicAuthPasswordKey]) != ngcValue {
		t.Fatal("basic auth Secret content mismatch")
	}
	admin := getSecret(t, ctx, reconciler.Client, WorkloadNamespace, "runai-test-admin-credentials")
	if string(admin.Data["ADMIN_PASSWORD"]) != adminValue {
		t.Fatal("opaque Secret content mismatch")
	}
	for _, namespace := range []string{WorkloadNamespace, "runai"} {
		registry := getSecret(t, ctx, reconciler.Client, namespace, "runai-test-registry-creds")
		var docker map[string]map[string]map[string]string
		if err := json.Unmarshal(registry.Data[corev1.DockerConfigJsonKey], &docker); err != nil {
			t.Fatal(err)
		}
		credentials := docker["auths"]["https://nvcr.io"]
		if credentials["username"] != "$oauthtoken" || credentials["password"] != ngcValue || credentials["email"] != "operator@example.com" {
			t.Fatal("Docker config Secret content mismatch")
		}
	}
	helm := helmChartObject(app.Spec.Release.ReleaseName)
	assertExists(t, ctx, reconciler.Client, helm, HelmChartNamespace, app.Spec.Release.ReleaseName)
	authSecretName, found, err := unstructured.NestedString(helm.Object, "spec", "authSecret", "name")
	if err != nil || !found || authSecretName != "runai-test-helm-repo-creds" {
		t.Fatalf("HelmChart authSecret = %q, found %v, err %v", authSecretName, found, err)
	}

	canonical, err := CanonicalPlan(app.Spec)
	if err != nil {
		t.Fatal(err)
	}
	stored := getApplication(t, ctx, reconciler.Client, app)
	serializedSpec, _ := json.Marshal(stored.Spec)
	serializedStatus, _ := json.Marshal(stored.Status)
	serializedHelm, _ := json.Marshal(helm.Object)
	eventText := drainEvents(events)
	for label, payload := range map[string][]byte{
		"canonical": canonical, "spec": serializedSpec, "HelmChart": serializedHelm,
		"status": serializedStatus, "events": []byte(eventText),
	} {
		if bytes.Contains(payload, []byte(adminValue)) || bytes.Contains(payload, []byte(ngcValue)) {
			t.Fatalf("%s leaked sentinel Secret input", label)
		}
	}
	if stored.Status.Progress.Total != 5 || len(stored.Status.Resources) != 4 {
		t.Fatalf("Run:ai protected progress/status = %#v", stored.Status)
	}
	assertConditionReason(t, stored, ConditionProtectedSecretsReady, "ProtectedSecretsReady")
}

func TestPlanV1Alpha4PreflightsEveryTargetBeforeMutation(t *testing.T) {
	ctx := context.Background()
	app := runAIPlanV1Alpha4(t)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("sentinel-a"), "ngcApiKey": []byte("sentinel-b")})
	foreignResource := app.Spec.ProtectedSecrets[len(app.Spec.ProtectedSecrets)-1]
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: foreignResource.Namespace, Name: foreignResource.Name}, Type: corev1.SecretTypeDockerConfigJson}
	reconciler, recorder := testReconciler(t, app, input, foreign)
	reconcileOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatal("ownership conflict added finalizer")
	}
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "OwnershipConflict" {
		t.Fatalf("ownership conflict not reported: %#v", stored.Status)
	}
	if len(recorder.childWrites) != 0 {
		t.Fatalf("preflight conflict mutated children: %#v", recorder.childWrites)
	}
}

func TestPlanV1Alpha4RepairsOwnedTargetDriftAndObserveNeverMutates(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha4(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_REPAIR")})
	desired, err := desiredProtectedSecret(app, app.Spec.ProtectedSecrets[0], input)
	if err != nil {
		t.Fatal(err)
	}
	drifted := desired.DeepCopy()
	drifted.Type = corev1.SecretTypeBasicAuth
	drifted.Data = map[string][]byte{"wrong": []byte("drift")}
	reconciler, recorder := testReconciler(t, app, input, drifted)
	reconcileOnce(t, ctx, reconciler, app)
	repaired := getSecret(t, ctx, reconciler.Client, WorkloadNamespace, "protected")
	if repaired.Type != corev1.SecretTypeOpaque || string(repaired.Data["ADMIN_PASSWORD"]) != "SENTINEL_REPAIR" {
		t.Fatalf("owned drift was not repaired: %#v", repaired)
	}
	if !containsWrite(recorder.childWrites, "update:Secret") {
		t.Fatalf("Secret update not recorded: %#v", recorder.childWrites)
	}

	observe := validPlanV1Alpha4(t)
	observe.Spec.ExecutionMode = applicationv1.ExecutionModeObserve
	refreshV4(t, observe)
	observeInput := inputSecretFor(observe, map[string][]byte{"adminPassword": []byte("SENTINEL_OBSERVE")})
	reconciler, recorder = testReconciler(t, observe, observeInput)
	reconcileOnce(t, ctx, reconciler, observe)
	stored := getApplication(t, ctx, reconciler.Client, observe)
	if stored.Status.Phase != applicationv1.PhaseObserving || len(stored.Finalizers) != 0 || len(recorder.childWrites) != 0 {
		t.Fatalf("Observe mutated v4 state: status=%#v writes=%#v", stored.Status, recorder.childWrites)
	}
}

func TestPlanV1Alpha4ProtectedCreateAlreadyExistsRaceRereadsOwnership(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha4(t)
	app.Spec.ManagedResources = nil
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	refreshV4(t, app)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_RACE")})
	existing, err := desiredProtectedSecret(app, app.Spec.ProtectedSecrets[0], input)
	if err != nil {
		t.Fatal(err)
	}
	existing.Data["ADMIN_PASSWORD"] = []byte("drifted")
	reconciler, recorder := testReconciler(t, app, input, existing)
	reconciler.APIReader = &protectedRaceReader{
		Reader: reconciler.Client,
		target: types.NamespacedName{Namespace: existing.Namespace, Name: existing.Name},
	}
	reconcileOnce(t, ctx, reconciler, app)
	repaired := getSecret(t, ctx, reconciler.Client, existing.Namespace, existing.Name)
	if string(repaired.Data["ADMIN_PASSWORD"]) != "SENTINEL_RACE" {
		t.Fatalf("AlreadyExists race did not repair owned target")
	}
	if !containsWrite(recorder.childWrites, "create:Secret") || !containsWrite(recorder.childWrites, "update:Secret") {
		t.Fatalf("race did not exercise create/re-read/update: %#v", recorder.childWrites)
	}
}

func TestPlanV1Alpha4ManagedResourcesGateProtectedSecretsAndHelm(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha4(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources[0].Readiness.Conditions = []applicationv1.ManagedResourceCondition{{Type: "Ready", Status: "True"}}
	refreshV4(t, app)
	managed, _ := desiredManagedResource(app, app.Spec.ManagedResources[0])
	managed.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_GATED")})
	reconciler, recorder := testReconciler(t, app, managed, input)
	reconcileOnce(t, ctx, reconciler, app)
	if containsWrite(recorder.childWrites, "Secret") || containsWrite(recorder.childWrites, "HelmChart") {
		t.Fatalf("managed readiness gate allowed later effects: %#v", recorder.childWrites)
	}
	stored := getApplication(t, ctx, reconciler.Client, app)
	assertConditionReason(t, stored, ConditionProtectedSecretsReady, "ManagedResourcesPending")
}

func TestPlanV1Alpha4DeletionOrdersTargetsSourceManagedAndFinalizer(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha4(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources = nil
	app.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{
		opaqueProtectedSecret("retained", WorkloadNamespace, "retained"),
		opaqueProtectedSecret("deleted", WorkloadNamespace, "deleted"),
	}
	app.Spec.ProtectedSecrets[0].DeletionPolicy = applicationv1.DeletionPolicyRetain
	refreshV4(t, app)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_DELETE")})
	retained, _ := desiredProtectedSecret(app, app.Spec.ProtectedSecrets[0], input)
	deleted, _ := desiredProtectedSecret(app, app.Spec.ProtectedSecrets[1], input)
	retained.UID, retained.ResourceVersion = "retained-uid", "1"
	deleted.UID, deleted.ResourceVersion = "deleted-uid", "1"
	reconciler, recorder := testReconciler(t, app, input, retained, deleted)
	if err := reconciler.Delete(ctx, app); err != nil {
		t.Fatal(err)
	}
	recorder.deletePreconditions = nil
	deleting := getApplication(t, ctx, reconciler.Client, app)
	reconcileOnce(t, ctx, reconciler, deleting)
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, WorkloadNamespace, "retained")
	assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: WorkloadNamespace, Name: "deleted"}})
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, input.Namespace, input.Name)

	deleting = getApplication(t, ctx, reconciler.Client, app)
	reconcileOnce(t, ctx, reconciler, deleting)
	assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: input.Namespace, Name: input.Name}})
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, WorkloadNamespace, "retained")
	for index, preconditioned := range recorder.deletePreconditions {
		if !preconditioned {
			t.Fatalf("Secret delete %d lacked UID/resourceVersion preconditions", index)
		}
	}
}

func TestPlanV1Alpha4DeletionPreflightsAllTargetsBeforeDeletingAny(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha4(t)
	app.Spec.ManagedResources = nil
	app.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{
		opaqueProtectedSecret("owned", WorkloadNamespace, "owned"),
		opaqueProtectedSecret("foreign", WorkloadNamespace, "foreign"),
	}
	refreshV4(t, app)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_DELETE_PREFLIGHT")})
	owned, _ := desiredProtectedSecret(app, app.Spec.ProtectedSecrets[0], input)
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: WorkloadNamespace, Name: "foreign"}, Type: corev1.SecretTypeOpaque}
	reconciler, recorder := testReconciler(t, app, input, owned, foreign)
	pending, err := reconciler.reconcileDeleteProtectedSecrets(ctx, app)
	var conflict *OwnershipConflictError
	if pending || !errors.As(err, &conflict) {
		t.Fatalf("delete preflight = pending %v, err %#v", pending, err)
	}
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, WorkloadNamespace, "owned")
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, WorkloadNamespace, "foreign")
	if containsWrite(recorder.childWrites, "delete:Secret") {
		t.Fatalf("foreign target allowed partial deletion: %#v", recorder.childWrites)
	}
}

func TestPlanV1Alpha4DeletionDoesNotDeleteReplacementInputSecret(t *testing.T) {
	ctx := context.Background()
	app := validPlanV1Alpha4(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources = nil
	refreshV4(t, app)
	replacement := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_REPLACEMENT")})
	replacement.UID = "replacement-uid"
	reconciler, _ := testReconciler(t, app, replacement)
	if err := reconciler.Delete(ctx, app); err != nil {
		t.Fatal(err)
	}
	deleting := getApplication(t, ctx, reconciler.Client, app)
	reconcileOnce(t, ctx, reconciler, deleting)
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, replacement.Namespace, replacement.Name)
	remaining := &applicationv1.OneKSApplication{}
	err := reconciler.Get(ctx, client.ObjectKeyFromObject(app), remaining)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestPlanV1Alpha4UsesAuthoritativeInputReader(t *testing.T) {
	app := validPlanV1Alpha4(t)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_AUTHORITATIVE")})
	reconciler, _ := testReconciler(t, app)
	authoritative := fake.NewClientBuilder().WithScheme(reconciler.Scheme).WithObjects(input).Build()
	reconciler.APIReader = authoritative
	observed, missing, err := reconciler.readSecretInput(context.Background(), app)
	if err != nil || missing || observed == nil || string(observed.Data["adminPassword"]) != "SENTINEL_AUTHORITATIVE" {
		t.Fatalf("authoritative input read = %#v, missing %v, err %v", observed, missing, err)
	}
}

func TestProtectedSecretAPIErrorsNeverExposeErrorPayloads(t *testing.T) {
	sentinel := "SENTINEL_API_BODY_MUST_NOT_LEAK"
	cause := errors.New(sentinel)
	err := &protectedSecretAPIError{operation: "read input", namespace: applicationv1.ApplicationNamespace, name: "inputs", cause: cause}
	if strings.Contains(err.Error(), sentinel) || !errors.Is(err, cause) {
		t.Fatalf("protected API error leaked payload or lost cause: %v", err)
	}
}

func validPlanV1Alpha4(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := validPlanV1Alpha3(t)
	app.Spec.PlanVersion = applicationv1.PlanVersionV1Alpha4
	app.Spec.SecretInputRef = &applicationv1.SecretInputReference{
		Namespace: applicationv1.ApplicationNamespace,
		Name:      "operator-inputs",
		UID:       "input-uid",
	}
	app.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{
		opaqueProtectedSecret("protected", WorkloadNamespace, "protected"),
	}
	refreshV4(t, app)
	return app
}

func runAIPlanV1Alpha4(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := validPlanV1Alpha4(t)
	app.Name = "runai-test-application"
	app.Spec.Release.ReleaseName = "runai-test"
	app.Spec.Release.AuthSecret = &applicationv1.HelmAuthSecretReference{Name: "runai-test-helm-repo-creds"}
	app.Spec.Release.ValuesContent = `global:
  imagePullSecrets:
    - name: runai-backend-registry-creds
tenantsManager:
  config:
    existingSecret: runai-backend-admin-credentials
    secretKeys:
      adminPasswordKey: ADMIN_PASSWORD
keycloakx:
  imagePullSecrets:
    - name: runai-backend-registry-creds
`
	app.Spec.ManagedResources = nil
	app.Spec.SecretInputRef = &applicationv1.SecretInputReference{
		Namespace: applicationv1.ApplicationNamespace,
		Name:      "runai-test-inputs",
		UID:       "runai-input-uid",
	}
	app.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{
		{
			ID: "runai-helm-repository-credentials", Namespace: HelmChartNamespace,
			Name: "runai-test-helm-repo-creds", BuilderType: applicationv1.ProtectedSecretBuilderBasicAuth,
			Username: "$oauthtoken", PasswordInputKey: "ngcApiKey", DeletionPolicy: applicationv1.DeletionPolicyDelete,
		},
		{
			ID: "runai-admin-credentials", Namespace: WorkloadNamespace,
			Name: "runai-test-admin-credentials", BuilderType: applicationv1.ProtectedSecretBuilderOpaque,
			OpaqueData:     []applicationv1.ProtectedSecretDataMapping{{Key: "ADMIN_PASSWORD", InputKey: "adminPassword"}},
			DeletionPolicy: applicationv1.DeletionPolicyDelete,
		},
		dockerProtectedSecret("runai-registry-credentials", WorkloadNamespace, "runai-test-registry-creds"),
		dockerProtectedSecret("runai-cluster-registry-credentials", "runai", "runai-test-registry-creds"),
	}
	refreshV4(t, app)
	return app
}

func opaqueProtectedSecret(id, namespace, name string) applicationv1.ProtectedSecretSpec {
	return applicationv1.ProtectedSecretSpec{
		ID: id, Namespace: namespace, Name: name,
		BuilderType:    applicationv1.ProtectedSecretBuilderOpaque,
		OpaqueData:     []applicationv1.ProtectedSecretDataMapping{{Key: "ADMIN_PASSWORD", InputKey: "adminPassword"}},
		DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
}

func dockerProtectedSecret(id, namespace, name string) applicationv1.ProtectedSecretSpec {
	return applicationv1.ProtectedSecretSpec{
		ID: id, Namespace: namespace, Name: name,
		BuilderType: applicationv1.ProtectedSecretBuilderDockerConfigJSON,
		Registry:    "https://nvcr.io", Username: "$oauthtoken", PasswordInputKey: "ngcApiKey", Email: "operator@example.com",
		DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
}

func managedTargetNamespaceResource() applicationv1.ManagedResourceSpec {
	return applicationv1.ManagedResourceSpec{
		ID: "target-namespace", Scope: applicationv1.ManagedResourceScopeCluster,
		APIVersion: "v1", Kind: "Namespace", APIResource: "namespaces", Name: WorkloadNamespace,
		ManifestJSON:   `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"` + WorkloadNamespace + `"}}`,
		Readiness:      applicationv1.ManagedResourceReadiness{TimeoutSeconds: 60},
		DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
}

func inputSecretFor(app *applicationv1.OneKSApplication, data map[string][]byte) *corev1.Secret {
	immutable := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       app.Spec.SecretInputRef.Namespace,
			Name:            app.Spec.SecretInputRef.Name,
			UID:             types.UID(app.Spec.SecretInputRef.UID),
			ResourceVersion: "1",
		},
		Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: data,
	}
}

func refreshV4(t *testing.T, app *applicationv1.OneKSApplication) {
	t.Helper()
	refreshDigest(t, app)
	app.Labels = producerLabels(app)
}

func getSecret(t *testing.T, ctx context.Context, kubeClient client.Client, namespace, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		t.Fatal(err)
	}
	return secret
}

func assertConditionReason(t *testing.T, app *applicationv1.OneKSApplication, conditionType, reason string) {
	t.Helper()
	condition := conditionByType(app.Status.Conditions, conditionType)
	if condition == nil || condition.Reason != reason {
		t.Fatalf("condition %s = %#v, want reason %s", conditionType, condition, reason)
	}
}

func containsWrite(writes []string, fragment string) bool {
	for _, write := range writes {
		if strings.Contains(write, fragment) {
			return true
		}
	}
	return false
}

type protectedRaceReader struct {
	client.Reader
	target types.NamespacedName
	gets   int
}

func (r *protectedRaceReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, secret := object.(*corev1.Secret); secret && key == r.target {
		r.gets++
		if r.gets <= 4 {
			return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func drainEvents(recorder *record.FakeRecorder) string {
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return strings.Join(events, "\n")
		}
	}
}
