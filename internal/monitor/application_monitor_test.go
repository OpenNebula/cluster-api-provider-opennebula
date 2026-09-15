/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestChartEventsMapApplicationPhases(t *testing.T) {
	tests := []struct {
		phase   string
		deleted bool
		event   string
		state   string
	}{
		{"", false, "chart_state_changed", "installing"},
		{string(applicationv1.PhasePending), false, "chart_state_changed", "installing"},
		{string(applicationv1.PhaseInstalling), false, "chart_state_changed", "installing"},
		{string(applicationv1.PhaseReady), false, "chart_state_changed", "ready"},
		{string(applicationv1.PhaseDeleting), false, "chart_state_changed", "deleting"},
		{string(applicationv1.PhaseDeleting), true, "chart_state_changed", "done"},
	}
	for _, test := range tests {
		app := testApplication("root", "runai", nil, test.phase)
		event := chartEvent(app, test.deleted)
		if event.Event != test.event || event.Payload["state"] != test.state {
			t.Fatalf("phase %q deleted=%t produced %#v", test.phase, test.deleted, event)
		}
		if event.Payload["release_name"] != "runai" || event.Payload["resource_version"] != "17" {
			t.Fatalf("unexpected correlation payload: %#v", event.Payload)
		}
	}
}

func TestChartFailedUsesLastError(t *testing.T) {
	app := testApplication("root", "runai", nil, string(applicationv1.PhaseFailed))
	app.Object["status"].(map[string]any)["lastError"] = map[string]any{
		"reason": "InstallerJobFailed", "message": "helm job failed",
	}
	event := chartEvent(app, false)
	if event.Event != "chart_failed" {
		t.Fatalf("unexpected event: %#v", event)
	}
	if event.Payload["error_msg"] != "helm job failed" {
		t.Fatalf("unexpected failure payload: %#v", event.Payload)
	}
	if _, found := event.Payload["state"]; found {
		t.Fatalf("chart_failed contains state: %#v", event.Payload)
	}
}

func testApplication(name, releaseName string, dependencies []any, phase string) *unstructured.Unstructured {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "oneks.opennebula.io/v1beta1",
		"kind":       "OneKSApplication",
		"spec": map[string]any{
			"release":      map[string]any{"releaseName": releaseName},
			"dependencies": dependencies,
		},
		"status": map[string]any{"phase": phase},
	}}
	app.SetName(name)
	app.SetNamespace(applicationv1.ApplicationNamespace)
	app.SetResourceVersion("17")
	return app
}

func TestDeletingTimestampOverridesFailedPhase(t *testing.T) {
	app := testApplication("root", "runai", nil, string(applicationv1.PhaseFailed))
	now := metav1.Now()
	app.SetDeletionTimestamp(&now)
	event := chartEvent(app, false)
	if event.Event != "chart_state_changed" || event.Payload["state"] != "deleting" {
		t.Fatalf("deleting application produced unexpected event: %#v", event)
	}
}
