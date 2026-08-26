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
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
)

const defaultApplicationNamespace = "oneks-system"

type Config struct {
	Endpoint             string
	ClusterID            string
	Key                  []byte
	AuthFile             string
	HTTPTimeout          time.Duration
	HealthAddress        string
	ApplicationNamespace string
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Endpoint:             strings.TrimSpace(os.Getenv("MONITOR_ENDPOINT")),
		ClusterID:            strings.TrimSpace(os.Getenv("MONITOR_CLUSTER_ID")),
		AuthFile:             strings.TrimSpace(os.Getenv("MONITOR_AUTH_FILE")),
		HealthAddress:        envOrDefault("MONITOR_HEALTH_ADDRESS", ":8081"),
		ApplicationNamespace: envOrDefault("MONITOR_APPLICATION_NAMESPACE", defaultApplicationNamespace),
	}
	if errors := validation.IsDNS1123Label(c.ApplicationNamespace); len(errors) != 0 {
		return Config{}, fmt.Errorf("MONITOR_APPLICATION_NAMESPACE contains invalid namespace %q", c.ApplicationNamespace)
	}
	if c.Endpoint == "" {
		return Config{}, fmt.Errorf("MONITOR_ENDPOINT is required")
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || !endpoint.IsAbs() ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return Config{}, fmt.Errorf("MONITOR_ENDPOINT must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	if c.ClusterID == "" {
		return Config{}, fmt.Errorf("MONITOR_CLUSTER_ID is required")
	}
	if len(c.ClusterID) > 128 || !utf8.ValidString(c.ClusterID) {
		return Config{}, fmt.Errorf("MONITOR_CLUSTER_ID must be valid UTF-8 and at most 128 bytes")
	}
	encodedKey := strings.TrimSpace(os.Getenv("MONITOR_KEY"))
	if encodedKey == "" {
		return Config{}, fmt.Errorf("MONITOR_KEY is required")
	}
	if c.Key, err = base64.StdEncoding.Strict().DecodeString(encodedKey); err != nil || len(c.Key) != 32 {
		return Config{}, fmt.Errorf("MONITOR_KEY must be Base64-encoded and decode to exactly 32 bytes")
	}
	if c.AuthFile == "" {
		return Config{}, fmt.Errorf("MONITOR_AUTH_FILE is required")
	}
	if c.HTTPTimeout, err = durationEnv("MONITOR_HTTP_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	return c, nil
}

func envOrDefault(key, value string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return value
}

func durationEnv(key string, value time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return value, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration: %q", key, raw)
	}
	return parsed, nil
}
