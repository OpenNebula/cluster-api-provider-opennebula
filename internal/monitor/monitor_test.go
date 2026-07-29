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
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

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
		t.Fatal("empty marker cache must not request periodic reconciliation")
	}
	marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: chartOperationMarker, Namespace: "kube-system"}}
	if err := operations.GetStore().Add(marker); err != nil {
		t.Fatalf("add marker: %v", err)
	}
	if !m.hasActiveChartOperation() {
		t.Fatal("chart operation marker must request periodic reconciliation")
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
