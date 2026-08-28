/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"os"
	"strings"
	"testing"
)

func TestMonitorDockerfileIncludesMonitorPackageBeforeBuild(t *testing.T) {
	dockerfile, err := os.ReadFile("../../Dockerfile.monitor")
	if err != nil {
		t.Fatalf("read Dockerfile.monitor: %v", err)
	}
	content := string(dockerfile)
	copySteps := []string{
		"COPY internal/monitor/ internal/monitor/",
		"COPY internal/resourceobserver/ internal/resourceobserver/",
	}
	buildStep := "go build -a -o monitor ./cmd/monitor"
	buildIndex := strings.Index(content, buildStep)
	for _, copyStep := range copySteps {
		copyIndex := strings.Index(content, copyStep)
		if copyIndex < 0 {
			t.Fatalf("Dockerfile.monitor does not include %q", copyStep)
		}
		if buildIndex >= 0 && copyIndex > buildIndex {
			t.Fatalf("Dockerfile.monitor copies package after building the monitor: %q", copyStep)
		}
	}
	if buildIndex < 0 {
		t.Fatalf("Dockerfile.monitor does not include monitor build step %q", buildStep)
	}
}
