/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestApplicationControllerReleaseGraphDryRun(t *testing.T) {
	releaseCommand := exec.Command(
		"make", "-n", "application-controller-release",
		"APPLICATION_CONTROLLER_VERSION:=9.8.7",
		"HELM:=/bin/true",
		"KUSTOMIZE:=/bin/true",
	)
	releaseCommand.Dir = "../.."
	output, err := releaseCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run application-controller release: %v\n%s", err, output)
	}
	text := string(output)
	for _, expected := range []string{
		"package -d",
		"_charts/application-controller-v9.8.7",
		"--version 9.8.7",
		"--app-version v9.8.7",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("release dry-run is missing %q:\n%s", expected, text)
		}
	}
	for _, forbidden := range []string{
		"Dockerfile.monitor", "docker-release-monitor", "MONITOR_IMG",
		"application-controller-manifests", "oneks-application-controller.yaml",
		"_releases/", "replace-me",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("application-controller release dry-run references forbidden token %q", forbidden)
		}
	}

	imageCommand := exec.Command(
		"make", "-n", "docker-release-application-controller",
		"APPLICATION_CONTROLLER_VERSION:=9.8.7",
		"CONTAINER_TOOL:=docker",
	)
	imageCommand.Dir = "../.."
	output, err = imageCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run application-controller image release: %v\n%s", err, output)
	}
	text = string(output)
	for _, expected := range []string{
		"Dockerfile.application-controller",
		"ghcr.io/opennebula/oneks-application-controller:v9.8.7",
		"--platform=linux/amd64,linux/arm64",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("image release dry-run is missing %q:\n%s", expected, text)
		}
	}
	for _, forbidden := range []string{"Dockerfile.monitor", "MONITOR_IMG_URL"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("application-controller image dry-run references monitor token %q", forbidden)
		}
	}
}

func TestApplicationControllerReleaseWorkflowIsIndependent(t *testing.T) {
	payload, err := os.ReadFile("../../.github/workflows/release-application-controller.yml")
	if err != nil {
		t.Fatalf("read application-controller release workflow: %v", err)
	}
	text := string(payload)
	for _, expected := range []string{
		`tags: ["application-controller-v*.*.*"]`,
		"docker-release-application-controller",
		"application-controller-release",
		"./published-application-controller/oneks-application-controller-*.tgz",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("release workflow is missing %q", expected)
		}
	}
	for _, forbidden := range []string{
		"docker-release-monitor", "monitor-release", "MONITOR_VERSION",
		"oneks-application-controller.yaml", "_releases/",
		"published-application-controller/_charts/",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("application-controller workflow references forbidden token %q", forbidden)
		}
	}
}
