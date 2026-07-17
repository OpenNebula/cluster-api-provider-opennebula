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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPSender(t *testing.T) {
	var idempotencyKey string
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		idempotencyKey = request.Header.Get("Idempotency-Key")
		return &http.Response{
			StatusCode: http.StatusNoContent, Status: "204 No Content",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header),
		}, nil
	})

	sender := NewHTTPSender(Config{Endpoint: "https://monitor.example/v1/status", HTTPTimeout: time.Second})
	sender.client.Transport = transport
	err := sender.Send(context.Background(), Report{
		ClusterID: "cluster-1", Kind: "Node", Name: "worker-1", UID: "uid-1", ResourceVersion: "42", Event: "Updated",
	})
	if err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}
	if idempotencyKey != "cluster-1/Node/uid-1/42//Updated" {
		t.Fatalf("unexpected Idempotency-Key: %q", idempotencyKey)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
