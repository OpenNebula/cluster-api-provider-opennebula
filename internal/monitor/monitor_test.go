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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

type recordingSender struct {
	reports  []Report
	payloads []CallbackPayload
	err      error
}

func (s *recordingSender) Send(_ context.Context, payload CallbackPayload) error {
	s.payloads = append(s.payloads, payload)
	if report, ok := payload.(Report); ok {
		s.reports = append(s.reports, report)
	}
	return s.err
}

type senderFunc func(context.Context, CallbackPayload) error

func (f senderFunc) Send(ctx context.Context, report CallbackPayload) error { return f(ctx, report) }

func TestEnqueueKeepsLatestReportForExistingKey(t *testing.T) {
	queue := newReportQueue(&recordingSender{})
	defer queue.queue.ShutDown()

	queue.Add("Node/worker-1", Report{Kind: "Node", Name: "worker-1", Event: "Added"})
	queue.Add("Node/worker-1", Report{Kind: "Node", Name: "worker-1", Event: "Updated"})

	if len(queue.pending) != 1 {
		t.Fatalf("expected one pending key, got %d", len(queue.pending))
	}
	if got := queue.pending["Node/worker-1"].value.(Report).Event; got != "Updated" {
		t.Fatalf("expected latest event, got %q", got)
	}
}

type testCallback struct {
	Kind  string `json:"kind"`
	Value int    `json:"value"`
}

func (callback testCallback) CallbackKind() string { return callback.Kind }

func TestEnqueueCoalescesLatestGenericCallback(t *testing.T) {
	queue := newReportQueue(&recordingSender{})
	defer queue.queue.ShutDown()
	queue.Add("resource-value/stable", testCallback{Kind: "ResourceValue", Value: 1})
	queue.Add("resource-value/stable", testCallback{Kind: "ResourceValue", Value: 2})
	if got := queue.pending["resource-value/stable"].value.(testCallback).Value; got != 2 {
		t.Fatalf("coalesced value = %d, want 2", got)
	}
}

func TestFailedCallbackRemainsPendingAndIsRateLimited(t *testing.T) {
	sender := &recordingSender{err: errors.New("endpoint unavailable")}
	queue := newReportQueue(sender)
	defer queue.queue.ShutDown()
	queue.Add("Node/worker-1", Report{Kind: "Node", Name: "worker-1", Event: "Updated"})

	if !queue.processNext(context.Background()) {
		t.Fatal("worker stopped after retryable callback failure")
	}
	if len(sender.reports) != 1 || len(queue.pending) != 1 {
		t.Fatalf("failed callback was not retained: reports=%d pending=%d", len(sender.reports), len(queue.pending))
	}
	if got := queue.queue.NumRequeues("Node/worker-1"); got != 1 {
		t.Fatalf("expected one rate-limited retry, got %d", got)
	}
}

func TestReportAddedDuringSendRemainsPending(t *testing.T) {
	var queue *reportQueue
	sent := make([]Report, 0, 2)
	queue = newReportQueue(senderFunc(func(_ context.Context, payload CallbackPayload) error {
		report := payload.(Report)
		sent = append(sent, report)
		if report.Event == "Added" {
			queue.Add("Node/worker-1", Report{Kind: "Node", Name: "worker-1", Event: "Updated"})
		}
		return nil
	}))
	defer queue.queue.ShutDown()
	queue.Add("Node/worker-1", Report{Kind: "Node", Name: "worker-1", Event: "Added"})

	if !queue.processNext(context.Background()) {
		t.Fatal("worker stopped after successful send")
	}
	if pending := queue.pending["Node/worker-1"]; pending == nil || pending.value.(Report).Event != "Updated" {
		t.Fatalf("new report was not retained: %#v", pending)
	}

	if !queue.processNext(context.Background()) {
		t.Fatal("worker stopped before sending replacement report")
	}
	if len(queue.pending) != 0 {
		t.Fatalf("sent replacement remained pending: %#v", queue.pending)
	}
	if len(sent) != 2 || sent[0].Event != "Added" || sent[1].Event != "Updated" {
		t.Fatalf("unexpected reports sent: %#v", sent)
	}
}

func TestPendingCallbackIdentitiesAreGloballyBounded(t *testing.T) {
	queue := newReportQueue(&recordingSender{})
	defer queue.queue.ShutDown()
	for index := 0; index < maxPendingReports; index++ {
		key := fmt.Sprintf("Node/worker-%d", index)
		report := Report{Kind: "Node", Name: key}
		queue.pending[key] = &queuedPayload{value: report}
	}
	if queue.Add("Node/overflow", Report{Kind: "Node", Name: "overflow"}) {
		t.Fatal("new callback identity exceeded the global bound")
	}
	if len(queue.pending) != maxPendingReports {
		t.Fatalf("callback bound changed after rejection: %d", len(queue.pending))
	}
	if !queue.Add("Node/worker-0", Report{Kind: "Node", Name: "worker-0", Event: "Updated"}) {
		t.Fatal("latest state for an existing callback identity was rejected")
	}
	if len(queue.pending) != maxPendingReports {
		t.Fatalf("existing-key update changed callback identity count: %d", len(queue.pending))
	}
}

func TestInitialNodeSynchronizationQueuesOneReportPerNode(t *testing.T) {
	nodes := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Node{}, 0, cache.Indexers{})
	queue := newReportQueue(&recordingSender{})
	defer queue.queue.ShutDown()
	m := &Monitor{nodes: nodes, reports: queue}
	for _, node := range []*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}, Spec: corev1.NodeSpec{ProviderID: "one://2"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Spec: corev1.NodeSpec{ProviderID: "one://1"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
	} {
		if err := nodes.GetStore().Add(node); err != nil {
			t.Fatalf("add node: %v", err)
		}
	}

	m.enqueueInitialNodes()
	if len(queue.pending) != 2 {
		t.Fatalf("expected one report per Node, got %#v", queue.pending)
	}
	if queue.pending["Node/node-1"].value.(Report).Status["ready"] != false ||
		queue.pending["Node/node-2"].value.(Report).Status["ready"] != true {
		t.Fatalf("initial reports do not contain independent readiness: %#v", queue.pending)
	}
	for _, queued := range queue.pending {
		report := queued.value.(Report)
		if _, exists := report.Status["readyProviderIDs"]; exists {
			t.Fatalf("initial report contains readiness snapshot: %#v", report.Status)
		}
	}
}

func TestNodeReportWaitsForProviderID(t *testing.T) {
	queue := newReportQueue(&recordingSender{})
	defer queue.queue.ShutDown()
	m := &Monitor{reports: queue}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}

	m.onNode(node, "Added")
	if len(queue.pending) != 0 {
		t.Fatalf("node without provider ID was queued: %#v", queue.pending)
	}

	node.Spec.ProviderID = "one://317"
	m.onNode(node, "Updated")
	queued := queue.pending["Node/worker-1"]
	if queued == nil {
		t.Fatal("node was not queued after provider ID was assigned")
	}
	report := queued.value.(Report)
	if got := report.Status["providerID"]; got != "one://317" {
		t.Fatalf("provider ID = %#v, want %q", got, "one://317")
	}
}
