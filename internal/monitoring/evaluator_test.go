/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitoring

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type queryResult struct {
	samples []Sample
	err     error
}

type sequenceQueryer struct {
	results []queryResult
	calls   int
}

func (q *sequenceQueryer) Query(_ context.Context, _ Source, _ string) ([]Sample, error) {
	index := q.calls
	q.calls++
	if index >= len(q.results) {
		index = len(q.results) - 1
	}
	return q.results[index].samples, q.results[index].err
}

func TestEvaluatorEmitsWarningCriticalAndRecoveryTransitions(t *testing.T) {
	profile := parsedTestProfile(t)
	queryer := &sequenceQueryer{results: []queryResult{
		{samples: []Sample{{Value: 81, Labels: map[string]string{"zone": "a"}}}},
		{samples: []Sample{{Value: 82, Labels: map[string]string{"zone": "a"}}}},
		{samples: []Sample{{Value: 91, Labels: map[string]string{"zone": "a"}}}},
		{samples: []Sample{{Value: 76, Labels: map[string]string{"zone": "a"}}}},
		{samples: []Sample{{Value: 74, Labels: map[string]string{"zone": "a"}}}},
	}}
	var signals []ClusterSignal
	evaluator := testEvaluator(profile, queryer, func(signal ClusterSignal) {
		signals = append(signals, signal)
	})

	for range queryer.results {
		evaluator.evaluateProfile(context.Background(), profile, evaluator.now())
	}
	if len(signals) != 4 {
		t.Fatalf("expected warning, critical, warning, resolved transitions, got %#v", signals)
	}
	want := []struct{ status, severity string }{
		{"active", "warning"}, {"active", "critical"},
		{"active", "warning"}, {"resolved", "info"},
	}
	for index, expected := range want {
		if signals[index].Status != expected.status || signals[index].Severity != expected.severity {
			t.Errorf("signal %d: got %s/%s want %s/%s", index, signals[index].Status, signals[index].Severity, expected.status, expected.severity)
		}
	}
}

func TestEvaluatorMarksKnownSignalUnknownAndRecovers(t *testing.T) {
	profile := parsedTestProfile(t)
	queryer := &sequenceQueryer{results: []queryResult{
		{samples: []Sample{{Value: 91, Labels: map[string]string{"zone": "a"}}}},
		{err: errors.New("Prometheus unavailable")},
		{samples: []Sample{{Value: 91, Labels: map[string]string{"zone": "a"}}}},
	}}
	var signals []ClusterSignal
	failures := 0
	evaluator := testEvaluator(profile, queryer, func(signal ClusterSignal) { signals = append(signals, signal) })
	evaluator.hooks.Failure = func(_, _ string, _ error) { failures++ }
	for range queryer.results {
		evaluator.evaluateProfile(context.Background(), profile, evaluator.now())
	}
	if failures != 1 || len(signals) != 3 {
		t.Fatalf("unexpected failures/signals: failures=%d signals=%#v", failures, signals)
	}
	if signals[1].Status != "unknown" || signals[2].Status != "active" || signals[2].Severity != "critical" {
		t.Fatalf("unexpected recovery sequence: %#v", signals)
	}
}

func TestEvaluatorMarksMissingSeriesUnknown(t *testing.T) {
	profile := parsedTestProfile(t)
	queryer := &sequenceQueryer{results: []queryResult{
		{samples: []Sample{{Value: 91, Labels: map[string]string{"zone": "a"}}}},
		{samples: []Sample{}},
		{samples: []Sample{}},
	}}
	var signals []ClusterSignal
	evaluator := testEvaluator(profile, queryer, func(signal ClusterSignal) { signals = append(signals, signal) })
	for range queryer.results {
		evaluator.evaluateProfile(context.Background(), profile, evaluator.now())
	}
	if len(signals) != 2 || signals[1].Status != "unknown" {
		t.Fatalf("missing series did not produce one unknown transition: %#v", signals)
	}
}

func TestEvaluatorRejectsCollapsedSeriesIdentities(t *testing.T) {
	profile := parsedTestProfile(t)
	queryer := &sequenceQueryer{
		results: []queryResult{{samples: []Sample{
			{Value: 81, Labels: map[string]string{"zone": "a", "pod": "one"}},
			{Value: 82, Labels: map[string]string{"zone": "a", "pod": "two"}},
		}}},
	}
	failures := 0
	evaluator := testEvaluator(profile, queryer, nil)
	evaluator.hooks.Failure = func(_, _ string, _ error) { failures++ }
	evaluator.evaluateProfile(context.Background(), profile, evaluator.now())
	if failures != 1 {
		t.Fatalf("expected collapsed identity failure, got %d", failures)
	}
	if len(evaluator.states) != 0 {
		t.Fatalf("invalid rule result partially changed state: %#v", evaluator.states)
	}
}

func TestEvaluatorResolvesRemovedProfileAndUpdatesMetrics(t *testing.T) {
	store := NewStore([]string{"monitoring"})
	if err := store.Upsert("kube-system/profile", validProfile()); err != nil {
		t.Fatalf("load profile: %v", err)
	}
	queryer := &sequenceQueryer{results: []queryResult{{samples: []Sample{{Value: 91, Labels: map[string]string{"zone": "a"}}}}}}
	var signals []ClusterSignal
	var warning, critical int
	evaluator := NewEvaluator(store, queryer, "42", func(signal ClusterSignal) {
		signals = append(signals, signal)
	}, EvaluationHooks{ActiveSignals: func(_, currentWarning, currentCritical int) {
		warning, critical = currentWarning, currentCritical
	}})
	evaluator.now = func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) }
	evaluator.evaluateProfiles(context.Background(), true)
	if warning != 0 || critical != 1 {
		t.Fatalf("unexpected active metrics: warning=%d critical=%d", warning, critical)
	}
	store.Delete("kube-system/profile")
	evaluator.evaluateProfiles(context.Background(), true)
	if len(signals) != 2 || signals[1].Status != "resolved" || warning != 0 || critical != 0 {
		t.Fatalf("profile removal did not resolve signal: signals=%#v warning=%d critical=%d", signals, warning, critical)
	}
}

func TestEvaluatorRestartReevaluatesCurrentState(t *testing.T) {
	profile := parsedTestProfile(t)
	queryer := &sequenceQueryer{results: []queryResult{{samples: []Sample{{Value: 91, Labels: map[string]string{"zone": "a"}}}}}}
	var first, restarted []ClusterSignal
	testEvaluator(profile, queryer, func(signal ClusterSignal) { first = append(first, signal) }).evaluateProfile(context.Background(), profile, time.Now())
	testEvaluator(profile, queryer, func(signal ClusterSignal) { restarted = append(restarted, signal) }).evaluateProfile(context.Background(), profile, time.Now())
	if len(first) != 1 || len(restarted) != 1 || restarted[0].Status != "active" {
		t.Fatalf("restart did not reconstruct current signal: first=%#v restarted=%#v", first, restarted)
	}
}

func TestEvaluatorRestartCannotResolveSeriesThatDisappearedWhileStopped(t *testing.T) {
	profile := parsedTestProfile(t)
	firstQueryer := &sequenceQueryer{results: []queryResult{{samples: []Sample{{
		Value: 91, Labels: map[string]string{"zone": "a"},
	}}}}}
	var first []ClusterSignal
	testEvaluator(profile, firstQueryer, func(signal ClusterSignal) {
		first = append(first, signal)
	}).evaluateProfile(context.Background(), profile, time.Now())

	restartedQueryer := &sequenceQueryer{results: []queryResult{{samples: []Sample{}}}}
	var restarted []ClusterSignal
	testEvaluator(profile, restartedQueryer, func(signal ClusterSignal) {
		restarted = append(restarted, signal)
	}).evaluateProfile(context.Background(), profile, time.Now())

	if len(first) != 1 || first[0].Status != "active" {
		t.Fatalf("initial evaluator did not emit active state: %#v", first)
	}
	if len(restarted) != 0 {
		t.Fatalf("restart invented a transition without the former identity: %#v", restarted)
	}
}

func TestEvaluatorBoundsStateIdentities(t *testing.T) {
	profile := parsedTestProfile(t)
	evaluator := testEvaluator(profile, &sequenceQueryer{}, nil)
	for index := 0; index < MaxEvaluatorStates; index++ {
		evaluator.states[fmt.Sprintf("existing-%d", index)] = signalState{}
	}
	err := evaluator.applySamples(
		profile, profile.Spec.Rules[0],
		[]Sample{{Value: 91, Labels: map[string]string{"zone": "new"}}},
		evaluator.now(),
	)
	if err == nil || err.Error() != fmt.Sprintf("monitoring signal state limit %d reached", MaxEvaluatorStates) {
		t.Fatalf("expected state limit error, got %v", err)
	}
	if len(evaluator.states) != MaxEvaluatorStates {
		t.Fatalf("state limit failure mutated evaluator state: %d", len(evaluator.states))
	}
}

func TestEvaluatorBatchesWorstCaseSequentialQueries(t *testing.T) {
	profile := parsedTestProfile(t)
	baseRule := profile.Spec.Rules[0]
	profile.Spec.Rules = make([]Rule, MaxQueriesPerEvaluationBatch+1)
	for index := range profile.Spec.Rules {
		profile.Spec.Rules[index] = baseRule
		profile.Spec.Rules[index].ID = fmt.Sprintf("rule-%d", index)
	}
	store := NewStore([]string{"monitoring"})
	store.profiles["kube-system/profile"] = profile
	queryer := &sequenceQueryer{results: []queryResult{{samples: []Sample{}}}}
	evaluator := NewEvaluator(store, queryer, "42", nil, EvaluationHooks{})
	evaluator.now = func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) }

	if more := evaluator.evaluateProfiles(context.Background(), true); !more {
		t.Fatal("expected another bounded evaluation batch")
	}
	if queryer.calls != MaxQueriesPerEvaluationBatch {
		t.Fatalf("first batch issued %d queries, want %d", queryer.calls, MaxQueriesPerEvaluationBatch)
	}
	if more := evaluator.evaluateProfiles(context.Background(), false); more {
		t.Fatal("final bounded evaluation batch unexpectedly reports more work")
	}
	if queryer.calls != MaxQueriesPerEvaluationBatch+1 {
		t.Fatalf("second batch did not finish remaining query: %d", queryer.calls)
	}
}

type cancellableQueryer struct {
	calls    atomic.Int32
	started  chan int32
	canceled chan int32
}

func (q *cancellableQueryer) Query(ctx context.Context, _ Source, _ string) ([]Sample, error) {
	call := q.calls.Add(1)
	q.started <- call
	<-ctx.Done()
	q.canceled <- call
	return nil, ctx.Err()
}

func TestEvaluatorProfileChangeCancelsCurrentQueryAndRestarts(t *testing.T) {
	store := NewStore([]string{"monitoring"})
	if err := store.Upsert("kube-system/profile", validProfile()); err != nil {
		t.Fatalf("load profile: %v", err)
	}
	queryer := &cancellableQueryer{
		started: make(chan int32, 2), canceled: make(chan int32, 2),
	}
	evaluator := NewEvaluator(store, queryer, "42", nil, EvaluationHooks{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		evaluator.Run(ctx)
		close(done)
	}()

	if call := waitForCall(t, queryer.started); call != 1 {
		t.Fatalf("unexpected initial query call %d", call)
	}
	if err := store.Upsert("kube-system/profile", validProfile()); err != nil {
		t.Fatalf("update profile: %v", err)
	}
	if call := waitForCall(t, queryer.canceled); call != 1 {
		t.Fatalf("profile update canceled query %d, want 1", call)
	}
	if call := waitForCall(t, queryer.started); call != 2 {
		t.Fatalf("profile update started query %d, want 2", call)
	}
	cancel()
	if call := waitForCall(t, queryer.canceled); call != 2 {
		t.Fatalf("shutdown canceled query %d, want 2", call)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("evaluator did not stop after canceling its active query")
	}
}

type immediateQueryer struct {
	calls  atomic.Int32
	called chan struct{}
}

func (q *immediateQueryer) Query(_ context.Context, _ Source, _ string) ([]Sample, error) {
	q.calls.Add(1)
	select {
	case q.called <- struct{}{}:
	default:
	}
	return []Sample{}, nil
}

func TestEvaluatorIdlesBetweenSchedulerTicks(t *testing.T) {
	store := NewStore([]string{"monitoring"})
	if err := store.Upsert("kube-system/profile", validProfile()); err != nil {
		t.Fatalf("load profile: %v", err)
	}
	queryer := &immediateQueryer{called: make(chan struct{}, 2)}
	evaluator := NewEvaluator(store, queryer, "42", nil, EvaluationHooks{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		evaluator.Run(ctx)
		close(done)
	}()

	select {
	case <-queryer.called:
	case <-time.After(time.Second):
		t.Fatal("initial evaluation did not run")
	}
	select {
	case <-queryer.called:
		t.Fatal("evaluator spun another query before its scheduler tick")
	case <-time.After(100 * time.Millisecond):
	}
	if calls := queryer.calls.Load(); calls != 1 {
		t.Fatalf("evaluator issued %d queries before its scheduler tick", calls)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle evaluator did not stop after cancellation")
	}
}

func TestEvaluatorRunsAtMostOneBoundedBatchPerSchedulerInterval(t *testing.T) {
	profile := parsedTestProfile(t)
	baseRule := profile.Spec.Rules[0]
	profile.Spec.Rules = make([]Rule, MaxQueriesPerEvaluationBatch+1)
	for index := range profile.Spec.Rules {
		profile.Spec.Rules[index] = baseRule
		profile.Spec.Rules[index].ID = fmt.Sprintf("rule-%d", index)
	}
	store := NewStore([]string{"monitoring"})
	store.profiles["kube-system/profile"] = profile
	queryer := &immediateQueryer{
		called: make(chan struct{}, MaxQueriesPerEvaluationBatch+1),
	}
	evaluator := NewEvaluator(store, queryer, "42", nil, EvaluationHooks{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		evaluator.Run(ctx)
		close(done)
	}()

	for index := 0; index < MaxQueriesPerEvaluationBatch; index++ {
		select {
		case <-queryer.called:
		case <-time.After(time.Second):
			t.Fatalf("initial batch stopped after %d queries", index)
		}
	}
	select {
	case <-queryer.called:
		t.Fatalf("evaluator exceeded %d queries in one scheduler interval", MaxQueriesPerEvaluationBatch)
	case <-time.After(100 * time.Millisecond):
	}
	if calls := queryer.calls.Load(); calls != MaxQueriesPerEvaluationBatch {
		t.Fatalf("evaluator issued %d queries in one scheduler interval", calls)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded evaluator did not stop after cancellation")
	}
}

func waitForCall(t *testing.T, calls <-chan int32) int32 {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for evaluator query")
		return 0
	}
}

func TestEvaluatorRunStopsOnContextCancellation(t *testing.T) {
	store := NewStore([]string{"monitoring"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		NewEvaluator(store, &sequenceQueryer{}, "42", nil, EvaluationHooks{}).Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("evaluator did not stop after cancellation")
	}
}

func parsedTestProfile(t *testing.T) Profile {
	t.Helper()
	profile, err := ParseProfile(validProfile(), allowed("monitoring"))
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	return profile
}

func testEvaluator(profile Profile, queryer Queryer, emit func(ClusterSignal)) *Evaluator {
	store := NewStore([]string{"monitoring"})
	evaluator := NewEvaluator(store, queryer, "42", emit, EvaluationHooks{})
	evaluator.now = func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) }
	return evaluator
}
