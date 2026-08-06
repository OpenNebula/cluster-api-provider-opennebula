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

func TestMonitorDockerfileIncludesMonitoringPackageBeforeBuild(t *testing.T) {
	dockerfile, err := os.ReadFile("../../Dockerfile.monitor")
	if err != nil {
		t.Fatalf("read Dockerfile.monitor: %v", err)
	}
	content := string(dockerfile)
	copyStep := "COPY internal/monitoring/ internal/monitoring/"
	buildStep := "go build -a -o monitor ./cmd/monitor"
	copyIndex := strings.Index(content, copyStep)
	buildIndex := strings.Index(content, buildStep)
	if copyIndex < 0 {
		t.Fatalf("Dockerfile.monitor does not include %q", copyStep)
	}
	if buildIndex < 0 {
		t.Fatalf("Dockerfile.monitor does not include monitor build step %q", buildStep)
	}
	if copyIndex > buildIndex {
		t.Fatal("Dockerfile.monitor copies internal/monitoring after building the monitor")
	}
}
