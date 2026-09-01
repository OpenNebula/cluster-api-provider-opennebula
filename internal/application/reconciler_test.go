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

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestExecuteCreatesManagedResourceBeforeHelmChartAndBecomesReady(t *testing.T) {
	var logs strings.Builder
	logger := funcr.New(func(prefix, args string) {
		logs.WriteString(prefix)
		logs.WriteByte(' ')
		logs.WriteString(args)
		logs.WriteByte('\n')
	}, funcr.Options{})
	ctx := ctrllog.IntoContext(context.Background(), logger)
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, recorder := testReconciler(t, app)

	reconcileOnce(t, ctx, reconciler, app)
	assertExists(t, ctx, reconciler.Client, &corev1.ConfigMap{}, "catalogue-workloads", app.Spec.ManagedResources[0].Name)
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
	for _, stage := range []string{
		"application reconciliation started",
		"managed resource created",
		"Helm release created",
		"application status updated",
		`"phase"="Ready"`,
		"application reconciliation completed",
		`"releaseName"="oneks-prometheus"`,
		`"targetNamespace"="catalogue-workloads"`,
	} {
		if !strings.Contains(logs.String(), stage) {
			t.Fatalf("successful reconciliation logs omit %q:\n%s", stage, logs.String())
		}
	}
	for _, sensitive := range []string{"password", "kubeconfig", "authorization"} {
		if strings.Contains(strings.ToLower(logs.String()), sensitive) {
			t.Fatalf("successful reconciliation logs contain sensitive term %q", sensitive)
		}
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

func TestTargetNamespaceAPIErrorIsRetriedWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, recorder := testReconciler(t, app)
	reconciler.Client = &namespaceErrorClient{
		Client: reconciler.Client,
		err: apierrors.NewForbidden(
			schema.GroupResource{Resource: "namespaces"}, "catalogue-workloads",
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
	if _, ok := object.(*corev1.Namespace); ok && key.Name == "catalogue-workloads" {
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
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apiextensions scheme: %v", err)
	}
	if err := applicationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add application scheme: %v", err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&applicationv1.OneKSApplication{}).
		WithObjects(append(objects, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "catalogue-workloads"},
		})...).Build()
	recorder := &recordingClient{Client: base}
	return &Reconciler{
		Client: recorder, ClusterID: "42",
		RequeueAfter: time.Millisecond,
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
