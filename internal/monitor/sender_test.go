/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
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
)

func TestOneKSClientEncryptsAndSendsMonitorPayloads(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	authFile := filepath.Join(t.TempDir(), "ONE_AUTH")
	if err := os.WriteFile(authFile, []byte("oneadmin:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewOneKSClient(Config{
		Endpoint: "http://oneks.example/api/v1", Key: key,
		AuthFile: authFile, HTTPTimeout: time.Second, ClusterID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	var path, plaintext string
	client.client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		user, password, ok := r.BasicAuth()
		if !ok || user != "oneadmin" || password != "secret" {
			t.Fatalf("unexpected authentication: %q/%q", user, password)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		plaintext = decryptEnvelope(t, body, key)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     "204 No Content",
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	if err := client.PublishNodeReady(context.Background(), 16, NodeReadyEvent{
		Event: "node_ready", Payload: NodeReadyPayload{VMID: 2, Ready: true},
	}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/clusters/42/nodegroups/16/events" {
		t.Fatalf("request path = %q", path)
	}
	if plaintext != `{"event":"node_ready","payload":{"vm_id":2,"ready":true}}` {
		t.Fatalf("plaintext = %s", plaintext)
	}

	if err := client.ReplacePods(context.Background(), PodSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/clusters/42/pods" || plaintext != `{}` {
		t.Fatalf("pods request path=%q plaintext=%s", path, plaintext)
	}
	if err := client.ReplaceObservations(context.Background(), ObservationSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/clusters/42/observations" || plaintext != `[]` {
		t.Fatalf("observations request path=%q plaintext=%s", path, plaintext)
	}
	if err := client.PublishChartEvent(context.Background(), ChartEvent{
		Event: "app_state_changed",
		Payload: ChartEventPayload{
			ReleaseName: "runai", ResourceVersion: json.Number("17"), State: "ready",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/clusters/42/events" ||
		plaintext != `{"event":"app_state_changed","payload":{"release_name":"runai","resource_version":17,"state":"ready"}}` {
		t.Fatalf("chart request path=%q plaintext=%s", path, plaintext)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func decryptEnvelope(t *testing.T, body, key []byte) string {
	t.Helper()
	var envelope encryptedEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(plaintext)
}
