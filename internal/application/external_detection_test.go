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
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExternalDetectionMaterializesAndChangesCanonicalDigests(t *testing.T) {
	plan := validDependencyPlan()
	baselineRoot := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	withoutCanonical, err := canonicalPlan(dependencyPlanChildSpec("42", plan))
	if err != nil {
		t.Fatalf("canonicalize dependency without detector: %v", err)
	}
	if got := Digest(withoutCanonical); got != "sha256-GHKwcVdzJtmSmOdJeQHMbAp_7L2TDsoLGnO7KeRq5zw" {
		t.Fatalf("dependency digest without detector = %s", got)
	}
	plan.ExternalDetection = &applicationv1.ExternalDetectionSpec{Detector: applicationv1.ExternalDetectorCertManager}
	refreshDependencyPlanDigestForTest("42", &plan)
	child := dependencyPlanChildSpec("42", plan)
	if child.ExternalDetection == nil || child.ExternalDetection.Detector != applicationv1.ExternalDetectorCertManager {
		t.Fatalf("external detector was not copied to child: %#v", child.ExternalDetection)
	}
	withCanonical, err := canonicalPlan(child)
	if err != nil {
		t.Fatalf("canonicalize dependency with detector: %v", err)
	}
	if reflect.DeepEqual(withoutCanonical, withCanonical) || !strings.Contains(string(withCanonical), `"externalDetection":{"detector":"cert-manager"}`) {
		t.Fatalf("external detector did not affect child canonical input: %s", withCanonical)
	}
	if got := Digest(withCanonical); got != "sha256-AZfmrfCmm7jJyLKA4z_8mqJ6bRtC84arLnv0Kyf8EG4" {
		t.Fatalf("cross-language external detector digest = %s", got)
	}

	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	if err := ValidatePlan(root, ValidationConfig{ClusterID: "42"}); err != nil {
		t.Fatalf("validate Root external dependency plan: %v", err)
	}
	rootCanonical, err := CanonicalPlan(root.Spec)
	if err != nil || !strings.Contains(string(rootCanonical), `"externalDetection":{"detector":"cert-manager"}`) {
		t.Fatalf("Root canonical input omitted external detection: %v %s", err, rootCanonical)
	}
	if root.Spec.PlanDigest == baselineRoot.Spec.PlanDigest {
		t.Fatal("external detection did not affect containing Root digest")
	}

	changed := plan
	changed.ExternalDetection = &applicationv1.ExternalDetectionSpec{Detector: "other"}
	refreshDependencyPlanDigestForTest("42", &changed)
	if changed.PlanDigest == plan.PlanDigest {
		t.Fatal("changing external detector did not change child digest")
	}
}

func TestExternalDetectionValidationScope(t *testing.T) {
	dependency := externalDependencyApplication(t)
	if err := ValidatePlan(dependency, ValidationConfig{ClusterID: "42"}); err != nil {
		t.Fatalf("valid external Dependency rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplication)
		reason string
	}{
		{"unsupported detector", func(app *applicationv1.OneKSApplication) { app.Spec.ExternalDetection.Detector = "other" }, "InvalidExternalDetector"},
		{"Root role", func(app *applicationv1.OneKSApplication) {
			app.Spec.Role = applicationv1.ApplicationRoleRoot
			app.Name = "root"
		}, "InvalidExternalDetection"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := dependency.DeepCopy()
			test.mutate(app)
			refreshPlanDigest(app)
			app.Labels = producerLabels(app)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: "42"}); err == nil || err.Reason != test.reason {
				t.Fatalf("validation error = %#v, want %s", err, test.reason)
			}
		})
	}
}

func TestUsableExternalSelectionIsRestartStableAndNeverCreatesHelm(t *testing.T) {
	ctx := context.Background()
	app := externalDependencyApplication(t)
	objects := append([]client.Object{app}, usableCertManagerObjects()...)
	reconciler, recorder := externalTestReconciler(t, objects...)

	reconcileExternalOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if !containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) || stored.Annotations[ExternalSelectionAnnotation] != "" {
		t.Fatalf("selection was persisted before cleanup finalizer: finalizers=%#v annotations=%#v", stored.Finalizers, stored.Annotations)
	}
	reconcileExternalOnce(t, ctx, reconciler, app)
	stored = getApplication(t, ctx, reconciler.Client, app)
	if got := stored.Annotations[ExternalSelectionAnnotation]; got != ExternalSelectionExternal {
		t.Fatalf("selection = %q, want External", got)
	}
	reconcileExternalOnce(t, ctx, reconciler, app)
	stored = getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.Phase != applicationv1.PhaseReady || stored.Status.HelmChartRef != nil ||
		stored.Status.Progress.Completed != 1 || stored.Status.Progress.Total != 1 {
		t.Fatalf("external dependency status = %#v", stored.Status)
	}
	assertExternalCondition(t, stored, ConditionPlanValid, metav1.ConditionTrue, "")
	assertExternalCondition(t, stored, ConditionDependenciesReady, metav1.ConditionTrue, "")
	assertExternalCondition(t, stored, ConditionResourcesReady, metav1.ConditionTrue, "")
	assertExternalCondition(t, stored, ConditionHelmReleaseReady, metav1.ConditionTrue, "ExternalDependencyReady")
	assertExternalCondition(t, stored, ConditionReady, metav1.ConditionTrue, "")
	assertNotFound(t, ctx, reconciler.Client, helmChartObject(app.Spec.Release.ReleaseName))
	if containsWrite(recorder.childWrites, "HelmChart") {
		t.Fatalf("External lifecycle wrote HelmChart: %#v", recorder.childWrites)
	}

	// A fresh reconciler adopts the persisted selection and still creates no HelmChart.
	restarted, restartedRecorder := externalTestReconciler(t, append([]client.Object{stored}, usableCertManagerObjects()...)...)
	reconcileExternalOnce(t, ctx, restarted, stored)
	if got := getApplication(t, ctx, restarted.Client, stored).Annotations[ExternalSelectionAnnotation]; got != ExternalSelectionExternal {
		t.Fatalf("restart changed External selection to %q", got)
	}
	if containsWrite(restartedRecorder.childWrites, "HelmChart") {
		t.Fatalf("restart wrote HelmChart: %#v", restartedRecorder.childWrites)
	}
}

func TestAbsentExternalSelectsManagedFallbackAndStaysManaged(t *testing.T) {
	ctx := context.Background()
	app := externalDependencyApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, _ := externalTestReconciler(t, app)
	reconcileExternalOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if got := stored.Annotations[ExternalSelectionAnnotation]; got != ExternalSelectionManaged {
		t.Fatalf("selection = %q, want Managed", got)
	}

	// A fresh reconciler must ignore newly usable external evidence and continue
	// the normal managed lifecycle selected before the restart.
	restartedObjects := append([]client.Object{stored}, usableCertManagerObjects()...)
	restarted, restartedRecorder := externalTestReconciler(t, restartedObjects...)
	reconcileExternalOnce(t, ctx, restarted, stored)
	restartedStored := getApplication(t, ctx, restarted.Client, stored)
	if got := restartedStored.Annotations[ExternalSelectionAnnotation]; got != ExternalSelectionManaged {
		t.Fatalf("Managed selection switched to %q", got)
	}
	assertExists(t, ctx, restarted.Client, helmChartObject(app.Spec.Release.ReleaseName), HelmChartNamespace, app.Spec.Release.ReleaseName)
	if !containsWrite(restartedRecorder.childWrites, "create:HelmChart") {
		t.Fatalf("restarted Managed lifecycle did not create HelmChart: %#v", restartedRecorder.childWrites)
	}
}

func TestCorruptPersistedExternalSelectionFailsClosed(t *testing.T) {
	ctx := context.Background()
	app := externalDependencyApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Annotations = map[string]string{ExternalSelectionAnnotation: "Corrupt"}
	reconciler, recorder := externalTestReconciler(t, app)

	reconcileExternalOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "ExternalSelectionInvalid" {
		t.Fatalf("corrupt selection status = %#v", stored.Status)
	}
	assertExternalCondition(t, stored, ConditionPlanValid, metav1.ConditionTrue, "ExternalSelectionInvalid")
	assertExternalCondition(t, stored, ConditionReady, metav1.ConditionFalse, "ExternalSelectionInvalid")
	if got := stored.Annotations[ExternalSelectionAnnotation]; got != "Corrupt" {
		t.Fatalf("corrupt selection was rewritten to %q", got)
	}
	if containsWrite(recorder.childWrites, "HelmChart") {
		t.Fatalf("corrupt selection wrote HelmChart: %#v", recorder.childWrites)
	}
}

func TestPresentButUnusableExternalFailsWithoutOwnedEffects(t *testing.T) {
	ctx := context.Background()
	app := externalDependencyApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	crd := establishedCRD(certManagerCRDNames[0])
	reconciler, recorder := externalTestReconciler(t, app, crd)
	result := reconcileExternalOnce(t, ctx, reconciler, app)
	if result.RequeueAfter == 0 {
		t.Fatal("present-but-unusable detection did not request retry")
	}
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "ExternalDependencyUnusable" {
		t.Fatalf("unexpected status: %#v", stored.Status)
	}
	if stored.Annotations[ExternalSelectionAnnotation] != "" || containsWrite(recorder.childWrites, "HelmChart") {
		t.Fatalf("unusable external prerequisite selected lifecycle or wrote Helm: annotations=%#v writes=%#v", stored.Annotations, recorder.childWrites)
	}
}

func TestExternalDetectionAPIFailureIsRetryableWithoutOwnedEffects(t *testing.T) {
	ctx := context.Background()
	app := externalDependencyApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, recorder := externalTestReconciler(t, app)
	readErr := errors.New("simulated detector API failure")
	reconciler.APIReader = &externalGetErrorReader{Reader: reconciler.Client, err: readErr}
	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if !errors.Is(err, readErr) {
		t.Fatalf("reconcile error = %v, want wrapped detector API failure", err)
	}
	if containsWrite(recorder.childWrites, "HelmChart") {
		t.Fatalf("API failure wrote HelmChart: %#v", recorder.childWrites)
	}
}

func TestExternallySelectedPrerequisiteLossNeverFallsBack(t *testing.T) {
	ctx := context.Background()
	app := externalDependencyApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Annotations = map[string]string{ExternalSelectionAnnotation: ExternalSelectionExternal}
	reconciler, recorder := externalTestReconciler(t, app)
	reconcileExternalOnce(t, ctx, reconciler, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "ExternalDependencyLost" || stored.Status.Phase != applicationv1.PhaseFailed {
		t.Fatalf("external loss status = %#v", stored.Status)
	}
	assertExternalCondition(t, stored, ConditionPlanValid, metav1.ConditionTrue, "")
	assertExternalCondition(t, stored, ConditionHelmReleaseReady, metav1.ConditionFalse, "ExternalDependencyLost")
	assertExternalCondition(t, stored, ConditionReady, metav1.ConditionFalse, "ExternalDependencyLost")
	if got := stored.Annotations[ExternalSelectionAnnotation]; got != ExternalSelectionExternal {
		t.Fatalf("external loss changed selection to %q", got)
	}
	if containsWrite(recorder.childWrites, "HelmChart") {
		t.Fatalf("external loss installed fallback HelmChart: %#v", recorder.childWrites)
	}
}

func TestExternalDependencyDeletionNeverTouchesExternalInstallation(t *testing.T) {
	ctx := context.Background()
	app := externalDependencyApplication(t)
	now := metav1.Now()
	app.DeletionTimestamp = &now
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Annotations = map[string]string{ExternalSelectionAnnotation: ExternalSelectionExternal}
	objects := append([]client.Object{app}, usableCertManagerObjects()...)
	reconciler, recorder := externalTestReconciler(t, objects...)
	reconcileExternalOnce(t, ctx, reconciler, app)
	if containsWrite(recorder.childWrites, "HelmChart") || containsWrite(recorder.childWrites, "Deployment") || containsWrite(recorder.childWrites, "Pod") {
		t.Fatalf("external deletion touched external installation: %#v", recorder.childWrites)
	}
	for _, crdName := range certManagerCRDNames {
		if err := reconciler.Get(ctx, types.NamespacedName{Name: crdName}, &apiextensionsv1.CustomResourceDefinition{}); err != nil {
			t.Fatalf("external CRD %s was touched: %v", crdName, err)
		}
	}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: certManagerNamespace}, &corev1.Namespace{}); err != nil {
		t.Fatalf("external namespace was touched: %v", err)
	}
	for _, component := range []string{"controller", "webhook"} {
		key := types.NamespacedName{Namespace: certManagerNamespace, Name: "cert-manager-" + component}
		if err := reconciler.Get(ctx, key, &appsv1.Deployment{}); err != nil {
			t.Fatalf("external %s Deployment was touched: %v", component, err)
		}
		if err := reconciler.Get(ctx, key, &corev1.Pod{}); err != nil {
			t.Fatalf("external %s Pod was touched: %v", component, err)
		}
	}
	webhookKey := types.NamespacedName{Namespace: certManagerNamespace, Name: certManagerWebhookName}
	if err := reconciler.Get(ctx, webhookKey, &corev1.Service{}); err != nil {
		t.Fatalf("external webhook Service was touched: %v", err)
	}
	if err := reconciler.Get(ctx, webhookKey, &corev1.Endpoints{}); err != nil {
		t.Fatalf("external webhook Endpoints were touched: %v", err)
	}
}

func TestCertManagerDetectorUsesOnlyScopedKubernetesReads(t *testing.T) {
	ctx := context.Background()
	app := externalDependencyApplication(t)
	reconciler, _ := externalTestReconciler(t, append([]client.Object{app}, usableCertManagerObjects()...)...)
	spy := &externalReadSpy{Reader: reconciler.Client}
	reconciler.APIReader = spy
	result, err := reconciler.detectExternalDependency(ctx, app)
	if err != nil || result.state != externalDetectionUsable {
		t.Fatalf("detect cert-manager = %#v, %v", result, err)
	}
	if spy.deploymentNamespace != certManagerNamespace || spy.podNamespace != certManagerNamespace ||
		spy.deploymentSelector != certManagerComponentSelector || spy.podSelector != certManagerComponentSelector {
		t.Fatalf("detector list scope = %#v", spy)
	}
	if !reflect.DeepEqual(spy.crdNames, certManagerCRDNames) || spy.serviceKey != (types.NamespacedName{Namespace: certManagerNamespace, Name: certManagerWebhookName}) || spy.endpointsKey != spy.serviceKey {
		t.Fatalf("detector exact GET identities = %#v", spy)
	}

	source, err := os.ReadFile("external_detection.go")
	if err != nil {
		t.Fatalf("read detector source: %v", err)
	}
	for _, forbidden := range []string{"os/exec", "kubectl", "vm_exec", "VM.exec", "OneKS::"} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("detector contains forbidden execution path %q", forbidden)
		}
	}
}

func TestCertManagerNamespaceAloneIsAbsent(t *testing.T) {
	ctx := context.Background()
	app := externalDependencyApplication(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: certManagerNamespace}}
	reconciler, _ := externalTestReconciler(t, app, namespace)

	result, err := reconciler.detectExternalDependency(ctx, app)
	if err != nil {
		t.Fatalf("detect namespace-only cert-manager: %v", err)
	}
	if result.state != externalDetectionAbsent {
		t.Fatalf("namespace-only detector state = %q, want %q: %s", result.state, externalDetectionAbsent, result.message)
	}
}

func externalDependencyApplication(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := validDependencyPlanApplication(t)
	app.Spec.CatalogueChartID = "cert-manager"
	app.Spec.Release.ChartID = "cert-manager"
	app.Spec.Release.Chart = "cert-manager"
	app.Spec.Release.ReleaseName = "cert-manager"
	app.Spec.Release.TargetNamespace = certManagerNamespace
	app.Spec.ExternalDetection = &applicationv1.ExternalDetectionSpec{Detector: applicationv1.ExternalDetectorCertManager}
	app.Name = dependencyApplicationName(app.Spec.Release.ReleaseName)
	app.UID = types.UID("uid-cert-manager")
	refreshPlanDigest(app)
	app.Labels = producerLabels(app)
	return app
}

func externalTestReconciler(t *testing.T, objects ...client.Object) (*Reconciler, *recordingClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	for name, add := range map[string]func(*runtime.Scheme) error{
		"core": corev1.AddToScheme, "apps": appsv1.AddToScheme,
		"apiextensions": apiextensionsv1.AddToScheme, "application": applicationv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add %s scheme: %v", name, err)
		}
	}
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&applicationv1.OneKSApplication{}).WithObjects(objects...).Build()
	recorder := &recordingClient{Client: base}
	return &Reconciler{Client: recorder, Scheme: scheme, ClusterID: "42", ControllerVersion: "test", RequeueAfter: time.Millisecond}, recorder
}

func usableCertManagerObjects() []client.Object {
	objects := make([]client.Object, 0, 11)
	for _, name := range certManagerCRDNames {
		objects = append(objects, establishedCRD(name))
	}
	objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: certManagerNamespace}})
	for _, component := range []string{"controller", "webhook"} {
		replicas := int32(1)
		labels := map[string]string{"app.kubernetes.io/component": component}
		objects = append(objects,
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: certManagerNamespace, Name: "cert-manager-" + component, Labels: labels}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{AvailableReplicas: 1}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: certManagerNamespace, Name: "cert-manager-" + component, Labels: labels}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
		)
	}
	objects = append(objects,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: certManagerNamespace, Name: certManagerWebhookName}},
		&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Namespace: certManagerNamespace, Name: certManagerWebhookName}, Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "192.0.2.1"}}}}},
	)
	return objects
}

func establishedCRD(name string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}}}
}

func reconcileExternalOnce(t *testing.T, ctx context.Context, reconciler *Reconciler, app *applicationv1.OneKSApplication) ctrl.Result {
	t.Helper()
	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if err != nil {
		t.Fatalf("reconcile external dependency: %v", err)
	}
	return result
}

func assertExternalCondition(t *testing.T, app *applicationv1.OneKSApplication, conditionType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := conditionByType(app.Status.Conditions, conditionType)
	if condition == nil || condition.Status != status || reason != "" && condition.Reason != reason {
		t.Fatalf("condition %s = %#v, want status %s reason %q", conditionType, condition, status, reason)
	}
}

type externalGetErrorReader struct {
	client.Reader
	err error
}

func (r *externalGetErrorReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*apiextensionsv1.CustomResourceDefinition); ok {
		return r.err
	}
	return r.Reader.Get(ctx, key, object, options...)
}

type externalReadSpy struct {
	client.Reader
	crdNames                                []string
	serviceKey, endpointsKey                types.NamespacedName
	deploymentNamespace, deploymentSelector string
	podNamespace, podSelector               string
}

func (s *externalReadSpy) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	switch object.(type) {
	case *apiextensionsv1.CustomResourceDefinition:
		s.crdNames = append(s.crdNames, key.Name)
	case *corev1.Service:
		s.serviceKey = key
	case *corev1.Endpoints:
		s.endpointsKey = key
	}
	return s.Reader.Get(ctx, key, object, options...)
}

func (s *externalReadSpy) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	listOptions := &client.ListOptions{}
	for _, option := range options {
		option.ApplyToList(listOptions)
	}
	switch list.(type) {
	case *appsv1.DeploymentList:
		s.deploymentNamespace = listOptions.Namespace
		if listOptions.LabelSelector != nil {
			s.deploymentSelector = listOptions.LabelSelector.String()
		}
	case *corev1.PodList:
		s.podNamespace = listOptions.Namespace
		if listOptions.LabelSelector != nil {
			s.podSelector = listOptions.LabelSelector.String()
		}
	}
	return s.Reader.List(ctx, list, options...)
}
