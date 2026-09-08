package monitor

import (
	"testing"
)

const validConfig = `- id: deployment-ready
  apiVersion: apps/v1
  resource: deployments
  namespace: payments
  name: api
  path: status.readyReplicas
`

func TestParseResourceConfig(t *testing.T) {
	config, err := parseResourceConfig([]byte(validConfig))
	if err != nil {
		t.Fatalf("parse valid config: %v", err)
	}
	if len(config) != 1 || config[0].Path != "status.readyReplicas" {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestParseResourceConfigRequiresFields(t *testing.T) {
	if _, err := parseResourceConfig([]byte("- id: incomplete\n")); err == nil {
		t.Fatal("incomplete resource was accepted")
	}
}

func TestParseResourceConfigAcceptsEmptyList(t *testing.T) {
	config, err := parseResourceConfig([]byte("[]\n"))
	if err != nil || len(config) != 0 {
		t.Fatalf("empty resource list was rejected: %#v %v", config, err)
	}
}
