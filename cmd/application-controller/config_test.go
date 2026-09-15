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

package main

import (
	"testing"
	"time"
)

func TestConfigFromEnvUsesControllerDefaults(t *testing.T) {
	t.Setenv("APPLICATION_CLUSTER_ID", " 42 ")
	t.Setenv("APPLICATION_METRICS_ADDRESS", "")
	t.Setenv("APPLICATION_HEALTH_ADDRESS", "")
	t.Setenv("APPLICATION_RECONCILIATION_POLL", "")

	config, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.clusterID != "42" || config.metricsAddress != "0" ||
		config.healthAddress != ":8081" || config.reconciliationPoll != 15*time.Second {
		t.Fatalf("unexpected controller configuration: %#v", config)
	}
}

func TestConfigFromEnvReadsControllerSettings(t *testing.T) {
	t.Setenv("APPLICATION_CLUSTER_ID", "42")
	t.Setenv("APPLICATION_METRICS_ADDRESS", ":9090")
	t.Setenv("APPLICATION_HEALTH_ADDRESS", ":8082")
	t.Setenv("APPLICATION_RECONCILIATION_POLL", "30s")

	config, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.metricsAddress != ":9090" || config.healthAddress != ":8082" ||
		config.reconciliationPoll != 30*time.Second {
		t.Fatalf("unexpected controller configuration: %#v", config)
	}
}

func TestConfigFromEnvRejectsInvalidControllerSettings(t *testing.T) {
	t.Setenv("APPLICATION_CLUSTER_ID", "")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("expected missing cluster ID to fail")
	}

	t.Setenv("APPLICATION_CLUSTER_ID", "42")
	t.Setenv("APPLICATION_RECONCILIATION_POLL", "0s")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("expected non-positive reconciliation poll to fail")
	}
}
