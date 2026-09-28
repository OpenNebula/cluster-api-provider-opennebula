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
	"os"
	"strconv"
	"strings"
)

// Publisher is the monitor-facing interface implemented by the OneKS client.
// Monitor components publish domain data without knowing HTTP paths or encryption
type Publisher interface {
	PublishNodeReady(context.Context, int, NodeReadyEvent) error
	ReplacePods(context.Context, PodSnapshot) error
	ReplaceObservations(context.Context, ObservationSnapshot) error
	PublishChartEvent(context.Context, ChartEvent) error
}

type encryptedEnvelope struct {
	Payload string `json:"payload"`
}

// OneKSClient owns transport details shared by every monitor: endpoint paths,
// authentication, encryption and HTTP response handling
type OneKSClient struct {
	endpoint  string
	clusterID int
	authFile  string
	aead      cipher.AEAD
	client    *http.Client
}

func NewOneKSClient(config Config) (*OneKSClient, error) {
	if _, err := readCredential(config.AuthFile); err != nil {
		return nil, fmt.Errorf("configure monitor authentication: %w", err)
	}
	block, err := aes.NewCipher(config.Key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return &OneKSClient{
		endpoint:  strings.TrimRight(config.Endpoint, "/"),
		clusterID: config.ClusterID,
		authFile:  config.AuthFile,
		aead:      aead,
		client: &http.Client{
			Timeout: config.HTTPTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return fmt.Errorf("callback redirects are disabled")
			},
		},
	}, nil
}

func (c *OneKSClient) PublishNodeReady(ctx context.Context, groupID int, event NodeReadyEvent) error {
	return c.send(ctx, c.clusterPath()+"/nodegroups/"+strconv.Itoa(groupID)+"/events", event)
}

func (c *OneKSClient) ReplacePods(ctx context.Context, snapshot PodSnapshot) error {
	return c.send(ctx, c.clusterPath()+"/pods", snapshot)
}

func (c *OneKSClient) ReplaceObservations(ctx context.Context, snapshot ObservationSnapshot) error {
	return c.send(ctx, c.clusterPath()+"/observations", snapshot)
}

func (c *OneKSClient) PublishChartEvent(ctx context.Context, event ChartEvent) error {
	return c.send(ctx, c.clusterPath()+"/events", event)
}

func (c *OneKSClient) clusterPath() string {
	return "/clusters/" + strconv.Itoa(c.clusterID)
}

func (c *OneKSClient) send(ctx context.Context, path string, payload any) error {
	// Read credentials for every request so a mounted Secret can rotate without
	// restarting the monitor process
	credential, err := readCredential(c.authFile)
	if err != nil {
		return fmt.Errorf("resolve monitor authentication: %w", err)
	}
	user, password, ok := strings.Cut(credential, ":")
	if !ok || user == "" || password == "" {
		return fmt.Errorf("monitor authentication credential must have the form username:password-or-token")
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	// AES-GCM requires a unique nonce per payload. The nonce is public and is
	// prepended to the ciphertext so OneKS can decrypt the envelope
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("generate payload nonce: %w", err)
	}
	sealed := c.aead.Seal(nil, nonce, plaintext, nil)
	body, err := json.Marshal(encryptedEnvelope{
		Payload: base64.StdEncoding.EncodeToString(append(nonce, sealed...)),
	})
	if err != nil {
		return fmt.Errorf("encode encrypted payload: %w", err)
	}
	endpoint := c.endpoint + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create payload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "capone-cluster-monitor")
	req.SetBasicAuth(user, password)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("send payload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("endpoint returned %s: %s", resp.Status, string(message))
	}
	return nil
}

func readCredential(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read monitor authentication file: %w", err)
	}
	credential := strings.TrimSpace(string(contents))
	if credential == "" {
		return "", fmt.Errorf("monitor authentication file is empty")
	}
	return credential, nil
}
