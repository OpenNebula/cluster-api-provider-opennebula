/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestConfigFromEnvConfiguresResourceObservations(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("MONITOR_CLUSTER_ID", "42")
	t.Setenv("ONE_XMLRPC", "http://opennebula.example:2633/RPC2")
	t.Setenv("MONITOR_AUTH_FILE", "/var/run/secrets/oneks-monitor/ONE_AUTH")
	t.Setenv("MONITOR_HEALTH_ADDRESS", ":8081")
	t.Setenv("MONITOR_KEY", base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")))
	t.Setenv("MONITOR_HTTP_TIMEOUT", "10s")
	t.Setenv("MONITOR_POD_POLL_INTERVAL", "20s")
	t.Setenv("MONITOR_RESOURCE_POLL_INTERVAL", "30s")
	t.Setenv("MONITOR_RESOURCE_CONFIG_NAMESPACE", "monitoring")
	t.Setenv("MONITOR_RESOURCE_CONFIG_NAME", "observed-resources")

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.OpenNebulaEndpoint != "http://opennebula.example:2633/RPC2" {
		t.Fatalf("unexpected OpenNebula endpoint: %q", config.OpenNebulaEndpoint)
	}
	if config.ClusterID != "42" {
		t.Fatalf("unexpected cluster ID: %q", config.ClusterID)
	}
	if config.PodPollInterval != 20*time.Second || config.ResourcePollInterval != 30*time.Second {
		t.Fatalf("unexpected observation configuration: %#v", config)
	}
	if config.ResourceConfigNamespace != "monitoring" || config.ResourceConfigName != "observed-resources" {
		t.Fatalf("unexpected resource ConfigMap: %#v", config)
	}
}

func TestConfigFromEnvRequiresOpenNebulaEndpoint(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("MONITOR_CLUSTER_ID", "42")
	t.Setenv("ONE_XMLRPC", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected ONE_XMLRPC validation error")
	}
}

func TestConfigFromEnvRequiresClusterID(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("MONITOR_CLUSTER_ID", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected MONITOR_CLUSTER_ID validation error")
	}
}

func TestConfigFromEnvRejectsInvalidResourcePollInterval(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("MONITOR_CLUSTER_ID", "42")
	t.Setenv("ONE_XMLRPC", "http://opennebula.example:2633/RPC2")
	t.Setenv("MONITOR_AUTH_FILE", "/var/run/secrets/oneks-monitor/ONE_AUTH")
	t.Setenv("MONITOR_HEALTH_ADDRESS", ":8081")
	t.Setenv("MONITOR_KEY", base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")))
	t.Setenv("MONITOR_HTTP_TIMEOUT", "10s")
	t.Setenv("MONITOR_POD_POLL_INTERVAL", "30s")
	t.Setenv("MONITOR_RESOURCE_POLL_INTERVAL", "0s")

	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected MONITOR_RESOURCE_POLL_INTERVAL validation error")
	}
}
