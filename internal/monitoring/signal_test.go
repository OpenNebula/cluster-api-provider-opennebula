/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitoring

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClusterSignalGoldenJSON(t *testing.T) {
	profile, err := ParseProfile(validProfile(), allowed("monitoring"))
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	signal, err := NewClusterSignal(
		"42", profile, profile.Spec.Rules[0],
		Sample{Value: 91.25, Labels: map[string]string{"zone": "west", "pod": "private"}},
		"active", "critical", 90,
		time.Date(2026, 8, 6, 12, 30, 0, 0, time.UTC), "",
	)
	if err != nil {
		t.Fatalf("create signal: %v", err)
	}
	body, err := json.Marshal(signal)
	if err != nil {
		t.Fatalf("marshal signal: %v", err)
	}
	want := `{"apiVersion":"monitoring.oneks.opennebula.io/v1alpha1","kind":"ClusterSignal","clusterId":"42","identity":"signal-dxdmZNRRXwuAwvelIQ3p0ZuDh1qGS1BrZBrzw4K4tSc","profile":"cluster-health","rule":"node-memory","source":"prometheus","category":"monitoring","severity":"critical","status":"active","observedAt":"2026-08-06T12:30:00Z","value":91.25,"unit":"percent","threshold":90,"labels":{"zone":"west"},"message":"cluster-health/node-memory is critical at 91.25 90"}`
	if string(body) != want {
		t.Fatalf("ClusterSignal JSON changed:\n got: %s\nwant: %s", body, want)
	}
	if strings.Contains(string(body), "private") {
		t.Fatal("non-allowlisted label entered ClusterSignal JSON")
	}
}

func TestSignalIdentityIsCanonicalAndASCIIStable(t *testing.T) {
	left := signalIdentity("profile", "rule", "prometheus", map[string]string{"zone": "wést", "cluster": "a"})
	right := signalIdentity("profile", "rule", "prometheus", map[string]string{"cluster": "a", "zone": "wést"})
	if left != right {
		t.Fatalf("identity depends on map iteration: %q != %q", left, right)
	}
	if len(left) != 50 || !strings.HasPrefix(left, "signal-") {
		t.Fatalf("unexpected identity shape: %q", left)
	}
}

func TestSignalIdentityUsesCanonicalASCIIJSONEscapes(t *testing.T) {
	fixtureBody, err := os.ReadFile("testdata/cluster_signal_identity_edge.json")
	if err != nil {
		t.Fatalf("read identity fixture: %v", err)
	}
	var fixture struct {
		Profile   string            `json:"profile"`
		Rule      string            `json:"rule"`
		Source    string            `json:"source"`
		Labels    map[string]string `json:"labels"`
		Canonical string            `json:"canonical"`
		Identity  string            `json:"identity"`
	}
	if err := json.Unmarshal(fixtureBody, &fixture); err != nil {
		t.Fatalf("decode identity fixture: %v", err)
	}
	canonical := canonicalSignalIdentityInput(
		fixture.Profile, fixture.Rule, fixture.Source, fixture.Labels,
	)
	if canonical != fixture.Canonical {
		t.Fatalf("canonical signal identity JSON changed:\n got: %s\nwant: %s", canonical, fixture.Canonical)
	}
	if !json.Valid([]byte(canonical)) {
		t.Fatalf("canonical identity is not JSON: %s", canonical)
	}
	for _, character := range []byte(canonical) {
		if character >= 0x80 {
			t.Fatalf("canonical identity is not ASCII: %q", canonical)
		}
	}
	if identity := signalIdentity(
		fixture.Profile, fixture.Rule, fixture.Source, fixture.Labels,
	); identity != fixture.Identity {
		t.Fatalf("signal identity changed: got %q want %q", identity, fixture.Identity)
	}
}

func TestClusterSignalRejectsUnboundedOrInvalidData(t *testing.T) {
	base := ClusterSignal{
		APIVersion: APIVersion, Kind: SignalKind, ClusterID: "42",
		Identity: "signal-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG",
		Profile:  "profile", Rule: "rule", Source: "prometheus", Category: "monitoring",
		Severity: "warning", Status: "active", ObservedAt: "2026-08-06T12:30:00Z",
		Value: 1, Unit: "count", Threshold: 1, Labels: map[string]string{}, Message: "warning",
	}
	tests := map[string]ClusterSignal{
		"severity":   copySignal(base, func(signal *ClusterSignal) { signal.Severity = "emergency" }),
		"status":     copySignal(base, func(signal *ClusterSignal) { signal.Status = "firing" }),
		"non-finite": copySignal(base, func(signal *ClusterSignal) { signal.Value = math.Inf(1) }),
		"labels": copySignal(base, func(signal *ClusterSignal) {
			signal.Labels = map[string]string{"zone": strings.Repeat("x", MaxSignalLabelValue+1)}
		}),
		"message": copySignal(base, func(signal *ClusterSignal) {
			signal.Message = strings.Repeat("x", MaxMessageBytes+1)
		}),
	}
	for name, signal := range tests {
		t.Run(name, func(t *testing.T) {
			if err := signal.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestNewClusterSignalRejectsOversizedAllowedLabel(t *testing.T) {
	profile, err := ParseProfile(validProfile(), allowed("monitoring"))
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	_, err = NewClusterSignal(
		"42", profile, profile.Spec.Rules[0],
		Sample{Value: 91, Labels: map[string]string{"zone": strings.Repeat("x", MaxSignalLabelValue+1)}},
		"active", "critical", 90, time.Now(), "",
	)
	if err == nil {
		t.Fatal("expected oversized label rejection")
	}
}

func copySignal(signal ClusterSignal, mutate func(*ClusterSignal)) ClusterSignal {
	mutate(&signal)
	return signal
}
