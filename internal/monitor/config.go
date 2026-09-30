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
	"strconv"
	"strings"
	"time"
)

// Config contains the complete runtime contract between the in-cluster
// monitor, Kubernetes, OpenNebula and the OneKS callback API.
type Config struct {
	// Endpoint is MONITOR_ENDPOINT, the absolute HTTP(S) base URL of the OneKS
	// callback API. A trailing slash is ignored when callback paths are built.
	Endpoint string
	// ClusterID is MONITOR_CLUSTER_ID, the non-negative OneKS cluster identifier
	// used in callback paths and to reject OpenNebula VMs from other clusters.
	ClusterID int
	// OpenNebulaEndpoint is ONE_XMLRPC, the absolute HTTP(S) URL of the
	// OpenNebula XML-RPC API used to resolve VM placement.
	OpenNebulaEndpoint string
	// Key is MONITOR_KEY decoded from Base64. It contains exactly 32 bytes and
	// is used as the AES-256-GCM key for every OneKS payload.
	Key []byte
	// AuthFile is MONITOR_AUTH_FILE. The file must contain
	// "username:password-or-token" and is reread for every OneKS callback so a
	// mounted Secret can rotate without restarting the monitor.
	AuthFile string
	// HTTPTimeout is MONITOR_HTTP_TIMEOUT and bounds each OneKS HTTP request.
	HTTPTimeout time.Duration
	// PodPollInterval is MONITOR_POD_POLL_INTERVAL and controls how often the
	// authoritative OneKS pod snapshot is rebuilt.
	PodPollInterval time.Duration
	// ResourceConfigNamespace is MONITOR_RESOURCE_CONFIG_NAMESPACE and defaults
	// to kube-system. It contains the resource-observation ConfigMap.
	ResourceConfigNamespace string
	// ResourceConfigName is MONITOR_RESOURCE_CONFIG_NAME and defaults to
	// capone-resource-monitor.
	ResourceConfigName string
	// ResourcePollInterval is MONITOR_RESOURCE_POLL_INTERVAL and controls both
	// Pending-pod and configured-resource observation polling.
	ResourcePollInterval time.Duration
	// HealthAddress is MONITOR_HEALTH_ADDRESS, the listen address that exposes
	// /healthz and /readyz.
	HealthAddress string
}

// ConfigFromEnv reads and validates all monitor environment variables. It
// returns an error before any Kubernetes, OpenNebula or OneKS client is built,
// so invalid cross-system configuration fails the container at startup.
func ConfigFromEnv() (Config, error) {
	clusterID := strings.TrimSpace(os.Getenv("MONITOR_CLUSTER_ID"))
	c := Config{
		Endpoint:                strings.TrimSpace(os.Getenv("MONITOR_ENDPOINT")),
		OpenNebulaEndpoint:      strings.TrimSpace(os.Getenv("ONE_XMLRPC")),
		AuthFile:                strings.TrimSpace(os.Getenv("MONITOR_AUTH_FILE")),
		ResourceConfigNamespace: strings.TrimSpace(os.Getenv("MONITOR_RESOURCE_CONFIG_NAMESPACE")),
		ResourceConfigName:      strings.TrimSpace(os.Getenv("MONITOR_RESOURCE_CONFIG_NAME")),
		HealthAddress:           strings.TrimSpace(os.Getenv("MONITOR_HEALTH_ADDRESS")),
	}
	if c.Endpoint == "" {
		return Config{}, fmt.Errorf("MONITOR_ENDPOINT is required")
	}
	if clusterID == "" {
		return Config{}, fmt.Errorf("MONITOR_CLUSTER_ID is required")
	}
	parsedClusterID, err := strconv.Atoi(clusterID)
	if err != nil || parsedClusterID < 0 {
		return Config{}, fmt.Errorf("MONITOR_CLUSTER_ID must be a non-negative integer: %q", clusterID)
	}
	c.ClusterID = parsedClusterID
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
	pollInterval := strings.TrimSpace(os.Getenv("MONITOR_RESOURCE_POLL_INTERVAL"))
	if pollInterval == "" {
		c.ResourcePollInterval = 10 * time.Second
	} else {
		c.ResourcePollInterval, err = time.ParseDuration(pollInterval)
		if err != nil || c.ResourcePollInterval <= 0 {
			return Config{}, fmt.Errorf("MONITOR_RESOURCE_POLL_INTERVAL must be a positive duration: %q", pollInterval)
		}
	}
	if c.ResourceConfigNamespace == "" {
		c.ResourceConfigNamespace = "kube-system"
	}
	if c.ResourceConfigName == "" {
		c.ResourceConfigName = "capone-resource-monitor"
	}
	return c, nil
}
