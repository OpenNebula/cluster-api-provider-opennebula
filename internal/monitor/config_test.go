/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitor

import (
	"encoding/base64"
	"strings"
	"testing"
)

func setRequiredMonitorEnv(t *testing.T, endpoint, clusterID string) string {
	t.Helper()
	authFile := writeMonitorAuthFile(t, "oneadmin:secret\n")
	t.Setenv("MONITOR_ENDPOINT", endpoint)
	t.Setenv("MONITOR_CLUSTER_ID", clusterID)
	t.Setenv("MONITOR_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	t.Setenv("MONITOR_AUTH_FILE", authFile)
	t.Setenv("MONITOR_APPLICATION_NAMESPACE", "oneks-system")
	t.Setenv("MONITOR_HTTP_TIMEOUT", "10s")
	t.Setenv("MONITOR_HEALTH_ADDRESS", ":8081")
	return authFile
}

func TestConfigLoadsExplicitValues(t *testing.T) {
	setRequiredMonitorEnv(t, "http://oneks.example/api/v1", "cluster-west_1")

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.ClusterID != "cluster-west_1" {
		t.Fatalf("cluster ID changed: %q", config.ClusterID)
	}
	if config.HealthAddress != ":8081" || config.ApplicationNamespace != "oneks-system" || config.HTTPTimeout.String() != "10s" {
		t.Fatalf("unexpected configuration: %#v", config)
	}
}

func TestConfigRequiresRuntimeValues(t *testing.T) {
	for _, variable := range []string{
		"MONITOR_ENDPOINT",
		"MONITOR_CLUSTER_ID",
		"MONITOR_AUTH_FILE",
		"MONITOR_APPLICATION_NAMESPACE",
		"MONITOR_HEALTH_ADDRESS",
		"MONITOR_KEY",
		"MONITOR_HTTP_TIMEOUT",
	} {
		t.Run(variable, func(t *testing.T) {
			setRequiredMonitorEnv(t, "https://oneks.example/api/v1", "42")
			t.Setenv(variable, "")
			if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), variable+" is required") {
				t.Fatalf("expected %s validation error, got %v", variable, err)
			}
		})
	}
}

func TestEndpointRejectsInvalidURLs(t *testing.T) {
	for name, endpoint := range map[string]string{
		"relative": "/api/v1",
		"scheme":   "ftp://oneks.example/api/v1",
		"no host":  "http:///api/v1",
	} {
		t.Run(name, func(t *testing.T) {
			setRequiredMonitorEnv(t, endpoint, "42")
			if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "absolute HTTP or HTTPS URL") {
				t.Fatalf("expected endpoint validation error, got %v", err)
			}
		})
	}
}

func TestConfigRejectsInvalidTimeout(t *testing.T) {
	setRequiredMonitorEnv(t, "https://oneks.example/api/v1", "42")
	t.Setenv("MONITOR_HTTP_TIMEOUT", "0s")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "positive duration") {
		t.Fatalf("expected timeout validation error, got %v", err)
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
