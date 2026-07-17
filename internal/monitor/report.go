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
	"time"
)

type Report struct {
	ClusterID              string         `json:"clusterId"`
	Kind                   string         `json:"kind"`
	Namespace              string         `json:"namespace,omitempty"`
	Name                   string         `json:"name"`
	UID                    string         `json:"uid,omitempty"`
	ResourceVersion        string         `json:"resourceVersion,omitempty"`
	RelatedResourceVersion string         `json:"relatedResourceVersion,omitempty"`
	Event                  string         `json:"event"`
	ObservedAt             time.Time      `json:"observedAt"`
	Status                 map[string]any `json:"status,omitempty"`
}

type Sender interface {
	Send(context.Context, Report) error
}

type HTTPSender struct {
	endpoint string
	client   *http.Client
}

func NewHTTPSender(config Config) *HTTPSender {
	return &HTTPSender{
		endpoint: config.Endpoint,
		client:   &http.Client{Timeout: config.HTTPTimeout},
	}
}

func (s *HTTPSender) Send(ctx context.Context, report Report) error {
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "capone-cluster-monitor")
	req.Header.Set("Idempotency-Key", report.ClusterID+"/"+report.Kind+"/"+report.UID+"/"+report.ResourceVersion+"/"+report.RelatedResourceVersion+"/"+report.Event)
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
