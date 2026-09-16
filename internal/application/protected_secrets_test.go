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
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestTargetNamespaceBootstrap(t *testing.T) {
	for _, managed := range []bool{true, false} {
		t.Run(map[bool]string{true: "managed through resource DAG", false: "missing unmanaged target"}[managed], func(t *testing.T) {
			app := validManagedRootPlan(t)
			if managed {
				app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{managedTargetNamespaceResource()}
				refreshOwnedPlan(t, app)
			}
			reconciler, recorder := testReconciler(t, app)
			requireNoError(t, reconciler.Client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "catalogue-workloads"}}), "delete target namespace")
			recorder.childWrites = nil

			stored := reconcileAndGet(t, ctx, reconciler, app)
			if !controllerutil.ContainsFinalizer(stored, applicationv1.ApplicationFinalizer) {
				t.Fatalf("target namespace handling lost application finalizer: %#v", stored.Finalizers)
			}
			if !managed {
				stored = reconcileAndGet(t, ctx, reconciler, app)
				assertLastErrorReason(t, stored, "TargetNamespaceMissing")
				assertNoChildWrites(t, recorder)
				return
			}
			if stored.Status.LastError != nil && stored.Status.LastError.Reason == "TargetNamespaceMissing" {
				t.Fatalf("managed namespace bootstrap was rejected: %#v", stored.Status.LastError)
			}
			reconcileOnce(t, ctx, reconciler, app)
			assertExists(t, ctx, reconciler.Client, &corev1.Namespace{}, "", "catalogue-workloads")
			if !containsWrite(recorder.childWrites, "create:Namespace") {
				t.Fatalf("normal managed DAG did not create target namespace: %#v", recorder.childWrites)
			}
		})
	}
}

func TestProtectedSecretWaitsForManagedTargetNamespace(t *testing.T) {
	app := validBoundProtectedRootPlan(t)
	app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{managedTargetNamespaceResource()}
	refreshOwnedPlan(t, app)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_NAMESPACE_BOOTSTRAP")})
	reconciler, recorder := testReconciler(t, app, input)
	requireNoError(t, reconciler.Client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "catalogue-workloads"}}), "delete target namespace")
	recorder.childWrites = nil

	reconcileOnce(t, ctx, reconciler, app)
	assertNotFound(t, ctx, reconciler.Client, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "catalogue-workloads"}})
	assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "catalogue-workloads", Name: "protected"}})

	recorder.childWrites = nil
	reconcileOnce(t, ctx, reconciler, app)
	assertExists(t, ctx, reconciler.Client, &corev1.Namespace{}, "", "catalogue-workloads")
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, "catalogue-workloads", "protected")
	writes := strings.Join(recorder.childWrites, ",")
	if !strings.HasPrefix(writes, "create:Namespace,create:Secret") {
		t.Fatalf("protected Secret was not sequenced after managed namespace readiness: %s", writes)
	}
}

func TestInputSecretInvalidKeepsPlanValid(t *testing.T) {
	app := withApplicationFinalizer(validBoundProtectedRootPlan(t))
	app.Spec.ManagedResources = nil
	refreshOwnedPlan(t, app)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_INVALID_RUNTIME")})
	input.UID = "replacement-uid"
	reconciler, _ := testReconciler(t, app, input)

	stored := reconcileAndGet(t, ctx, reconciler, app)
	if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.LastError == nil || stored.Status.LastError.Reason != "InputSecretInvalid" {
		t.Fatalf("runtime input failure status = %#v", stored.Status)
	}
	for conditionType, want := range map[string]metav1.ConditionStatus{
		ConditionPlanValid:             metav1.ConditionTrue,
		ConditionProtectedSecretsReady: metav1.ConditionFalse,
		ConditionReady:                 metav1.ConditionFalse,
	} {
		condition := meta.FindStatusCondition(stored.Status.Conditions, conditionType)
		if condition == nil || condition.Status != want {
			t.Fatalf("condition %s = %#v, want %s", conditionType, condition, want)
		}
	}
}

func TestProtectedSecretPlanValidatesStructuredProtectedSecretContract(t *testing.T) {
	app := validBoundProtectedRootPlan(t)
	assertPlanValid(t, app)

	tests := []struct {
		name   string
		reason string
		mutate func(*applicationv1.OneKSApplication)
	}{
		{"source target collision", "ProtectedSecretInputIdentityCollision", func(app *applicationv1.OneKSApplication) {
			app.Spec.ProtectedSecrets[0].Namespace = app.Spec.SecretInputRef.Namespace
			app.Spec.ProtectedSecrets[0].Name = app.Spec.SecretInputRef.Name
		}},
		{"unresolved content", "UnresolvedPlaceholder", func(app *applicationv1.OneKSApplication) {
			app.Spec.ProtectedSecrets[0].BuilderType = applicationv1.ProtectedSecretBuilderBasicAuth
			app.Spec.ProtectedSecrets[0].Username = "${unknown}"
			app.Spec.ProtectedSecrets[0].PasswordInputKey = "password"
			app.Spec.ProtectedSecrets[0].OpaqueData = nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := validBoundProtectedRootPlan(t)
			test.mutate(candidate)
			refreshOwnedPlan(t, candidate)
			assertPlanError(t, candidate, test.reason)
		})
	}
}

func TestProtectedSecretPlanInputSecretPendingAndInvalidFailClosed(t *testing.T) {
	missing := withApplicationFinalizer(validBoundProtectedRootPlan(t))
	reconciler, recorder := testReconciler(t, missing)
	stored := reconcileAndGet(t, ctx, reconciler, missing)
	assertConditionReason(t, stored, ConditionProtectedSecretsReady, "InputSecretMissing")
	assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "catalogue-workloads", Name: "protected"}})
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
			app := withApplicationFinalizer(validBoundProtectedRootPlan(t))
			input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_INVALID")})
			test.mutate(input)
			reconciler, _ := testReconciler(t, app, input)
			stored := reconcileAndGet(t, ctx, reconciler, app)
			if stored.Status.LastError == nil || stored.Status.LastError.Reason != "InputSecretInvalid" || strings.Contains(stored.Status.LastError.Message, "SENTINEL") {
				t.Fatalf("invalid input status leaked or was not terminal: %#v", stored.Status)
			}
			assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "catalogue-workloads", Name: "protected"}})
			assertNotFound(t, ctx, reconciler.Client, helmChartObject(app.Spec.Release.ReleaseName))
		})
	}
}

func TestProtectedSecretPlanRunAIProtectedSecretsMaterializeWithoutLeakingValues(t *testing.T) {
	app := runAIProtectedPlan(t)
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
	admin := getSecret(t, ctx, reconciler.Client, "catalogue-workloads", "runai-test-admin-credentials")
	if string(admin.Data["ADMIN_PASSWORD"]) != adminValue {
		t.Fatal("opaque Secret content mismatch")
	}
	for _, namespace := range []string{"catalogue-workloads", "runai"} {
		registry := getSecret(t, ctx, reconciler.Client, namespace, "runai-test-registry-creds")
		var docker map[string]map[string]map[string]string
		requireNoError(t, json.Unmarshal(registry.Data[corev1.DockerConfigJsonKey], &docker), "decode registry credentials")
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

	stored := getApplication(t, ctx, reconciler.Client, app)
	serializedSpec, _ := json.Marshal(stored.Spec)
	serializedStatus, _ := json.Marshal(stored.Status)
	serializedHelm, _ := json.Marshal(helm.Object)
	eventText := drainEvents(events)
	for label, payload := range map[string][]byte{
		"spec": serializedSpec, "HelmChart": serializedHelm,
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

func TestProtectedSecretPlanPreflightsEveryTargetBeforeMutation(t *testing.T) {
	app := runAIProtectedPlan(t)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("sentinel-a"), "ngcApiKey": []byte("sentinel-b")})
	foreignResource := app.Spec.ProtectedSecrets[len(app.Spec.ProtectedSecrets)-1]
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: foreignResource.Namespace, Name: foreignResource.Name}, Type: corev1.SecretTypeDockerConfigJson}
	reconciler, recorder := testReconciler(t, app, input, foreign)
	reconcileOnce(t, ctx, reconciler, app)
	assertNoChildWrites(t, recorder)
	stored := reconcileAndGet(t, ctx, reconciler, app)
	if !controllerutil.ContainsFinalizer(stored, applicationv1.ApplicationFinalizer) {
		t.Fatal("current plan did not acquire its cleanup finalizer")
	}
	assertLastErrorReason(t, stored, "OwnershipConflict")
	assertNoChildWrites(t, recorder)
}

func TestProtectedSecretPlanRepairsOwnedTargetDrift(t *testing.T) {
	app := withApplicationFinalizer(validBoundProtectedRootPlan(t))
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
	repaired := getSecret(t, ctx, reconciler.Client, "catalogue-workloads", "protected")
	if repaired.Type != corev1.SecretTypeOpaque || string(repaired.Data["ADMIN_PASSWORD"]) != "SENTINEL_REPAIR" {
		t.Fatalf("owned drift was not repaired: %#v", repaired)
	}
	if !containsWrite(recorder.childWrites, "update:Secret") {
		t.Fatalf("Secret update not recorded: %#v", recorder.childWrites)
	}

}

func TestProtectedSecretPlanProtectedCreateAlreadyExistsRaceRetriesBeforeRepair(t *testing.T) {
	app := withApplicationFinalizer(validBoundProtectedRootPlan(t))
	app.Spec.ManagedResources = nil
	refreshOwnedPlan(t, app)
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
	reconcileOnce(t, ctx, reconciler, app)
	repaired := getSecret(t, ctx, reconciler.Client, existing.Namespace, existing.Name)
	if string(repaired.Data["ADMIN_PASSWORD"]) != "SENTINEL_RACE" {
		t.Fatalf("AlreadyExists race did not repair owned target")
	}
	if !containsWrite(recorder.childWrites, "create:Secret") || !containsWrite(recorder.childWrites, "update:Secret") {
		t.Fatalf("race did not exercise create/retry/update: %#v", recorder.childWrites)
	}
}

func TestProtectedSecretPlanManagedResourcesGateProtectedSecretsAndHelm(t *testing.T) {
	app := withApplicationFinalizer(validBoundProtectedRootPlan(t))
	app.Spec.ManagedResources[0].Readiness.Conditions = []applicationv1.ManagedResourceCondition{{Type: "Ready", Status: "True"}}
	refreshOwnedPlan(t, app)
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

func TestProtectedSecretPlanDeletionOrdersTargetsSourceManagedAndFinalizer(t *testing.T) {
	app := withApplicationFinalizer(validBoundProtectedRootPlan(t))
	app.Spec.ManagedResources = nil
	app.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{
		opaqueProtectedSecret("retained", "catalogue-workloads", "retained"),
		opaqueProtectedSecret("deleted", "catalogue-workloads", "deleted"),
	}
	app.Spec.ProtectedSecrets[0].DeletionPolicy = applicationv1.DeletionPolicyRetain
	refreshOwnedPlan(t, app)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_DELETE")})
	retained, _ := desiredProtectedSecret(app, app.Spec.ProtectedSecrets[0], input)
	deleted, _ := desiredProtectedSecret(app, app.Spec.ProtectedSecrets[1], input)
	retained.UID, retained.ResourceVersion = "retained-uid", "1"
	deleted.UID, deleted.ResourceVersion = "deleted-uid", "1"
	reconciler, recorder := testReconciler(t, app, input, retained, deleted)
	requireNoError(t, reconciler.Delete(ctx, app), "delete application")
	recorder.deletePreconditions = nil
	deleting := getApplication(t, ctx, reconciler.Client, app)
	reconcileOnce(t, ctx, reconciler, deleting)
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, "catalogue-workloads", "retained")
	assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "catalogue-workloads", Name: "deleted"}})
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, input.Namespace, input.Name)

	deleting = getApplication(t, ctx, reconciler.Client, app)
	reconcileOnce(t, ctx, reconciler, deleting)
	assertNotFound(t, ctx, reconciler.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: input.Namespace, Name: input.Name}})
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, "catalogue-workloads", "retained")
	for index, preconditioned := range recorder.deletePreconditions {
		if !preconditioned {
			t.Fatalf("Secret delete %d lacked UID/resourceVersion preconditions", index)
		}
	}
}

func TestProtectedSecretPlanDeletionPreflightsAllTargetsBeforeDeletingAny(t *testing.T) {
	app := validBoundProtectedRootPlan(t)
	app.Spec.ManagedResources = nil
	app.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{
		opaqueProtectedSecret("owned", "catalogue-workloads", "owned"),
		opaqueProtectedSecret("foreign", "catalogue-workloads", "foreign"),
	}
	refreshOwnedPlan(t, app)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_DELETE_PREFLIGHT")})
	owned, _ := desiredProtectedSecret(app, app.Spec.ProtectedSecrets[0], input)
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "catalogue-workloads", Name: "foreign"}, Type: corev1.SecretTypeOpaque}
	reconciler, recorder := testReconciler(t, app, input, owned, foreign)
	pending, err := reconciler.reconcileDeleteProtectedSecrets(ctx, app)
	var conflict *OwnershipConflictError
	if pending || !errors.As(err, &conflict) {
		t.Fatalf("delete preflight = pending %v, err %#v", pending, err)
	}
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, "catalogue-workloads", "owned")
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, "catalogue-workloads", "foreign")
	if containsWrite(recorder.childWrites, "delete:Secret") {
		t.Fatalf("foreign target allowed partial deletion: %#v", recorder.childWrites)
	}
}

func TestProtectedSecretPlanDeletionDoesNotDeleteReplacementInputSecret(t *testing.T) {
	app := withApplicationFinalizer(validBoundProtectedRootPlan(t))
	app.Spec.ManagedResources = nil
	refreshOwnedPlan(t, app)
	replacement := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_REPLACEMENT")})
	replacement.UID = "replacement-uid"
	reconciler, _ := testReconciler(t, app, replacement)
	requireNoError(t, reconciler.Delete(ctx, app), "delete application")
	deleting := getApplication(t, ctx, reconciler.Client, app)
	reconcileOnce(t, ctx, reconciler, deleting)
	assertExists(t, ctx, reconciler.Client, &corev1.Secret{}, replacement.Namespace, replacement.Name)
	remaining := &applicationv1.OneKSApplication{}
	err := reconciler.Get(ctx, client.ObjectKeyFromObject(app), remaining)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestProtectedSecretPlanUsesAuthoritativeInputReader(t *testing.T) {
	app := validBoundProtectedRootPlan(t)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("SENTINEL_AUTHORITATIVE")})
	reconciler, _ := testReconciler(t, app)
	authoritative := authoritativeClient(reconciler, input)
	reconciler.APIReader = authoritative
	observed, missing, err := reconciler.readSecretInput(context.Background(), app)
	if err != nil || missing || observed == nil || string(observed.Data["adminPassword"]) != "SENTINEL_AUTHORITATIVE" {
		t.Fatalf("authoritative input read = %#v, missing %v, err %v", observed, missing, err)
	}
}

func TestCurrentPlanBindsInputUIDBeforeExecution(t *testing.T) {
	app := validProtectedRootPlan(t)
	input := inputSecretFor(app, map[string][]byte{"adminPassword": []byte("secret")})
	input.UID = "input-v5-uid"
	input.Labels = map[string]string{
		LabelRootManagedBy: RootManagedByValue, LabelProducer: ProducerValue,
		LabelClusterID: app.Spec.ClusterID, LabelCatalogueChartID: app.Spec.CatalogueChartID,
		LabelApplicationName: app.Name,
	}
	reconciler, recorder := testReconciler(t, app, input)

	stored := reconcileAndGet(t, ctx, reconciler, app)
	if !controllerutil.ContainsFinalizer(stored, applicationv1.ApplicationFinalizer) {
		t.Fatalf("v5 finalizer was not acquired before input binding")
	}
	if stored.Status.SecretInputUID != "" || len(recorder.childWrites) != 0 {
		t.Fatalf("v5 executed before finalizer acquisition: status=%#v writes=%#v", stored.Status, recorder.childWrites)
	}

	reconcileOnce(t, ctx, reconciler, app)
	stored = getApplication(t, ctx, reconciler.Client, app)
	assertNoChildWrites(t, recorder)
	if stored.Status.SecretInputUID != "input-v5-uid" {
		t.Fatalf("bound UID = %q", stored.Status.SecretInputUID)
	}

	replacement := input.DeepCopy()
	replacement.UID = "replacement-uid"
	reconciler.APIReader = authoritativeClient(reconciler, replacement)
	if _, _, err := reconciler.readSecretInput(ctx, stored); err == nil || !strings.Contains(err.Error(), "UID") {
		t.Fatalf("replacement input error = %v", err)
	}
}

func TestCurrentPlanHelmOnlyRootDoesNotRequireProtectedSecrets(t *testing.T) {
	app := validProtectedRootPlan(t)
	app.Spec.Release.AuthSecret = nil
	app.Spec.SecretInputRef = nil
	app.Spec.ProtectedSecrets = nil
	app.Spec.ManagedResources = nil
	refreshOwnedPlan(t, app)

	assertPlanValid(t, app)
	if usesProtectedSecrets(app) {
		t.Fatal("Helm-only v1beta1 Root entered protected Secret lifecycle")
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

func validBoundProtectedRootPlan(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := validManagedRootPlan(t)
	app.Spec.PlanVersion = applicationv1.PlanVersion
	app.Spec.SecretInputRef = &applicationv1.SecretInputReference{
		Namespace: applicationv1.ApplicationNamespace,
		Name:      "operator-inputs",
	}
	app.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{
		opaqueProtectedSecret("protected", "catalogue-workloads", "protected"),
	}
	app.Status.SecretInputUID = "input-uid"
	refreshOwnedPlan(t, app)
	return app
}

func validProtectedRootPlan(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := validBoundProtectedRootPlan(t)
	app.Status.SecretInputUID = ""
	return app
}

func runAIProtectedPlan(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := validBoundProtectedRootPlan(t)
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
	}
	app.Status.SecretInputUID = "runai-input-uid"
	app.Spec.ProtectedSecrets = []applicationv1.ProtectedSecretSpec{
		{
			ID: "runai-helm-repository-credentials", Namespace: HelmChartNamespace,
			Name: "runai-test-helm-repo-creds", BuilderType: applicationv1.ProtectedSecretBuilderBasicAuth,
			Username: "$oauthtoken", PasswordInputKey: "ngcApiKey", DeletionPolicy: applicationv1.DeletionPolicyDelete,
		},
		{
			ID: "runai-admin-credentials", Namespace: "catalogue-workloads",
			Name: "runai-test-admin-credentials", BuilderType: applicationv1.ProtectedSecretBuilderOpaque,
			OpaqueData:     []applicationv1.ProtectedSecretDataMapping{{Key: "ADMIN_PASSWORD", InputKey: "adminPassword"}},
			DeletionPolicy: applicationv1.DeletionPolicyDelete,
		},
		dockerProtectedSecret("runai-registry-credentials", "catalogue-workloads", "runai-test-registry-creds"),
		dockerProtectedSecret("runai-cluster-registry-credentials", "runai", "runai-test-registry-creds"),
	}
	refreshOwnedPlan(t, app)
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
		APIVersion: "v1", Kind: "Namespace", Name: "catalogue-workloads",
		ManifestJSON:   `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"` + "catalogue-workloads" + `"}}`,
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
			UID:             types.UID(app.Status.SecretInputUID),
			ResourceVersion: "1",
			Labels: map[string]string{
				LabelRootManagedBy: RootManagedByValue, LabelProducer: ProducerValue,
				LabelClusterID: app.Spec.ClusterID, LabelCatalogueChartID: app.Spec.CatalogueChartID,
				LabelApplicationName: app.Name,
			},
		},
		Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: data,
	}
}

func getSecret(t *testing.T, ctx context.Context, kubeClient client.Client, namespace, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	requireNoError(t, kubeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret), "get Secret")
	return secret
}

func assertConditionReason(t *testing.T, app *applicationv1.OneKSApplication, conditionType, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(app.Status.Conditions, conditionType)
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
