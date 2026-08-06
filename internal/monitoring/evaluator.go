/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitoring

import (
	"context"
	"fmt"
	"time"
)

const (
	MaxEvaluatorStates           = 4096
	MaxQueriesPerEvaluationBatch = 32
)

type Queryer interface {
	Query(context.Context, Source, string) ([]Sample, error)
}

type EvaluationHooks struct {
	Failure       func(profile, rule string, err error)
	ActiveSignals func(info, warning, critical int)
}

type signalState struct {
	signal         ClusterSignal
	activeSeverity string
}

type Evaluator struct {
	store     *Store
	queryer   Queryer
	clusterID string
	emit      func(ClusterSignal)
	hooks     EvaluationHooks
	now       func() time.Time
	states    map[string]signalState
	next      map[string]time.Time
}

func NewEvaluator(
	store *Store, queryer Queryer, clusterID string,
	emit func(ClusterSignal), hooks EvaluationHooks,
) *Evaluator {
	return &Evaluator{
		store: store, queryer: queryer, clusterID: clusterID,
		emit: emit, hooks: hooks, now: time.Now,
		states: make(map[string]signalState),
		next:   make(map[string]time.Time),
	}
}

func (e *Evaluator) Run(ctx context.Context) {
	e.drainChanges()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	startEvaluation := func(force bool) (<-chan struct{}, context.CancelFunc) {
		evaluationContext, cancel := context.WithCancel(ctx)
		result := make(chan struct{}, 1)
		go func() {
			e.evaluateProfiles(evaluationContext, force)
			result <- struct{}{}
		}()
		return result, cancel
	}

	result, cancelEvaluation := startEvaluation(true)
	pending := false
	force := false
	for {
		select {
		case <-ctx.Done():
			if cancelEvaluation != nil {
				cancelEvaluation()
				<-result
			}
			return
		case <-e.store.Changes():
			if cancelEvaluation == nil {
				result, cancelEvaluation = startEvaluation(true)
			} else {
				pending = true
				force = true
				cancelEvaluation()
			}
		case <-ticker.C:
			if cancelEvaluation == nil {
				result, cancelEvaluation = startEvaluation(false)
			} else {
				pending = true
			}
		case <-result:
			result = nil
			cancelEvaluation = nil
			if pending {
				result, cancelEvaluation = startEvaluation(force)
				pending = false
				force = false
			}
		}
	}
}

func (e *Evaluator) evaluateProfiles(ctx context.Context, force bool) bool {
	now := e.now()
	profiles := e.store.List()
	configured := make(map[string]struct{})
	if force {
		clear(e.next)
	}
	queries := 0
	moreDue := false
	for _, profile := range profiles {
		sources := make(map[string]Source, len(profile.Spec.Sources))
		for _, source := range profile.Spec.Sources {
			sources[source.ID] = source
		}
		for _, rule := range profile.Spec.Rules {
			key := profile.Metadata.Name + "\x00" + rule.ID
			configured[key] = struct{}{}
			if now.Before(e.next[key]) {
				continue
			}
			if queries >= MaxQueriesPerEvaluationBatch {
				moreDue = true
				continue
			}
			if ctx.Err() != nil {
				return false
			}
			if !e.evaluateRule(ctx, profile, rule, sources[rule.Source], now) {
				return false
			}
			e.next[key] = now.Add(profile.Spec.EvaluationInterval.Value())
			queries++
		}
	}
	for key := range e.next {
		if _, exists := configured[key]; !exists {
			delete(e.next, key)
		}
	}
	e.removeUnconfigured(configured, now)
	e.updateActiveMetrics()
	return moreDue
}

func (e *Evaluator) evaluateProfile(ctx context.Context, profile Profile, now time.Time) bool {
	sources := make(map[string]Source, len(profile.Spec.Sources))
	for _, source := range profile.Spec.Sources {
		sources[source.ID] = source
	}
	for _, rule := range profile.Spec.Rules {
		if !e.evaluateRule(ctx, profile, rule, sources[rule.Source], now) {
			return false
		}
	}
	return true
}

func (e *Evaluator) evaluateRule(
	ctx context.Context, profile Profile, rule Rule, source Source, now time.Time,
) bool {
	if ctx.Err() != nil {
		return false
	}
	queryContext, cancel := context.WithTimeout(ctx, source.Timeout.Value())
	samples, err := e.queryer.Query(queryContext, source, rule.Query)
	cancel()
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		e.ruleFailed(profile, rule, now, err)
		return true
	}
	if err := e.applySamples(profile, rule, samples, now); err != nil {
		e.ruleFailed(profile, rule, now, err)
	}
	return true
}

func (e *Evaluator) applySamples(profile Profile, rule Rule, samples []Sample, now time.Time) error {
	type candidateSignal struct {
		signal         ClusterSignal
		activeSeverity string
	}
	seen := make(map[string]struct{}, len(samples))
	candidates := make([]candidateSignal, 0, len(samples))
	newStates := 0
	for _, sample := range samples {
		labels, err := signalLabels(rule, sample)
		if err != nil {
			return err
		}
		identity := signalIdentity(profile.Metadata.Name, rule.ID, SignalSourcePrometheus, labels)
		if _, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("multiple Prometheus series map to signal identity %s", identity)
		}
		seen[identity] = struct{}{}
		previous, exists := e.states[identity]
		if !exists {
			newStates++
			if len(e.states)+newStates > MaxEvaluatorStates {
				return fmt.Errorf("monitoring signal state limit %d reached", MaxEvaluatorStates)
			}
		}
		status, severity, threshold, activeSeverity := classify(rule, sample.Value, previous.activeSeverity)
		signal, err := NewClusterSignal(
			e.clusterID, profile, rule, sample, status, severity,
			threshold, now, "",
		)
		if err != nil {
			return err
		}
		candidates = append(candidates, candidateSignal{
			signal: signal, activeSeverity: activeSeverity,
		})
	}
	for _, candidate := range candidates {
		e.record(candidate.signal, candidate.activeSeverity)
	}
	for identity, state := range e.states {
		if state.signal.Profile != profile.Metadata.Name || state.signal.Rule != rule.ID {
			continue
		}
		if _, exists := seen[identity]; !exists {
			e.recordUnknown(state, now, "No current Prometheus series for monitoring rule")
		}
	}
	return nil
}

func classify(rule Rule, value float64, previousActive string) (string, string, float64, string) {
	if previousActive != "" && !recovered(rule, value) {
		severity := "warning"
		threshold := rule.Warning.Value()
		if triggers(rule.Comparison, value, rule.Critical.Value()) {
			severity = "critical"
			threshold = rule.Critical.Value()
		}
		return "active", severity, threshold, severity
	}
	if triggers(rule.Comparison, value, rule.Critical.Value()) {
		return "active", "critical", rule.Critical.Value(), "critical"
	}
	if triggers(rule.Comparison, value, rule.Warning.Value()) {
		return "active", "warning", rule.Warning.Value(), "warning"
	}
	return "resolved", "info", rule.Recovery.Value(), ""
}

func triggers(comparison Comparison, value, threshold float64) bool {
	switch comparison {
	case GreaterThan:
		return value > threshold
	case GreaterThanOrEqual:
		return value >= threshold
	case LessThan:
		return value < threshold
	case LessThanOrEqual:
		return value <= threshold
	default:
		return false
	}
}

func recovered(rule Rule, value float64) bool {
	switch rule.Comparison {
	case GreaterThan:
		return value <= rule.Recovery.Value()
	case GreaterThanOrEqual:
		return value < rule.Recovery.Value()
	case LessThan:
		return value >= rule.Recovery.Value()
	case LessThanOrEqual:
		return value > rule.Recovery.Value()
	default:
		return false
	}
}

func (e *Evaluator) ruleFailed(profile Profile, rule Rule, now time.Time, err error) {
	if e.hooks.Failure != nil {
		e.hooks.Failure(profile.Metadata.Name, rule.ID, err)
	}
	for _, state := range e.states {
		if state.signal.Profile == profile.Metadata.Name && state.signal.Rule == rule.ID {
			e.recordUnknown(state, now, "Monitoring rule evaluation is temporarily unavailable")
		}
	}
}

func (e *Evaluator) recordUnknown(state signalState, now time.Time, message string) {
	signal := state.signal
	signal.Status = "unknown"
	signal.Severity = "info"
	signal.ObservedAt = now.UTC().Format(time.RFC3339Nano)
	signal.Message = message
	if err := signal.Validate(); err != nil {
		if e.hooks.Failure != nil {
			e.hooks.Failure(signal.Profile, signal.Rule, err)
		}
		return
	}
	e.record(signal, state.activeSeverity)
}

func (e *Evaluator) record(signal ClusterSignal, activeSeverity string) {
	previous, exists := e.states[signal.Identity]
	e.states[signal.Identity] = signalState{signal: signal, activeSeverity: activeSeverity}
	if exists && previous.signal.Status == signal.Status && previous.signal.Severity == signal.Severity {
		return
	}
	if e.emit != nil {
		e.emit(signal)
	}
}

func (e *Evaluator) removeUnconfigured(configured map[string]struct{}, now time.Time) {
	for identity, state := range e.states {
		key := state.signal.Profile + "\x00" + state.signal.Rule
		if _, exists := configured[key]; exists {
			continue
		}
		if state.signal.Status != "resolved" {
			signal := state.signal
			signal.Status = "resolved"
			signal.Severity = "info"
			signal.ObservedAt = now.UTC().Format(time.RFC3339Nano)
			signal.Message = "Monitoring profile or rule was removed"
			if signal.Validate() == nil && e.emit != nil {
				e.emit(signal)
			}
		}
		delete(e.states, identity)
	}
}

func (e *Evaluator) updateActiveMetrics() {
	var warning, critical int
	for _, state := range e.states {
		if state.signal.Status != "active" {
			continue
		}
		switch state.signal.Severity {
		case "warning":
			warning++
		case "critical":
			critical++
		}
	}
	if e.hooks.ActiveSignals != nil {
		e.hooks.ActiveSignals(0, warning, critical)
	}
}

func (e *Evaluator) drainChanges() {
	for {
		select {
		case <-e.store.Changes():
		default:
			return
		}
	}
}
