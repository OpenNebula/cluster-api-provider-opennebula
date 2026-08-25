/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitor

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setRequiredMonitorEnv(t *testing.T, endpoint, clusterID string) string {
	t.Helper()
	authFile := filepath.Join(t.TempDir(), "ONE_AUTH")
	if err := os.WriteFile(authFile, []byte("oneadmin:secret\n"), 0o600); err != nil {
		t.Fatalf("write authentication file: %v", err)
	}
	t.Setenv("MONITOR_ENDPOINT", endpoint)
	t.Setenv("MONITOR_CLUSTER_ID", clusterID)
	t.Setenv("MONITOR_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	t.Setenv("MONITOR_AUTH_FILE", authFile)
	return authFile
}

func TestMetricsListenerIsDisabledByDefault(t *testing.T) {
	setRequiredMonitorEnv(t, "http://oneks.example/api/v1", "42")
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
	setRequiredMonitorEnv(t, "http://oneks.example/api/v1", "42")
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
	setRequiredMonitorEnv(t, "http://oneks.example/api/v1", "42")
	t.Setenv("MONITOR_APPLICATION_NAMESPACE", "oneks-custom")
	t.Setenv("MONITOR_PROFILE_NAMESPACE", "monitor-system")
	t.Setenv("MONITOR_PROMETHEUS_NAMESPACES", "monitoring, observability,monitoring")

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.ProfileNamespace != "monitor-system" {
		t.Fatalf("unexpected profile namespace: %q", config.ProfileNamespace)
	}
	if config.ApplicationNamespace != "oneks-custom" {
		t.Fatalf("unexpected application namespace: %q", config.ApplicationNamespace)
	}
	if len(config.PrometheusNamespaces) != 2 || config.PrometheusNamespaces[0] != "monitoring" || config.PrometheusNamespaces[1] != "observability" {
		t.Fatalf("unexpected namespace allowlist: %#v", config.PrometheusNamespaces)
	}
}

func TestMonitoringNamespaceAllowlistRejectsInvalidNames(t *testing.T) {
	setRequiredMonitorEnv(t, "http://oneks.example/api/v1", "42")
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
			setRequiredMonitorEnv(t, "http://oneks.example/api/v1", clusterID)

			if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "at most 128 bytes") {
				t.Fatalf("expected bounded cluster ID error, got %v", err)
			}
		})
	}
}

func TestEndpointAcceptsAbsoluteHTTPAndHTTPS(t *testing.T) {
	for _, endpoint := range []string{
		"http://oneks.example/api/v1",
		"https://oneks.example/api/v1",
	} {
		t.Run(endpoint, func(t *testing.T) {
			setRequiredMonitorEnv(t, endpoint, "42")
			if _, err := ConfigFromEnv(); err != nil {
				t.Fatalf("expected endpoint acceptance, got %v", err)
			}
		})
	}
}

func TestEndpointRejectsUnsafeOrInvalidURLs(t *testing.T) {
	for name, endpoint := range map[string]string{
		"relative":    "/api/v1",
		"credentials": "http://user:password@oneks.example/api/v1",
		"query":       "http://oneks.example/api/v1?target=elsewhere",
		"fragment":    "http://oneks.example/api/v1#fragment",
		"scheme":      "ftp://oneks.example/api/v1",
	} {
		t.Run(name, func(t *testing.T) {
			setRequiredMonitorEnv(t, endpoint, "42")

			if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "absolute HTTP or HTTPS URL") {
				t.Fatalf("expected endpoint validation error, got %v", err)
			}
		})
	}
}

func TestMonitorKeyMustDecodeTo32Bytes(t *testing.T) {
	setRequiredMonitorEnv(t, "http://oneks.example/api/v1", "42")
	for name, key := range map[string]string{
		"invalid Base64": "%%%",
		"too short":      base64.StdEncoding.EncodeToString([]byte("short")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MONITOR_KEY", key)
			if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "exactly 32 bytes") {
				t.Fatalf("expected key validation error, got %v", err)
			}
		})
	}
}

func TestMonitorAuthFileIsRequired(t *testing.T) {
	setRequiredMonitorEnv(t, "http://oneks.example/api/v1", "42")
	t.Setenv("MONITOR_AUTH_FILE", "")

	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "MONITOR_AUTH_FILE is required") {
		t.Fatalf("expected authentication file validation error, got %v", err)
	}
}

func TestMonitorAuthFileWhitespaceIsRejected(t *testing.T) {
	setRequiredMonitorEnv(t, "http://oneks.example/api/v1", "42")
	t.Setenv("MONITOR_AUTH_FILE", " \t\n")

	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "MONITOR_AUTH_FILE is required") {
		t.Fatalf("expected authentication file validation error, got %v", err)
	}
}

func TestMonitorAuthFilePathIsAcceptedWithoutLegacyFallback(t *testing.T) {
	authFile := setRequiredMonitorEnv(t, "http://oneks.example/api/v1", "42")
	t.Setenv("MONITOR_AUTH", "legacy:credential")

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.AuthFile != authFile {
		t.Fatalf("unexpected authentication file path: %q", config.AuthFile)
	}

	t.Setenv("MONITOR_AUTH_FILE", "")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "MONITOR_AUTH_FILE is required") {
		t.Fatalf("legacy authentication unexpectedly used as fallback: %v", err)
	}
}
