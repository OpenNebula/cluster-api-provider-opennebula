/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitor

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
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

func TestHTTPEncryptedSender(t *testing.T) {
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

	key := bytes.Repeat([]byte{0x42}, 32)
	sender := newTestEncryptedSender(t, "http://monitor.example/api/v1", key)
	sender.client.Transport = transport
	if err := sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1", ResourceVersion: "42", Event: "Updated"}); err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}
	if authorization != "Basic b25lYWRtaW46c2VjcmV0" || monitorToken != "" {
		t.Fatalf("unexpected authentication headers: authorization=%q token=%q", authorization, monitorToken)
	}
	if endpoint != "http://monitor.example/api/v1/clusters/42/status" {
		t.Fatalf("unexpected endpoint: %q", endpoint)
	}
	if method != http.MethodPost || contentType != "application/json" {
		t.Fatalf("unexpected request metadata: method=%q content-type=%q", method, contentType)
	}
	var envelope encryptedEnvelope
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	wantPayload := "ERERERERERERERERyEVAegOooh0e0hfTBqLYt0tUb4VjY/ILjT7iboF7" +
		"gjIRW2JB+gM8vgWrc7Ng6WI/5HgC1DZQ+YetiYw4JfKfrIYg4IcLZnUl" +
		"TIMFCrYuiGHw0Z2kHBza3aQy"
	if envelope.Payload != wantPayload {
		t.Fatalf("encrypted payload changed:\n got: %s\nwant: %s", envelope.Payload, wantPayload)
	}
	wantBody := `{"kind":"Node","name":"worker-1","resourceVersion":"42","event":"Updated"}`
	if decrypted := decryptEnvelope(t, body, key); decrypted != wantBody {
		t.Fatalf("unexpected decrypted body:\n got: %s\nwant: %s", decrypted, wantBody)
	}
}

func TestClusterSignalUsesEncryptedCallbackWire(t *testing.T) {
	var body, authorization, token string
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		body = string(payload)
		authorization = request.Header.Get("Authorization")
		token = request.Header.Get("X-OneKS-Monitor-Token")
		return &http.Response{
			StatusCode: http.StatusNoContent, Status: "204 No Content",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header),
		}, nil
	})
	key := bytes.Repeat([]byte{0x24}, 32)
	sender := newTestEncryptedSender(t, "http://monitor.example/api/v1", key)
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
	if authorization != "Basic b25lYWRtaW46c2VjcmV0" || token != "" {
		t.Fatalf("unexpected callback headers: authorization=%q token=%q", authorization, token)
	}
	want := `{"apiVersion":"monitoring.oneks.opennebula.io/v1alpha1","kind":"ClusterSignal","clusterId":"42","identity":"signal-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG","profile":"health","rule":"availability","source":"prometheus","category":"monitoring","severity":"warning","status":"active","observedAt":"2026-08-06T12:30:00Z","value":0.5,"unit":"ratio","threshold":0.9,"labels":{"zone":"a"},"message":"availability warning"}`
	if decrypted := decryptEnvelope(t, body, key); decrypted != want {
		t.Fatalf("unexpected decrypted callback:\n got: %s\nwant: %s", decrypted, want)
	}
}

func TestHTTPEncryptedSenderRejectsRedirect(t *testing.T) {
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
	sender := newTestEncryptedSender(t, "http://monitor.example/api/v1", bytes.Repeat([]byte{0x42}, 32))
	sender.client.Transport = transport
	err := sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1", Event: "Updated"})
	if err == nil || !strings.Contains(err.Error(), "redirects are disabled") {
		t.Fatalf("expected redirect rejection, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected exactly one HTTP request, got %d", requests)
	}
}

func newTestEncryptedSender(t *testing.T, endpoint string, key []byte) *HTTPEncryptedSender {
	t.Helper()
	sender, err := NewHTTPEncryptedSender(Config{
		Endpoint: endpoint, ClusterID: "42", Key: key,
		Auth: "oneadmin:secret", HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	sender.random = bytes.NewReader(bytes.Repeat([]byte{0x11}, sender.aead.NonceSize()))
	return sender
}

func TestHTTPEncryptedSenderRejectsMalformedBasicAuth(t *testing.T) {
	sender, err := NewHTTPEncryptedSender(Config{
		Endpoint: "http://monitor.example/api/v1", ClusterID: "42",
		Key: bytes.Repeat([]byte{0x42}, 32), Auth: "invalid", HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	err = sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1"})
	if err == nil || !strings.Contains(err.Error(), "MONITOR_AUTH must have the form user:password") {
		t.Fatalf("expected malformed Basic Auth rejection, got %v", err)
	}
}

func decryptEnvelope(t *testing.T, body string, key []byte) string {
	t.Helper()
	var envelope encryptedEnvelope
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("create AES cipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("create AES-GCM: %v", err)
	}
	if len(raw) < aead.NonceSize()+aead.Overhead() {
		t.Fatalf("encrypted payload is too short")
	}
	plaintext, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		t.Fatalf("decrypt payload: %v", err)
	}
	return string(plaintext)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
