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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
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

type encryptedEnvelope struct {
	Payload string `json:"payload"`
}

type HTTPEncryptedSender struct {
	endpoint string
	auth     string
	aead     cipher.AEAD
	random   io.Reader
	client   *http.Client
}

func NewHTTPEncryptedSender(config Config) (*HTTPEncryptedSender, error) {
	block, err := aes.NewCipher(config.Key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return &HTTPEncryptedSender{
		endpoint: strings.TrimRight(config.Endpoint, "/") + "/clusters/" +
			url.PathEscape(config.ClusterID) + "/status",
		auth:   config.Auth,
		aead:   aead,
		random: rand.Reader,
		client: &http.Client{
			Timeout: config.HTTPTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return fmt.Errorf("callback redirects are disabled")
			},
		},
	}, nil
}

func (s *HTTPEncryptedSender) Send(ctx context.Context, payload CallbackPayload) error {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return fmt.Errorf("generate report nonce: %w", err)
	}
	sealed := s.aead.Seal(nil, nonce, plaintext, nil)
	wirePayload := make([]byte, 0, len(nonce)+len(sealed))
	wirePayload = append(wirePayload, nonce...)
	wirePayload = append(wirePayload, sealed...)
	body, err := json.Marshal(encryptedEnvelope{
		Payload: base64.StdEncoding.EncodeToString(wirePayload),
	})
	if err != nil {
		return fmt.Errorf("encode encrypted report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "capone-cluster-monitor")
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
