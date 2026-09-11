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

func TestConfigFromEnvUsesOpenNebulaEndpointWithoutClusterID(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("ONE_XMLRPC", "http://opennebula.example:2633/RPC2")
	t.Setenv("MONITOR_AUTH_FILE", "/var/run/secrets/oneks-monitor/ONE_AUTH")
	t.Setenv("MONITOR_HEALTH_ADDRESS", ":8081")
	t.Setenv("MONITOR_KEY", base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")))
	t.Setenv("MONITOR_HTTP_TIMEOUT", "10s")
	t.Setenv("MONITOR_POD_POLL_INTERVAL", "30s")
	t.Setenv("MONITOR_CLUSTER_ID", "")

	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.OpenNebulaEndpoint != "http://opennebula.example:2633/RPC2" {
		t.Fatalf("unexpected OpenNebula endpoint: %q", config.OpenNebulaEndpoint)
	}
	if config.PodPollInterval != 30*time.Second {
		t.Fatalf("unexpected Pod poll interval: %s", config.PodPollInterval)
	}
}

func TestConfigFromEnvRequiresOpenNebulaEndpoint(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("ONE_XMLRPC", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected ONE_XMLRPC validation error")
	}
}

func TestConfigFromEnvRejectsInvalidPodPollInterval(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "https://oneks.example/api/v1")
	t.Setenv("ONE_XMLRPC", "http://opennebula.example:2633/RPC2")
	t.Setenv("MONITOR_AUTH_FILE", "/var/run/secrets/oneks-monitor/ONE_AUTH")
	t.Setenv("MONITOR_HEALTH_ADDRESS", ":8081")
	t.Setenv("MONITOR_KEY", base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")))
	t.Setenv("MONITOR_HTTP_TIMEOUT", "10s")
	t.Setenv("MONITOR_POD_POLL_INTERVAL", "0s")

	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected MONITOR_POD_POLL_INTERVAL validation error")
	}
}
