/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
)

var chartMetricStates = []string{"pending", "deployed", "failed", "uninstalling", "unknown"}

// Metrics owns the monitor's bounded, low-cardinality Prometheus metrics.
type Metrics struct {
	nodesTotal         prometheus.Gauge
	nodesReady         prometheus.Gauge
	nodesNotReady      prometheus.Gauge
	memoryPressure     prometheus.Gauge
	diskPressure       prometheus.Gauge
	pidPressure        prometheus.Gauge
	helmCharts         *prometheus.GaugeVec
	queueDepth         prometheus.Gauge
	callbackAttempts   prometheus.Counter
	callbackFailures   prometheus.Counter
	callbackRetries    prometheus.Counter
	callbackRejections prometheus.Counter
	lastCallback       prometheus.Gauge
	profileFailures    prometheus.Counter
	ruleFailures       prometheus.Counter
	activeSignals      *prometheus.GaugeVec
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	m := &Metrics{
		nodesTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "capone_monitor_nodes_total", Help: "Number of monitored Kubernetes Nodes.",
		}),
		nodesReady: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "capone_monitor_nodes_ready", Help: "Number of monitored Nodes with Ready=True.",
		}),
		nodesNotReady: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "capone_monitor_nodes_not_ready", Help: "Number of monitored Nodes without Ready=True.",
		}),
		memoryPressure: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "capone_monitor_nodes_memory_pressure", Help: "Number of monitored Nodes with MemoryPressure=True.",
		}),
		diskPressure: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "capone_monitor_nodes_disk_pressure", Help: "Number of monitored Nodes with DiskPressure=True.",
		}),
		pidPressure: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "capone_monitor_nodes_pid_pressure", Help: "Number of monitored Nodes with PIDPressure=True.",
		}),
		helmCharts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "capone_monitor_helmcharts", Help: "Number of monitored HelmCharts by lifecycle state.",
		}, []string{"state"}),
		queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "capone_monitor_callback_queue_depth", Help: "Number of callback identities awaiting delivery.",
		}),
		callbackAttempts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "capone_monitor_callback_attempts_total", Help: "Total callback delivery attempts.",
		}),
		callbackFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "capone_monitor_callback_failures_total", Help: "Total failed callback delivery attempts.",
		}),
		callbackRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "capone_monitor_callback_retries_total", Help: "Total callbacks scheduled for rate-limited retry.",
		}),
		callbackRejections: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "capone_monitor_callback_rejected_total", Help: "Total callback identities rejected at the bounded queue limit.",
		}),
		lastCallback: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "capone_monitor_callback_last_success_timestamp_seconds", Help: "Unix timestamp of the last successful callback.",
		}),
		profileFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "capone_monitor_profile_parse_failures_total", Help: "Total rejected MonitoringProfile ConfigMap updates.",
		}),
		ruleFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "capone_monitor_rule_evaluation_failures_total", Help: "Total failed monitoring-rule evaluations.",
		}),
		activeSignals: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "capone_monitor_active_signals", Help: "Number of currently active monitoring signals by severity.",
		}, []string{"severity"}),
	}
	registerer.MustRegister(
		m.nodesTotal, m.nodesReady, m.nodesNotReady, m.memoryPressure,
		m.diskPressure, m.pidPressure, m.helmCharts, m.queueDepth,
		m.callbackAttempts, m.callbackFailures, m.callbackRetries,
		m.callbackRejections, m.lastCallback, m.profileFailures, m.ruleFailures,
		m.activeSignals,
	)
	for _, state := range chartMetricStates {
		m.helmCharts.WithLabelValues(state).Set(0)
	}
	for _, severity := range []string{"info", "warning", "critical"} {
		m.activeSignals.WithLabelValues(severity).Set(0)
	}
	return m
}

func (m *Metrics) setNodes(nodes []*corev1.Node) {
	if m == nil {
		return
	}
	var ready, memory, disk, pid int
	for _, node := range nodes {
		if nodeReady(node) {
			ready++
		}
		for _, condition := range node.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			switch condition.Type {
			case corev1.NodeMemoryPressure:
				memory++
			case corev1.NodeDiskPressure:
				disk++
			case corev1.NodePIDPressure:
				pid++
			}
		}
	}
	m.nodesTotal.Set(float64(len(nodes)))
	m.nodesReady.Set(float64(ready))
	m.nodesNotReady.Set(float64(len(nodes) - ready))
	m.memoryPressure.Set(float64(memory))
	m.diskPressure.Set(float64(disk))
	m.pidPressure.Set(float64(pid))
}

func (m *Metrics) setHelmCharts(states map[string]int) {
	if m == nil {
		return
	}
	for _, state := range chartMetricStates {
		m.helmCharts.WithLabelValues(state).Set(float64(states[state]))
	}
}

func (m *Metrics) setQueueDepth(depth int) {
	if m != nil {
		m.queueDepth.Set(float64(depth))
	}
}

func (m *Metrics) callbackAttempt() {
	if m != nil {
		m.callbackAttempts.Inc()
	}
}

func (m *Metrics) callbackFailed() {
	if m != nil {
		m.callbackFailures.Inc()
		m.callbackRetries.Inc()
	}
}

func (m *Metrics) callbackRejected() {
	if m != nil {
		m.callbackRejections.Inc()
	}
}

func (m *Metrics) callbackSucceeded(now time.Time) {
	if m != nil {
		m.lastCallback.Set(float64(now.Unix()))
	}
}

func (m *Metrics) ProfileParseFailed() {
	if m != nil {
		m.profileFailures.Inc()
	}
}

func (m *Metrics) RuleEvaluationFailed() {
	if m != nil {
		m.ruleFailures.Inc()
	}
}

func (m *Metrics) SetActiveSignals(info, warning, critical int) {
	if m == nil {
		return
	}
	m.activeSignals.WithLabelValues("info").Set(float64(info))
	m.activeSignals.WithLabelValues("warning").Set(float64(warning))
	m.activeSignals.WithLabelValues("critical").Set(float64(critical))
}
