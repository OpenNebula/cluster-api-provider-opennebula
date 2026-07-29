/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const defaultChartAnnotation = "oneks.opennebula.io/chart-id"

type Config struct {
	Endpoint        string
	ClusterID       string
	Token           string
	Auth            string
	ChartAnnotation string
	HTTPTimeout     time.Duration
	ResyncPeriod    time.Duration
	ReconcilePeriod time.Duration
	HealthAddress   string
	KubeSystemNS    string
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Endpoint:        strings.TrimSpace(os.Getenv("MONITOR_ENDPOINT")),
		ClusterID:       strings.TrimSpace(os.Getenv("MONITOR_CLUSTER_ID")),
		Token:           strings.TrimSpace(os.Getenv("MONITOR_TOKEN")),
		Auth:            strings.TrimSpace(os.Getenv("MONITOR_AUTH")),
		ChartAnnotation: envOrDefault("MONITOR_CHART_ANNOTATION", defaultChartAnnotation),
		HealthAddress:   envOrDefault("MONITOR_HEALTH_ADDRESS", ":8081"),
		KubeSystemNS:    envOrDefault("MONITOR_CHART_NAMESPACE", "kube-system"),
	}
	if c.Endpoint == "" {
		return Config{}, fmt.Errorf("MONITOR_ENDPOINT is required")
	}
	if c.ClusterID == "" {
		return Config{}, fmt.Errorf("MONITOR_CLUSTER_ID is required")
	}
	if c.Token == "" {
		return Config{}, fmt.Errorf("MONITOR_TOKEN is required")
	}
	if c.Auth == "" {
		return Config{}, fmt.Errorf("MONITOR_AUTH is required")
	}
	var err error
	if c.HTTPTimeout, err = durationEnv("MONITOR_HTTP_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if c.ResyncPeriod, err = durationEnv("MONITOR_RESYNC_PERIOD", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if c.ReconcilePeriod, err = durationEnv("MONITOR_RECONCILE_PERIOD", 30*time.Second); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) watchesChart(annotations map[string]string) (string, bool) {
	id, ok := annotations[c.ChartAnnotation]
	if !ok || strings.TrimSpace(id) == "" {
		return "", false
	}
	return id, true
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
