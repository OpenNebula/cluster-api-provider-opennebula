/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPSender(t *testing.T) {
	var authorization, monitorToken, endpoint string
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		authorization = request.Header.Get("Authorization")
		monitorToken = request.Header.Get("X-OneKS-Monitor-Token")
		endpoint = request.URL.String()
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})

	sender := NewHTTPSender(Config{Endpoint: "https://monitor.example/api/v1", ClusterID: "42", Token: "cluster-token", Auth: "oneadmin:secret", HTTPTimeout: time.Second})
	sender.client.Transport = transport
	if err := sender.Send(context.Background(), Report{Kind: "Node", Name: "worker-1", ResourceVersion: "42", Event: "Updated"}); err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}
	if monitorToken != "cluster-token" || authorization == "Bearer cluster-token" {
		t.Fatalf("unexpected authentication headers: token=%q authorization=%q", monitorToken, authorization)
	}
	if endpoint != "https://monitor.example/api/v1/clusters/42/status" {
		t.Fatalf("unexpected endpoint: %q", endpoint)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
