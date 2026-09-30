/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import "testing"

const validResourceConfig = `- apiVersion: apps/v1
  resource: deployments
  namespace: payments
  name: api
  path: status.readyReplicas
`

func TestParseResourceConfig(t *testing.T) {
	config, err := parseResourceConfig([]byte(validResourceConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(config) != 1 || config[0].Path != "status.readyReplicas" {
		t.Fatalf("unexpected resource configuration: %#v", config)
	}
}

func TestParseResourceConfigRejectsIncompleteResource(t *testing.T) {
	if _, err := parseResourceConfig([]byte("- apiVersion: apps/v1\n")); err == nil {
		t.Fatal("incomplete resource was accepted")
	}
}

func TestParseResourceConfigAcceptsEmptyList(t *testing.T) {
	config, err := parseResourceConfig([]byte("[]\n"))
	if err != nil || len(config) != 0 {
		t.Fatalf("empty resource list was rejected: %#v %v", config, err)
	}
}
