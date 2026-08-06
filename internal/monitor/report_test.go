/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/monitoring"
)

func TestExistingNodeReportJSONIsStable(t *testing.T) {
	report := Report{
		Kind: "Node", Name: "worker-1", UID: "node-uid",
		ResourceVersion: "42", Event: "Updated",
		Status: map[string]any{
			"state": "ready", "providerID": "one://317",
			"readyProviderIDs": []string{"one://317"},
		},
	}

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	want := `{"kind":"Node","name":"worker-1","uid":"node-uid","resourceVersion":"42","event":"Updated","status":{"providerID":"one://317","readyProviderIDs":["one://317"],"state":"ready"}}`
	if string(body) != want {
		t.Fatalf("Node callback changed:\n got: %s\nwant: %s", body, want)
	}
}

func TestExistingHelmChartReportJSONIsStable(t *testing.T) {
	report := Report{
		Kind: "HelmChart", Namespace: "kube-system", Name: "prometheus",
		UID: "chart-uid", ResourceVersion: "19",
		RelatedResourceVersion: "20", Event: "Updated",
		Status: map[string]any{"chartId": "catalogue-id", "status": "deployed"},
	}

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	want := `{"kind":"HelmChart","namespace":"kube-system","name":"prometheus","uid":"chart-uid","resourceVersion":"19","relatedResourceVersion":"20","event":"Updated","status":{"chartId":"catalogue-id","status":"deployed"}}`
	if string(body) != want {
		t.Fatalf("HelmChart callback changed:\n got: %s\nwant: %s", body, want)
	}
}

func TestHTTPSender(t *testing.T) {
	var authorization, monitorToken, endpoint, method, contentType, body string
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		authorization = request.Header.Get("Authorization")
		monitorToken = request.Header.Get("X-OneKS-Monitor-Token")
		endpoint = request.URL.String()
		method = request.Method
		contentType = request.Header.Get("Content-Type")
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		body = string(payload)
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})

	sender := NewHTTPSender(Config{Endpoint: "https://monitor.example/api/v1", ClusterID: "42", Token: "cluster-token", Auth: "oneadmin:secret", HTTPTimeout: time.Second})
	sender.client.Transport = transport
	if err := sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1", ResourceVersion: "42", Event: "Updated"}); err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}
	if authorization != "Basic b25lYWRtaW46c2VjcmV0" {
		t.Fatalf("unexpected Authorization header: %q", authorization)
	}
	if monitorToken != "cluster-token" {
		t.Fatalf("unexpected X-OneKS-Monitor-Token header: %q", monitorToken)
	}
	if endpoint != "https://monitor.example/api/v1/clusters/42/status" {
		t.Fatalf("unexpected endpoint: %q", endpoint)
	}
	if method != http.MethodPost {
		t.Fatalf("unexpected method: %q", method)
	}
	if contentType != "application/json" {
		t.Fatalf("unexpected content type: %q", contentType)
	}
	wantBody := `{"kind":"Node","name":"worker-1","resourceVersion":"42","event":"Updated"}`
	if body != wantBody {
		t.Fatalf("unexpected request body:\n got: %s\nwant: %s", body, wantBody)
	}
}

func TestClusterSignalUsesExistingAuthenticatedCallbackWire(t *testing.T) {
	var body, authorization, token, endpoint string
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		body = string(payload)
		authorization = request.Header.Get("Authorization")
		token = request.Header.Get("X-OneKS-Monitor-Token")
		endpoint = request.URL.String()
		return &http.Response{
			StatusCode: http.StatusNoContent, Status: "204 No Content",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header),
		}, nil
	})
	sender := NewHTTPSender(Config{
		Endpoint: "https://monitor.example/api/v1", ClusterID: "42",
		Token: "cluster-token", Auth: "oneadmin:secret", HTTPTimeout: time.Second,
	})
	sender.client.Transport = transport
	signal := monitoring.ClusterSignal{
		APIVersion: monitoring.APIVersion, Kind: monitoring.SignalKind,
		ClusterID: "42", Identity: "signal-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG",
		Profile: "health", Rule: "availability", Source: "prometheus",
		Category: "monitoring", Severity: "warning", Status: "active",
		ObservedAt: "2026-08-06T12:30:00Z", Value: 0.5, Unit: "ratio",
		Threshold: 0.9, Labels: map[string]string{"zone": "a"}, Message: "availability warning",
	}
	if err := sender.Send(context.Background(), signal); err != nil {
		t.Fatalf("send ClusterSignal: %v", err)
	}
	if authorization != "Basic b25lYWRtaW46c2VjcmV0" || token != "cluster-token" {
		t.Fatalf("callback authentication changed: authorization=%q token=%q", authorization, token)
	}
	if endpoint != "https://monitor.example/api/v1/clusters/42/status" {
		t.Fatalf("callback endpoint changed: %q", endpoint)
	}
	want := `{"apiVersion":"monitoring.oneks.opennebula.io/v1alpha1","kind":"ClusterSignal","clusterId":"42","identity":"signal-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG","profile":"health","rule":"availability","source":"prometheus","category":"monitoring","severity":"warning","status":"active","observedAt":"2026-08-06T12:30:00Z","value":0.5,"unit":"ratio","threshold":0.9,"labels":{"zone":"a"},"message":"availability warning"}`
	if body != want {
		t.Fatalf("unexpected ClusterSignal callback:\n got: %s\nwant: %s", body, want)
	}
}

func TestHTTPSenderRejectsRedirectWithoutForwardingCredentials(t *testing.T) {
	requests := 0
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if requests > 1 {
			t.Fatalf("callback followed redirect to %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Status:     "302 Found",
			Header: http.Header{
				"Location": []string{"http://attacker.invalid/capture"},
			},
			Body: io.NopCloser(strings.NewReader("")),
		}, nil
	})
	sender := NewHTTPSender(Config{
		Endpoint: "https://monitor.example/api/v1", ClusterID: "42",
		Token: "cluster-token", Auth: "oneadmin:secret", HTTPTimeout: time.Second,
	})
	sender.client.Transport = transport
	err := sender.Send(
		context.Background(),
		Report{Kind: "Node", Name: "worker-1", Event: "Updated"},
	)
	if err == nil || !strings.Contains(err.Error(), "redirects are disabled") {
		t.Fatalf("expected redirect rejection, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected exactly one HTTPS request, got %d", requests)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
