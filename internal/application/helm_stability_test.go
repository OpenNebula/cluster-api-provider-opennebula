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
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDependencyPendingReleaseDoesNotPublishTransientHelmFailure(t *testing.T) {
	plan := dependencyPlanForTest("dependency-pending-release", "dependency-chart", nil)
	root := deletingRootForTest(t, "deleting-root", plan)
	dependency := existingDependencyForTest(root, plan)
	r, _ := testReconciler(t, root, dependency)

	_, err := r.recordObservedStatus(context.Background(), dependency,
		dependencyObservation{ready: true, reason: "NoDependencies", message: "Application has no direct dependencies"},
		observation{
			managed: componentObservation{ready: true}, allResources: true,
			helmState: componentObservation{failed: true, reason: "InstallerJobFailed", message: "temporary Helm error"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	stored := getApplication(t, context.Background(), r.Client, dependency)
	if stored.Status.Phase != applicationv1.PhaseDeleting || stored.Status.LastError != nil {
		t.Fatalf("pending-release dependency status = %#v", stored.Status)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, ConditionReady)
	if ready == nil || ready.Reason != "DependencyReleasePending" {
		t.Fatalf("pending-release Ready condition = %#v", ready)
	}
}

func TestLiveConsumerKeepsDependencyFailureVisible(t *testing.T) {
	plan := dependencyPlanForTest("shared-live-dependency", "dependency-chart", nil)
	deletingRoot := deletingRootForTest(t, "deleting-root", plan)
	liveRoot := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	liveRoot.Name = "live-root"
	liveRoot.UID = types.UID("live-root-uid")
	dependency := existingDependencyForTest(deletingRoot, plan)
	r, _ := testReconciler(t, deletingRoot, liveRoot, dependency)

	_, err := r.recordObservedStatus(context.Background(), dependency,
		dependencyObservation{ready: true, reason: "NoDependencies", message: "Application has no direct dependencies"},
		observation{
			managed: componentObservation{ready: true}, allResources: true,
			helmState: componentObservation{failed: true, reason: "InstallerJobFailed", message: "persistent Helm error"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	stored := getApplication(t, context.Background(), r.Client, dependency)
	if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.LastError == nil ||
		stored.Status.LastError.Reason != "InstallerJobFailed" {
		t.Fatalf("shared dependency failure was hidden: %#v", stored.Status)
	}
}

func TestRetainedDependencyStructuralFailureDuringConsumerDeletionRemainsVisible(t *testing.T) {
	plan := dependencyPlanForTest("retained-failing-dependency", "dependency-chart", nil)
	plan.DeletionPolicy = applicationv1.DeletionPolicyRetain
	root := deletingRootForTest(t, "deleting-root", plan)
	dependency := existingDependencyForTest(root, plan)
	dependency.Status.Phase = applicationv1.PhaseReady
	r, _ := testReconciler(t, root, dependency)

	_, err := r.recordObservedStatus(context.Background(), dependency,
		dependencyObservation{ready: true, reason: "NoDependencies", message: "Application has no direct dependencies"},
		observation{
			managed: componentObservation{ready: true}, allResources: true,
			helmState: componentObservation{failed: true, reason: "InstallerJobFailed", message: "real Helm failure"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	stored := getApplication(t, context.Background(), r.Client, dependency)
	if stored.Status.Phase != applicationv1.PhaseFailed || stored.Status.LastError == nil ||
		stored.Status.LastError.Reason != "InstallerJobFailed" {
		t.Fatalf("retained dependency structural failure was hidden: %#v", stored.Status)
	}
}

func TestRetainedDependencyIsNotMarkedPendingRelease(t *testing.T) {
	plan := dependencyPlanForTest("retained-dependency", "dependency-chart", nil)
	plan.DeletionPolicy = applicationv1.DeletionPolicyRetain
	root := deletingRootForTest(t, "deleting-root", plan)
	dependency := existingDependencyForTest(root, plan)
	r, _ := testReconciler(t, root, dependency)

	pending, err := r.dependencyReleasePending(context.Background(), dependency)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("retained dependency was marked for release")
	}
}

func TestAuthoritativeHelmReadRejectsStaleCachedFailure(t *testing.T) {
	app := withApplicationFinalizer(goldenApplication(t))
	app.Status.Phase = applicationv1.PhaseReady
	chart := desiredHelmChart(app)
	chart.SetUID(types.UID("chart-uid"))
	chart.Object["status"] = map[string]any{"jobName": "helm-install-authoritative"}
	stale := failedInstallerJob("helm-install-authoritative", "stale failure")
	completed := completedJob("helm-install-authoritative")
	r, _ := testReconciler(t, app, chart, stale)
	r.APIReader = authoritativeClient(r, chart.DeepCopy(), completed)

	observed, err := r.observeHelm(context.Background(), app, observation{})
	if err != nil {
		t.Fatal(err)
	}
	if observed.helmState.failed || !observed.helmState.ready || observed.helmState.reason != "InstallerJobComplete" {
		t.Fatalf("authoritative completed Job did not override stale cache: %#v", observed.helmState)
	}
}

func TestJobEventsMapOnlyToOwningApplication(t *testing.T) {
	app := withApplicationFinalizer(goldenApplication(t))
	chart := desiredHelmChart(app)
	chart.Object["status"] = map[string]any{"jobName": "helm-install-owned"}
	r, _ := testReconciler(t, app, chart)

	installer := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: HelmChartNamespace, Name: "helm-install-owned"}}
	requests := r.requestsForJob(context.Background(), installer)
	assertSingleJobRequest(t, requests, app)

	cleanup := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: HelmChartNamespace, Name: "cleanup-owned", Labels: ownershipLabels(app),
	}}
	requests = r.requestsForJob(context.Background(), cleanup)
	assertSingleJobRequest(t, requests, app)

	unrelated := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: HelmChartNamespace, Name: "unrelated"}}
	if requests := r.requestsForJob(context.Background(), unrelated); len(requests) != 0 {
		t.Fatalf("unrelated Job enqueued applications: %#v", requests)
	}
}

func failedInstallerJob(name, message string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: HelmChartNamespace, Name: name, UID: types.UID(name + "-uid")},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: message,
		}}},
	}
}

func assertSingleJobRequest(t *testing.T, requests []ctrl.Request, app *applicationv1.OneKSApplication) {
	t.Helper()
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(app) {
		t.Fatalf("Job requests = %#v, want only %s/%s", requests, app.Namespace, app.Name)
	}
}
