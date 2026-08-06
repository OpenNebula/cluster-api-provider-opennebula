/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Report struct {
	Kind                   string         `json:"kind"`
	Namespace              string         `json:"namespace,omitempty"`
	Name                   string         `json:"name"`
	UID                    string         `json:"uid,omitempty"`
	ResourceVersion        string         `json:"resourceVersion,omitempty"`
	RelatedResourceVersion string         `json:"relatedResourceVersion,omitempty"`
	Event                  string         `json:"event"`
	Status                 map[string]any `json:"status,omitempty"`
}

func (Report) CallbackKind() string { return "resource-status" }

type CallbackPayload interface {
	CallbackKind() string
}

type Sender interface {
	Send(context.Context, CallbackPayload) error
}

type HTTPSender struct {
	endpoint string
	token    string
	auth     string
	client   *http.Client
}

func NewHTTPSender(config Config) *HTTPSender {
	return &HTTPSender{
		endpoint: strings.TrimRight(config.Endpoint, "/") + "/clusters/" +
			url.PathEscape(config.ClusterID) + "/status",
		token: config.Token,
		auth:  config.Auth,
		client: &http.Client{
			Timeout: config.HTTPTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return fmt.Errorf("callback redirects are disabled")
			},
		},
	}
}

func (s *HTTPSender) Send(ctx context.Context, payload CallbackPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "capone-cluster-monitor")
	req.Header.Set("X-OneKS-Monitor-Token", s.token)
	user, password, ok := strings.Cut(s.auth, ":")
	if !ok || user == "" || password == "" {
		return fmt.Errorf("MONITOR_AUTH must have the form user:password")
	}
	req.SetBasicAuth(user, password)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("send report: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("endpoint returned %s: %s", resp.Status, string(message))
	}
	return nil
}
