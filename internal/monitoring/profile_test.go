/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitoring

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseProfile(t *testing.T) {
	profile, err := ParseProfile(validProfile(), allowed("monitoring"))
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if profile.Metadata.Name != "cluster-health" || len(profile.Spec.Sources) != 1 || len(profile.Spec.Rules) != 1 {
		t.Fatalf("unexpected profile: %#v", profile)
	}
	if profile.Spec.Sources[0].Service.Port != "9090" {
		t.Fatalf("numeric service port was not normalized: %q", profile.Spec.Sources[0].Service.Port)
	}
}

func TestParseProfileRejectsUnknownFields(t *testing.T) {
	document := strings.Replace(string(validProfile()), "  rules:", "  unexpected: true\n  rules:", 1)
	_, err := ParseProfile([]byte(document), allowed("monitoring"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected strict unknown-field error, got %v", err)
	}
}

func TestParseProfileRejectsAmbiguousYAML(t *testing.T) {
	tests := map[string]string{
		"duplicate key":      strings.Replace(string(validProfile()), "kind: MonitoringProfile", "kind: MonitoringProfile\nkind: MonitoringProfile", 1),
		"multiple documents": string(validProfile()) + "---\napiVersion: monitoring.oneks.opennebula.io/v1alpha1\n",
		"alias":              strings.Replace(string(validProfile()), "name: cluster-health", "name: &name cluster-health\n  alias: *name", 1),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProfile([]byte(document), allowed("monitoring")); err == nil {
				t.Fatal("expected ambiguous YAML rejection")
			}
		})
	}
}

func TestParseProfileRejectsUnallowedServiceNamespace(t *testing.T) {
	_, err := ParseProfile(validProfile(), allowed("other"))
	if err == nil || !strings.Contains(err.Error(), "is not allowed") {
		t.Fatalf("expected namespace allowlist error, got %v", err)
	}
}

func TestParseProfileRejectsExternalURLAndSecretFields(t *testing.T) {
	document := strings.Replace(string(validProfile()), "      service:", "      url: https://prometheus.example\n      secretRef: credentials\n      service:", 1)
	_, err := ParseProfile([]byte(document), allowed("monitoring"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected external endpoint fields to be rejected, got %v", err)
	}
}

func TestParseProfileRejectsMalformedThresholdsAndTemplates(t *testing.T) {
	tests := map[string]string{
		"threshold order":   strings.Replace(string(validProfile()), "critical: 90", "critical: 70", 1),
		"missing threshold": strings.Replace(string(validProfile()), "      recovery: 75\n", "", 1),
		"template field":    strings.Replace(string(validProfile()), "{{value}}", "{{query}}", 1),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProfile([]byte(document), allowed("monitoring")); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestStoreRejectsDuplicateProfileNames(t *testing.T) {
	store := NewStore([]string{"monitoring"})
	if err := store.Upsert("kube-system/first", validProfile()); err != nil {
		t.Fatalf("load first profile: %v", err)
	}
	if err := store.Upsert("kube-system/second", validProfile()); err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Fatalf("expected duplicate profile-name rejection, got %v", err)
	}
}

func TestParseProfileEnforcesPayloadAndCardinalityLimits(t *testing.T) {
	oversized := make([]byte, MaxProfileDocumentBytes+1)
	if _, err := ParseProfile(oversized, allowed("monitoring")); err == nil {
		t.Fatal("expected oversized document rejection")
	}

	rules := make([]string, 0, MaxRules+1)
	for i := 0; i <= MaxRules; i++ {
		rules = append(rules, fmt.Sprintf(`
    - id: rule-%d
      source: prometheus
      query: up
      unit: count
      comparison: GreaterThan
      warning: 1
      critical: 2
      recovery: 0
      labels:
        allow: []
      message: value {{value}}`, i))
	}
	document := strings.Replace(string(validProfile()), profileRuleBlock(), strings.Join(rules, ""), 1)
	_, err := ParseProfile([]byte(document), allowed("monitoring"))
	if err == nil || !strings.Contains(err.Error(), "rules must contain") {
		t.Fatalf("expected rule cardinality rejection, got %v", err)
	}
}

func TestStoreRetainsLastValidProfileAfterInvalidUpdate(t *testing.T) {
	store := NewStore([]string{"monitoring"})
	if err := store.Upsert("kube-system/profile", validProfile()); err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if err := store.Upsert("kube-system/profile", []byte("not: a profile")); err == nil {
		t.Fatal("expected invalid update rejection")
	}
	profiles := store.List()
	if len(profiles) != 1 || profiles[0].Metadata.Name != "cluster-health" {
		t.Fatalf("last valid profile was not retained: %#v", profiles)
	}
	store.Delete("kube-system/profile")
	if len(store.List()) != 0 {
		t.Fatal("profile deletion was not applied")
	}
}

func allowed(namespaces ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		result[namespace] = struct{}{}
	}
	return result
}

func validProfile() []byte {
	return []byte(`apiVersion: monitoring.oneks.opennebula.io/v1alpha1
kind: MonitoringProfile
metadata:
  name: cluster-health
spec:
  evaluationInterval: 1m
  sources:
    - id: prometheus
      type: prometheus
      service:
        namespace: monitoring
        name: prometheus
        port: 9090
        path: /api/v1/query
      timeout: 10s
  rules:` + profileRuleBlock() + `
`)
}

func profileRuleBlock() string {
	return `
    - id: node-memory
      source: prometheus
      query: node_memory_pressure
      unit: percent
      comparison: GreaterThanOrEqual
      warning: 80
      critical: 90
      recovery: 75
      labels:
        allow:
          - zone
      message: "{{profile}}/{{rule}} is {{severity}} at {{value}} {{threshold}}"`
}
