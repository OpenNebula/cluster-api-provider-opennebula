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

func TestHTTPEncryptedSenderSendsNodeEvent(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	var path, plaintext string
	authFile := filepath.Join(t.TempDir(), "ONE_AUTH")
	if err := os.WriteFile(authFile, []byte("oneadmin:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sender, err := NewHTTPEncryptedSender(Config{
		Endpoint: "http://oneks.example/api/v1", Key: key,
		AuthFile: authFile, HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender.client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
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
	if err := sender.Send(context.Background(), NodeGroupEventDestination{ClusterID: 42, GroupID: 16}, Event{
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
}

func TestClusterObservationsDestinationPath(t *testing.T) {
	if got := (ClusterObservationsDestination{ClusterID: "42"}).path(); got != "/clusters/42/observations" {
		t.Fatalf("destination path = %q", got)
	}
}

func TestClusterPodsDestinationPath(t *testing.T) {
	if got := (ClusterPodsDestination{ClusterID: 42}).path(); got != "/clusters/42/pods" {
		t.Fatalf("destination path = %q", got)
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
