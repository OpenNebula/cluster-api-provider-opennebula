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
	Endpoint           string
	OpenNebulaEndpoint string
	Key                []byte
	AuthFile           string
	HTTPTimeout        time.Duration
	PodPollInterval    time.Duration
	HealthAddress      string
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Endpoint:           strings.TrimSpace(os.Getenv("MONITOR_ENDPOINT")),
		OpenNebulaEndpoint: strings.TrimSpace(os.Getenv("ONE_XMLRPC")),
		AuthFile:           strings.TrimSpace(os.Getenv("MONITOR_AUTH_FILE")),
		HealthAddress:      strings.TrimSpace(os.Getenv("MONITOR_HEALTH_ADDRESS")),
	}
	if c.Endpoint == "" {
		return Config{}, fmt.Errorf("MONITOR_ENDPOINT is required")
	}
	if c.OpenNebulaEndpoint == "" {
		return Config{}, fmt.Errorf("ONE_XMLRPC is required")
	}
	if c.AuthFile == "" {
		return Config{}, fmt.Errorf("MONITOR_AUTH_FILE is required")
	}
	if c.HealthAddress == "" {
		return Config{}, fmt.Errorf("MONITOR_HEALTH_ADDRESS is required")
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return Config{}, fmt.Errorf("MONITOR_ENDPOINT must be an absolute HTTP or HTTPS URL")
	}
	openNebulaEndpoint, err := url.Parse(c.OpenNebulaEndpoint)
	if err != nil || openNebulaEndpoint.Host == "" || (openNebulaEndpoint.Scheme != "http" && openNebulaEndpoint.Scheme != "https") {
		return Config{}, fmt.Errorf("ONE_XMLRPC must be an absolute HTTP or HTTPS URL")
	}

	encodedKey := strings.TrimSpace(os.Getenv("MONITOR_KEY"))
	c.Key, err = base64.StdEncoding.Strict().DecodeString(encodedKey)
	if err != nil || len(c.Key) != 32 {
		return Config{}, fmt.Errorf("MONITOR_KEY must be Base64-encoded and decode to exactly 32 bytes")
	}
	timeout := strings.TrimSpace(os.Getenv("MONITOR_HTTP_TIMEOUT"))
	c.HTTPTimeout, err = time.ParseDuration(timeout)
	if err != nil || c.HTTPTimeout <= 0 {
		return Config{}, fmt.Errorf("MONITOR_HTTP_TIMEOUT must be a positive duration: %q", timeout)
	}
	podPollInterval := strings.TrimSpace(os.Getenv("MONITOR_POD_POLL_INTERVAL"))
	c.PodPollInterval, err = time.ParseDuration(podPollInterval)
	if err != nil || c.PodPollInterval <= 0 {
		return Config{}, fmt.Errorf("MONITOR_POD_POLL_INTERVAL must be a positive duration: %q", podPollInterval)
	}
	return c, nil
}
