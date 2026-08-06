/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/monitor"
)

func TestHealthHandlerPreservesHealthAndReadinessEndpoints(t *testing.T) {
	watcher := &monitor.Monitor{}
	handler := healthHandler(watcher)

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("unexpected health response: %d %q", health.Code, health.Body.String())
	}

	notReady := httptest.NewRecorder()
	handler.ServeHTTP(notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected readiness failure before sync, got %d", notReady.Code)
	}

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusNotFound {
		t.Fatalf("health listener exposed metrics with status %d", metrics.Code)
	}
}
