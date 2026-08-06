/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitor

import (
	"strings"
	"testing"
)

func TestMetricsListenerIsDisabledByDefault(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("MONITOR_CLUSTER_ID", "42")
	t.Setenv("MONITOR_TOKEN", "token")
	t.Setenv("MONITOR_AUTH", "oneadmin:secret")
	t.Setenv("MONITOR_METRICS_ADDRESS", "")

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.HealthAddress != ":8081" {
		t.Fatalf("health address changed: %q", config.HealthAddress)
	}
	if config.MetricsAddress != "" {
		t.Fatalf("metrics listener enabled by default at %q", config.MetricsAddress)
	}
}

func TestMetricsListenerUsesSeparateConfiguredAddress(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("MONITOR_CLUSTER_ID", "42")
	t.Setenv("MONITOR_TOKEN", "token")
	t.Setenv("MONITOR_AUTH", "oneadmin:secret")
	t.Setenv("MONITOR_METRICS_ADDRESS", "127.0.0.1:9090")

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.MetricsAddress != "127.0.0.1:9090" {
		t.Fatalf("unexpected metrics address: %q", config.MetricsAddress)
	}
}

func TestMonitoringNamespacesAreExplicitAndNormalized(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("MONITOR_CLUSTER_ID", "42")
	t.Setenv("MONITOR_TOKEN", "token")
	t.Setenv("MONITOR_AUTH", "oneadmin:secret")
	t.Setenv("MONITOR_CHART_NAMESPACE", "charts")
	t.Setenv("MONITOR_PROFILE_NAMESPACE", "monitor-system")
	t.Setenv("MONITOR_PROMETHEUS_NAMESPACES", "monitoring, observability,monitoring")

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.ProfileNamespace != "monitor-system" {
		t.Fatalf("unexpected profile namespace: %q", config.ProfileNamespace)
	}
	if len(config.PrometheusNamespaces) != 2 || config.PrometheusNamespaces[0] != "monitoring" || config.PrometheusNamespaces[1] != "observability" {
		t.Fatalf("unexpected namespace allowlist: %#v", config.PrometheusNamespaces)
	}
}

func TestMonitoringNamespaceAllowlistRejectsInvalidNames(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("MONITOR_CLUSTER_ID", "42")
	t.Setenv("MONITOR_TOKEN", "token")
	t.Setenv("MONITOR_AUTH", "oneadmin:secret")
	t.Setenv("MONITOR_PROMETHEUS_NAMESPACES", "monitoring,https://external.example")

	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected invalid namespace allowlist rejection")
	}
}

func TestClusterIDIsBoundedBeforeEvaluatorConstruction(t *testing.T) {
	for name, clusterID := range map[string]string{
		"too long":     strings.Repeat("x", 129),
		"invalid UTF8": string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
			t.Setenv("MONITOR_CLUSTER_ID", clusterID)
			t.Setenv("MONITOR_TOKEN", "token")
			t.Setenv("MONITOR_AUTH", "oneadmin:secret")

			if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "at most 128 bytes") {
				t.Fatalf("expected bounded cluster ID error, got %v", err)
			}
		})
	}
}

func TestEndpointRequiresAbsoluteCredentialFreeHTTPS(t *testing.T) {
	for name, endpoint := range map[string]string{
		"HTTP":        "http://oneks.example/api/v1",
		"relative":    "/api/v1",
		"credentials": "https://user:password@oneks.example/api/v1",
		"query":       "https://oneks.example/api/v1?target=elsewhere",
		"fragment":    "https://oneks.example/api/v1#fragment",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MONITOR_ENDPOINT", endpoint)
			t.Setenv("MONITOR_CLUSTER_ID", "42")
			t.Setenv("MONITOR_TOKEN", "token")
			t.Setenv("MONITOR_AUTH", "oneadmin:secret")

			if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "absolute HTTPS URL") {
				t.Fatalf("expected secure endpoint validation error, got %v", err)
			}
		})
	}
}
