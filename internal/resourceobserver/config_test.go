package resourceobserver

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

func TestParseConfig(t *testing.T) {
	config, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatalf("parse valid config: %v", err)
	}
	if len(config) != 1 || config[0].Path != "status.readyReplicas" {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestParseConfigRequiresFields(t *testing.T) {
	if _, err := ParseConfig([]byte("- id: incomplete\n")); err == nil {
		t.Fatal("incomplete resource was accepted")
	}
}

func TestParseConfigAcceptsEmptyList(t *testing.T) {
	config, err := ParseConfig([]byte("[]\n"))
	if err != nil || len(config) != 0 {
		t.Fatalf("empty resource list was rejected: %#v %v", config, err)
	}
}
