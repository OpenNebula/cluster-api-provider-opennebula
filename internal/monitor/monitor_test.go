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

package monitor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/monitoring"
)

type recordingSender struct {
	reports []CallbackPayload
	err     error
}

func (s *recordingSender) Send(_ context.Context, report CallbackPayload) error {
	s.reports = append(s.reports, report)
	return s.err
}

func TestEnqueueKeepsLatestReportForExistingKey(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[string](),
	)
	defer queue.ShutDown()
	m := &Monitor{queue: queue, pending: map[string]pendingReport{}}

	m.enqueue("Node/worker-1", Report{Kind: "Node", Name: "worker-1", Event: "Added"})
	m.enqueue("Node/worker-1", Report{Kind: "Node", Name: "worker-1", Event: "Updated"})

	if len(m.pending) != 1 {
		t.Fatalf("expected one pending key, got %d", len(m.pending))
	}
	latest, ok := m.pending["Node/worker-1"].report.(Report)
	if !ok {
		t.Fatalf("pending callback changed type: %T", m.pending["Node/worker-1"].report)
	}
	if got := latest.Event; got != "Updated" {
		t.Fatalf("expected latest event, got %q", got)
	}
}

func TestFailedCallbackRemainsPendingAndIsRateLimited(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[string](),
	)
	defer queue.ShutDown()
	sender := &recordingSender{err: errors.New("endpoint unavailable")}
	m := &Monitor{sender: sender, queue: queue, pending: map[string]pendingReport{}}
	m.enqueue("Node/worker-1", Report{Kind: "Node", Name: "worker-1", Event: "Updated"})

	if !m.processNext(context.Background()) {
		t.Fatal("worker stopped after retryable callback failure")
	}
	if len(sender.reports) != 1 || len(m.pending) != 1 {
		t.Fatalf("failed callback was not retained: reports=%d pending=%d", len(sender.reports), len(m.pending))
	}
	if got := queue.NumRequeues("Node/worker-1"); got != 1 {
		t.Fatalf("expected one rate-limited retry, got %d", got)
	}
}

func TestClusterSignalQueueKeepsLatestTransition(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[string](),
	)
	defer queue.ShutDown()
	m := &Monitor{queue: queue, pending: map[string]pendingReport{}}
	signal := monitoring.ClusterSignal{
		APIVersion: monitoring.APIVersion, Kind: monitoring.SignalKind,
		ClusterID: "42", Identity: "signal-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG",
		Profile: "health", Rule: "availability", Source: "prometheus",
		Category: "monitoring", Severity: "warning", Status: "active",
		ObservedAt: "2026-08-06T12:30:00Z", Value: 0.5, Unit: "ratio",
		Threshold: 0.9, Labels: map[string]string{}, Message: "warning",
	}
	m.enqueue("ClusterSignal/"+signal.Identity, signal)
	signal.Severity = "critical"
	signal.Threshold = 0.5
	signal.Message = "critical"
	m.enqueue("ClusterSignal/"+signal.Identity, signal)

	pending, ok := m.pending["ClusterSignal/"+signal.Identity].report.(monitoring.ClusterSignal)
	if !ok || pending.Severity != "critical" || len(m.pending) != 1 {
		t.Fatalf("latest signal transition was not retained: %#v", m.pending)
	}
}

func TestPendingCallbackIdentitiesAreGloballyBounded(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[string](),
	)
	defer queue.ShutDown()
	pending := make(map[string]pendingReport, MaxPendingCallbackIdentities)
	for index := 0; index < MaxPendingCallbackIdentities; index++ {
		key := fmt.Sprintf("Node/worker-%d", index)
		pending[key] = pendingReport{report: Report{Kind: "Node", Name: key}}
	}
	m := &Monitor{queue: queue, pending: pending}
	if m.enqueue("Node/overflow", Report{Kind: "Node", Name: "overflow"}) {
		t.Fatal("new callback identity exceeded the global bound")
	}
	if len(m.pending) != MaxPendingCallbackIdentities {
		t.Fatalf("callback bound changed after rejection: %d", len(m.pending))
	}
	if !m.enqueue("Node/worker-0", Report{Kind: "Node", Name: "worker-0", Event: "Updated"}) {
		t.Fatal("latest state for an existing callback identity was rejected")
	}
	if len(m.pending) != MaxPendingCallbackIdentities {
		t.Fatalf("existing-key update changed callback identity count: %d", len(m.pending))
	}
}

func TestChartStatusFromCompletedJob(t *testing.T) {
	jobs := jobInformer()
	m := &Monitor{jobs: jobs}
	addJob(t, jobs, batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "helm-install-cni", Namespace: "kube-system", ResourceVersion: "20"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	})

	status, resourceVersion := m.chartStatus(chartObject())
	if status != "deployed" || resourceVersion != "20" {
		t.Fatalf("expected deployed at rv 20, got %s at rv %s", status, resourceVersion)
	}
}

func TestChartStatusFromActiveJob(t *testing.T) {
	jobs := jobInformer()
	m := &Monitor{jobs: jobs}
	addJob(t, jobs, batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "helm-install-cni", Namespace: "kube-system", ResourceVersion: "21"},
		Status:     batchv1.JobStatus{Active: 1},
	})

	status, resourceVersion := m.chartStatus(chartObject())
	if status != "pending" || resourceVersion != "21" {
		t.Fatalf("expected pending at rv 21, got %s at rv %s", status, resourceVersion)
	}
}

func TestChartStatusFromFailedJob(t *testing.T) {
	jobs := jobInformer()
	m := &Monitor{jobs: jobs}
	addJob(t, jobs, batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "helm-install-cni", Namespace: "kube-system", ResourceVersion: "22"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		}}},
	})

	status, resourceVersion := m.chartStatus(chartObject())
	if status != "failed" || resourceVersion != "22" {
		t.Fatalf("expected failed at rv 22, got %s at rv %s", status, resourceVersion)
	}
}

func TestChartFailedConditionTakesPrecedence(t *testing.T) {
	chart := chartObject()
	chart.Object["status"] = map[string]any{
		"jobName": "helm-install-cni",
		"conditions": []any{map[string]any{
			"type": "Failed", "status": "True",
		}},
	}
	m := &Monitor{jobs: jobInformer()}

	status, resourceVersion := m.chartStatus(chart)
	if status != "failed" || resourceVersion != "" {
		t.Fatalf("expected chart condition failure, got %s at rv %s", status, resourceVersion)
	}
}

func TestChartStatusWithoutJob(t *testing.T) {
	m := &Monitor{jobs: jobInformer()}
	status, resourceVersion := m.chartStatus(chartObject())
	if status != "unknown" || resourceVersion != "" {
		t.Fatalf("expected unknown without the referenced job, got %s at rv %s", status, resourceVersion)
	}
}

func TestChartStatusBeforeJobCreation(t *testing.T) {
	chart := chartObject()
	delete(chart.Object, "status")
	m := &Monitor{jobs: jobInformer()}

	status, resourceVersion := m.chartStatus(chart)
	if status != "pending" || resourceVersion != "" {
		t.Fatalf("expected pending before job creation, got %s at rv %s", status, resourceVersion)
	}
}

func TestReadyProviderIDs(t *testing.T) {
	nodes := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Node{}, 0, cache.Indexers{})
	m := &Monitor{nodes: nodes}
	for _, node := range []*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}, Spec: corev1.NodeSpec{ProviderID: "one://2"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Spec: corev1.NodeSpec{ProviderID: "one://1"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-3"}, Spec: corev1.NodeSpec{ProviderID: "one://3"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
	} {
		if err := nodes.GetStore().Add(node); err != nil {
			t.Fatalf("add node: %v", err)
		}
	}

	got := m.readyProviderIDs("one://2")
	if len(got) != 1 || got[0] != "one://1" {
		t.Fatalf("unexpected ready provider IDs: %#v", got)
	}
}

func TestHasActiveChartOperation(t *testing.T) {
	operations := cache.NewSharedIndexInformer(
		&cache.ListWatch{}, &corev1.ConfigMap{}, 0, cache.Indexers{
			cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
		},
	)
	m := &Monitor{config: Config{KubeSystemNS: "kube-system"}, operations: operations}
	if m.hasActiveChartOperation() {
		t.Fatal("empty marker cache must not request startup reconciliation")
	}
	marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: chartOperationMarker, Namespace: "kube-system"}}
	if err := operations.GetStore().Add(marker); err != nil {
		t.Fatalf("add marker: %v", err)
	}
	if !m.hasActiveChartOperation() {
		t.Fatal("chart operation marker must request startup reconciliation")
	}
}

func TestMonitoringProfileLifecycleRetainsLastValidUpdate(t *testing.T) {
	store := monitoring.NewStore([]string{"monitoring"})
	m := &Monitor{profiles: store}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-health", Namespace: "kube-system",
			Labels: map[string]string{monitoring.ProfileLabel: "true"},
		},
		Data: map[string]string{monitoring.ProfileDataKey: monitorProfileDocument()},
	}
	m.onMonitoringProfile(configMap)
	if profiles := store.List(); len(profiles) != 1 || profiles[0].Metadata.Name != "cluster-health" {
		t.Fatalf("valid profile was not loaded: %#v", profiles)
	}

	invalid := configMap.DeepCopy()
	invalid.Data[monitoring.ProfileDataKey] = "not: a profile"
	m.onMonitoringProfile(invalid)
	if profiles := store.List(); len(profiles) != 1 || profiles[0].Metadata.Name != "cluster-health" {
		t.Fatalf("invalid update replaced the last valid profile: %#v", profiles)
	}

	m.onMonitoringProfileDeleted(cache.DeletedFinalStateUnknown{Key: "kube-system/cluster-health", Obj: configMap})
	if profiles := store.List(); len(profiles) != 0 {
		t.Fatalf("deleted profile remains loaded: %#v", profiles)
	}
}

func TestReadinessJobReport(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oneks-readiness-runai", Namespace: "kube-system",
			ResourceVersion: "31",
			Labels:          map[string]string{readinessJobLabel: "true"},
			Annotations: map[string]string{
				defaultChartAnnotation:     "runai-chart",
				readinessReleaseAnnotation: "runai-backend",
			},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	report, watched := readinessJobReport(
		Config{ChartAnnotation: defaultChartAnnotation}, job, "Updated",
	)
	if !watched {
		t.Fatal("labelled readiness Job was not watched")
	}
	if report.Kind != "ReadinessJob" || report.Status["status"] != "complete" {
		t.Fatalf("unexpected readiness report: %#v", report)
	}
	if report.Status["chartId"] != "runai-chart" || report.Status["releaseName"] != "runai-backend" {
		t.Fatalf("missing installation identity: %#v", report.Status)
	}
}

func TestUnlabelledJobIsNotAReadinessJob(t *testing.T) {
	_, watched := readinessJobReport(
		Config{ChartAnnotation: defaultChartAnnotation},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ordinary"}}, "Added",
	)
	if watched {
		t.Fatal("ordinary Job must not emit a readiness report")
	}
}

func jobInformer() cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(&cache.ListWatch{}, &batchv1.Job{}, 0, cache.Indexers{})
}

func addJob(t *testing.T, informer cache.SharedIndexInformer, job batchv1.Job) {
	t.Helper()
	if err := informer.GetStore().Add(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}
}

func monitorProfileDocument() string {
	return `apiVersion: monitoring.oneks.opennebula.io/v1alpha1
kind: MonitoringProfile
metadata:
  name: cluster-health
spec:
  evaluationInterval: 1m
  sources:
  - id: prometheus
    type: prometheus
    service:
      namespace: monitoring
      name: prometheus
      port: http
      path: /api/v1/query
    timeout: 10s
  rules:
  - id: availability
    source: prometheus
    query: up
    unit: ratio
    comparison: LessThan
    warning: 0.9
    critical: 0.5
    recovery: 1
    labels:
      allow: []
    message: "{{rule}} is {{severity}} at {{value}}"`
}
