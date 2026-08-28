/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Endpoint                string
	ClusterID               string
	Key                     []byte
	AuthFile                string
	HTTPTimeout             time.Duration
	HealthAddress           string
	ApplicationNamespace    string
	ResourceConfigNamespace string
	ResourceConfigName      string
	ResourcePollInterval    time.Duration
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Endpoint:                strings.TrimSpace(os.Getenv("MONITOR_ENDPOINT")),
		ClusterID:               strings.TrimSpace(os.Getenv("MONITOR_CLUSTER_ID")),
		AuthFile:                strings.TrimSpace(os.Getenv("MONITOR_AUTH_FILE")),
		HealthAddress:           strings.TrimSpace(os.Getenv("MONITOR_HEALTH_ADDRESS")),
		ApplicationNamespace:    strings.TrimSpace(os.Getenv("MONITOR_APPLICATION_NAMESPACE")),
		ResourceConfigNamespace: strings.TrimSpace(os.Getenv("MONITOR_RESOURCE_CONFIG_NAMESPACE")),
		ResourceConfigName:      strings.TrimSpace(os.Getenv("MONITOR_RESOURCE_CONFIG_NAME")),
	}
	if c.ResourceConfigNamespace == "" {
		c.ResourceConfigNamespace = "kube-system"
	}
	if c.ResourceConfigName == "" {
		c.ResourceConfigName = "capone-resource-monitor"
	}
	if c.Endpoint == "" {
		return Config{}, fmt.Errorf("MONITOR_ENDPOINT is required")
	}
	if c.ClusterID == "" {
		return Config{}, fmt.Errorf("MONITOR_CLUSTER_ID is required")
	}
	if c.AuthFile == "" {
		return Config{}, fmt.Errorf("MONITOR_AUTH_FILE is required")
	}

	if c.ApplicationNamespace == "" {
		return Config{}, fmt.Errorf("MONITOR_APPLICATION_NAMESPACE is required")
	}

	if c.HealthAddress == "" {
		return Config{}, fmt.Errorf("MONITOR_HEALTH_ADDRESS is required")
	}

	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return Config{}, fmt.Errorf("MONITOR_ENDPOINT must be an absolute HTTP or HTTPS URL")
	}

	encodedKey := strings.TrimSpace(os.Getenv("MONITOR_KEY"))
	if encodedKey == "" {
		return Config{}, fmt.Errorf("MONITOR_KEY is required")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return Config{}, fmt.Errorf(
			"MONITOR_KEY must be Base64-encoded and decode to exactly 32 bytes",
		)
	}
	c.Key = key

	timeout := strings.TrimSpace(os.Getenv("MONITOR_HTTP_TIMEOUT"))
	if timeout == "" {
		return Config{}, fmt.Errorf("MONITOR_HTTP_TIMEOUT is required")
	}
	c.HTTPTimeout, err = time.ParseDuration(timeout)
	if err != nil || c.HTTPTimeout <= 0 {
		return Config{}, fmt.Errorf("MONITOR_HTTP_TIMEOUT must be a positive duration: %q", timeout)
	}

	pollInterval := strings.TrimSpace(os.Getenv("MONITOR_RESOURCE_POLL_INTERVAL"))
	if pollInterval == "" {
		pollInterval = "30s"
	}
	c.ResourcePollInterval, err = time.ParseDuration(pollInterval)
	if err != nil || c.ResourcePollInterval <= 0 {
		return Config{}, fmt.Errorf("MONITOR_RESOURCE_POLL_INTERVAL must be a positive duration: %q", pollInterval)
	}

	return c, nil
}
