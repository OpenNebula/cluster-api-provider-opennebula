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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/resourceobserver"
)

func TestExistingNodeReportJSONIsStable(t *testing.T) {
	report := Report{
		Kind: "Node", Name: "worker-1", UID: "node-uid",
		ResourceVersion: "42", Event: "Updated",
		Status: map[string]any{
			"providerID": "one://317", "ready": true,
		},
	}

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	want := `{"kind":"Node","name":"worker-1","uid":"node-uid","resourceVersion":"42","event":"Updated","status":{"providerID":"one://317","ready":true}}`
	if string(body) != want {
		t.Fatalf("Node callback changed:\n got: %s\nwant: %s", body, want)
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

func TestHTTPEncryptedSenderEncryptsResourceValue(t *testing.T) {
	var body string
	sender := newTestEncryptedSender(t, "http://monitor.example/api/v1", bytes.Repeat([]byte{0x42}, 32))
	sender.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		payload, _ := io.ReadAll(request.Body)
		body = string(payload)
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	value := resourceobserver.ResourceValue{
		Kind: "ResourceValue", ID: "deployment-ready",
		APIVersion: "apps/v1", Resource: "deployments", Namespace: "payments",
		Name: "api", Path: "status.readyReplicas", Value: int64(3),
		ObservedAt: "2026-08-28T12:00:00Z",
	}
	if err := sender.Send(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(decryptEnvelope(t, body, bytes.Repeat([]byte{0x42}, 32))), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["kind"] != "ResourceValue" || decoded["id"] != "deployment-ready" || decoded["value"] != float64(3) {
		t.Fatalf("unexpected decrypted resource value: %#v", decoded)
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
	authFile := writeMonitorAuthFile(t, "oneadmin:secret\n")
	sender, err := NewHTTPEncryptedSender(Config{
		Endpoint: endpoint, ClusterID: "42", Key: key,
		AuthFile: authFile, HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	sender.random = bytes.NewReader(bytes.Repeat([]byte{0x11}, sender.aead.NonceSize()))
	return sender
}

func TestHTTPEncryptedSenderRejectsMalformedBasicAuth(t *testing.T) {
	for name, credential := range map[string]string{
		"missing colon":  "invalid",
		"empty username": ":token",
		"empty password": "oneadmin:",
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			sender, err := NewHTTPEncryptedSender(Config{
				Endpoint: "http://monitor.example/api/v1", ClusterID: "42",
				Key: bytes.Repeat([]byte{0x42}, 32), AuthFile: writeMonitorAuthFile(t, credential),
				HTTPTimeout: time.Second,
			})
			if err != nil {
				t.Fatalf("create sender: %v", err)
			}
			sender.client.Transport = roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: http.StatusNoContent,
					Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			err = sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1"})
			if err == nil || !strings.Contains(err.Error(), "must have the form username:password-or-token") {
				t.Fatalf("expected malformed Basic Auth rejection, got %v", err)
			}
			if strings.Contains(err.Error(), credential) {
				t.Fatalf("authentication error exposed credential contents: %v", err)
			}
			if requests != 0 {
				t.Fatalf("malformed authentication sent %d HTTP requests", requests)
			}
		})
	}
}

func TestHTTPEncryptedSenderPreservesAdditionalPasswordColons(t *testing.T) {
	var authorization, user, password string
	sender, err := NewHTTPEncryptedSender(Config{
		Endpoint: "http://monitor.example/api/v1", ClusterID: "42", Key: bytes.Repeat([]byte{0x42}, 32),
		AuthFile: writeMonitorAuthFile(t, "username:token:part:two\n"), HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	sender.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		authorization = request.Header.Get("Authorization")
		var ok bool
		user, password, ok = request.BasicAuth()
		if !ok {
			t.Error("request did not contain HTTP Basic authentication")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	if err := sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1"}); err != nil {
		t.Fatalf("send report: %v", err)
	}
	if user != "username" || password != "token:part:two" {
		t.Fatalf("HTTP Basic authentication = %q/%q", user, password)
	}
	wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("username:token:part:two"))
	if authorization != wantAuthorization {
		t.Fatalf("Authorization header = %q, want %q", authorization, wantAuthorization)
	}
}

func TestHTTPEncryptedSenderDoesNotSendWhenAuthFileDisappears(t *testing.T) {
	authFile := writeMonitorAuthFile(t, "oneadmin:token\n")
	sender, err := NewHTTPEncryptedSender(Config{
		Endpoint: "http://monitor.example/api/v1", ClusterID: "42",
		Key: bytes.Repeat([]byte{0x42}, 32), AuthFile: authFile, HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	requests := 0
	sender.client.Transport = roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusNoContent,
			Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := os.Remove(authFile); err != nil {
		t.Fatalf("remove authentication file: %v", err)
	}

	err = sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1"})
	if err == nil || !strings.Contains(err.Error(), "resolve monitor authentication") {
		t.Fatalf("expected authentication resolution error, got %v", err)
	}
	if requests != 0 {
		t.Fatalf("authentication read failure sent %d HTTP requests", requests)
	}
}

func TestHTTPEncryptedSenderRejectsMalformedRotatedCredentialWithoutSecondRequest(t *testing.T) {
	authFile := writeMonitorAuthFile(t, "oneadmin:first-token\n")
	sender, err := NewHTTPEncryptedSender(Config{
		Endpoint: "http://monitor.example/api/v1", ClusterID: "42",
		Key: bytes.Repeat([]byte{0x42}, 32), AuthFile: authFile, HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	requests := 0
	sender.client.Transport = roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})

	if err := sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1"}); err != nil {
		t.Fatalf("send with initial credential: %v", err)
	}
	if err := os.WriteFile(authFile, []byte("malformed-rotated-credential\n"), 0o600); err != nil {
		t.Fatalf("rotate authentication file: %v", err)
	}
	err = sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-2"})
	if err == nil || !strings.Contains(err.Error(), "must have the form username:password-or-token") {
		t.Fatalf("expected malformed rotated credential rejection, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("malformed rotated credential resulted in %d requests, want 1", requests)
	}
}

func TestHTTPEncryptedSenderReadsRotatedAuthenticationFileForEveryRequest(t *testing.T) {
	authFile := writeMonitorAuthFile(t, "first-user:first-token\n")
	sender, err := NewHTTPEncryptedSender(Config{
		Endpoint: "http://monitor.example/api/v1", ClusterID: "42",
		Key: bytes.Repeat([]byte{0x42}, 32), AuthFile: authFile, HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}

	var credentials [][2]string
	sender.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		user, password, ok := request.BasicAuth()
		if !ok {
			t.Fatal("request did not contain HTTP Basic authentication")
		}
		credentials = append(credentials, [2]string{user, password})
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})

	if err := sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1"}); err != nil {
		t.Fatalf("send with initial credential: %v", err)
	}
	if err := os.WriteFile(authFile, []byte("second-user:second-token\n"), 0o600); err != nil {
		t.Fatalf("rotate authentication file: %v", err)
	}
	if err := sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-2"}); err != nil {
		t.Fatalf("send with rotated credential: %v", err)
	}

	want := [][2]string{{"first-user", "first-token"}, {"second-user", "second-token"}}
	if len(credentials) != len(want) || credentials[0] != want[0] || credentials[1] != want[1] {
		t.Fatalf("unexpected per-request credentials: %#v", credentials)
	}
}

func TestHTTPEncryptedSenderFailsStartupForUnreadableOrEmptyAuthFile(t *testing.T) {
	for name, authFile := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "missing-ONE_AUTH"),
		"empty":   writeMonitorAuthFile(t, ""),
		"blank":   writeMonitorAuthFile(t, " \t\n"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewHTTPEncryptedSender(Config{
				Endpoint: "http://monitor.example/api/v1", ClusterID: "42",
				Key: bytes.Repeat([]byte{0x42}, 32), AuthFile: authFile, HTTPTimeout: time.Second,
			})
			if err == nil {
				t.Fatal("expected sender construction to reject authentication file")
			}
		})
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
