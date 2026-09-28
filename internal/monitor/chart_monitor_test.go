/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestChartEventsMapAnnotatedStates(t *testing.T) {
	tests := []struct {
		state   string
		deleted bool
		event   string
		want    string
	}{
		{"", false, "app_state_changed", "installing"},
		{"installing", false, "app_state_changed", "installing"},
		{"ready", false, "app_state_changed", "ready"},
		{"deleting", false, "app_state_changed", "deleting"},
		{"deleting", true, "app_state_changed", "done"},
	}
	for _, test := range tests {
		chart := testHelmChart("runai", "runai", "", test.state)
		event := mustChartEvent(t, chart, test.deleted)
		if event.Event != test.event || event.Payload.State != test.want {
			t.Fatalf("state %q deleted=%t produced %#v", test.state, test.deleted, event)
		}
		if event.Payload.ReleaseName != "runai" || event.Payload.ResourceVersion != json.Number("17") {
			t.Fatalf("unexpected correlation payload: %#v", event.Payload)
		}
	}
}

func TestDependencyChartEventIncludesParent(t *testing.T) {
	event := mustChartEvent(t, testHelmChart("prometheus", "prometheus", "runai", "ready"), false)
	if event.Payload.Parent != "runai" {
		t.Fatalf("dependency event has no parent: %#v", event.Payload)
	}
}

func TestRootChartEventOmitsParent(t *testing.T) {
	event := mustChartEvent(t, testHelmChart("runai", "runai", "", "ready"), false)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"event":"app_state_changed","payload":{"release_name":"runai","resource_version":17,"state":"ready"}}` {
		t.Fatalf("root event contains parent: %#v", event.Payload)
	}
}

func TestChartEventsEncodeResourceVersionAsInteger(t *testing.T) {
	for _, state := range []string{"ready", "failed"} {
		for _, version := range []string{"17", "9007199254740993", "18446744073709551615"} {
			chart := testHelmChart("runai", "runai", "", state)
			chart.SetResourceVersion(version)
			event := mustChartEvent(t, chart, false)
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Payload map[string]json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := string(decoded.Payload["resource_version"]); got != version {
				t.Fatalf("state %s: resource_version encoded as %s, want integer %s", state, got, version)
			}
		}
	}
}

func TestChartFailedUsesErrorAnnotation(t *testing.T) {
	chart := testHelmChart("runai", "runai", "", "failed")
	annotations := chart.GetAnnotations()
	annotations[errorAnnotation] = "helm job failed"
	chart.SetAnnotations(annotations)
	event := mustChartEvent(t, chart, false)
	if event.Event != "app_failed" || event.Payload.ErrorMessage != "helm job failed" {
		t.Fatalf("unexpected event: %#v", event)
	}
	if event.Payload.State != "" {
		t.Fatalf("app_failed contains state: %#v", event.Payload)
	}
}

func TestDeletingTimestampOverridesFailedState(t *testing.T) {
	chart := testHelmChart("runai", "runai", "", "failed")
	now := metav1.Now()
	chart.SetDeletionTimestamp(&now)
	event := mustChartEvent(t, chart, false)
	if event.Event != "app_state_changed" || event.Payload.State != "deleting" {
		t.Fatalf("deleting chart produced unexpected event: %#v", event)
	}
}

func TestChartEventRejectsInvalidMetadata(t *testing.T) {
	for _, test := range []struct {
		name  string
		chart *unstructured.Unstructured
	}{
		{name: "missing release", chart: testHelmChart("runai", "", "", "ready")},
		{name: "invalid state", chart: testHelmChart("runai", "runai", "", "unknown")},
		{name: "invalid resource version", chart: testHelmChart("runai", "runai", "", "ready")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "invalid resource version" {
				test.chart.SetResourceVersion("opaque")
			}
			if _, err := chartEvent(test.chart, false); err == nil {
				t.Fatal("expected invalid chart metadata error")
			}
		})
	}
}

func TestChartEventSignatureIncludesCorrelationMetadata(t *testing.T) {
	chart := testHelmChart("runai", "runai", "", "installing")
	before := chartEventSignature(chart)
	annotations := chart.GetAnnotations()
	annotations[parentAnnotation] = "platform"
	chart.SetAnnotations(annotations)
	if after := chartEventSignature(chart); after == before {
		t.Fatal("parent annotation change was not detected")
	}
}

func mustChartEvent(t *testing.T, chart *unstructured.Unstructured, deleted bool) ChartEvent {
	t.Helper()
	event, err := chartEvent(chart, deleted)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func testHelmChart(name, releaseName, parent, state string) *unstructured.Unstructured {
	chart := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
	}}
	chart.SetName(name)
	chart.SetNamespace(helmChartNamespace)
	chart.SetResourceVersion("17")
	annotations := map[string]string{
		releaseAnnotation: releaseName,
		stateAnnotation:   state,
	}
	if parent != "" {
		annotations[parentAnnotation] = parent
	}
	chart.SetAnnotations(annotations)
	chart.SetLabels(map[string]string{managedLabel: "true"})
	return chart
}
