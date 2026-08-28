package resourceobserver

import (
	"strings"
	"testing"
)

const validConfig = `- id: deployment-ready
  apiVersion: apps/v1
  resource: deployments
  namespace: payments
  name: api
  path: status.readyReplicas
`

func TestParseConfigStrictAndCanonical(t *testing.T) {
	config, digest, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatalf("parse valid config: %v", err)
	}
	if len(config) != 1 || config[0].Path != "status.readyReplicas" || !strings.HasPrefix(digest, "sha256-") || len(digest) != 71 {
		t.Fatalf("unexpected config or digest: %#v %q", config, digest)
	}
	_, second, err := ParseConfig([]byte(validConfig))
	if err != nil || second != digest {
		t.Fatalf("digest is not stable: %q %q %v", digest, second, err)
	}
}

func TestParseConfigRejectsUnsafeOrInvalidInput(t *testing.T) {
	tests := map[string]string{
		"unknown field":      strings.Replace(validConfig, "path: status.readyReplicas", "path: status.readyReplicas\n  unknown: true", 1),
		"duplicate field":    strings.Replace(validConfig, "name: api", "name: api\n  name: duplicate", 1),
		"multiple documents": validConfig + "---\n{}\n",
		"invalid path":       strings.Replace(validConfig, "status.readyReplicas", "status.conditions[0]", 1),
		"mapping root":       "id: deployment-ready\n",
		"null root":          "null\n",
		"duplicate id":       validConfig + validConfig,
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseConfig([]byte(document)); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestParseConfigAcceptsEmptyList(t *testing.T) {
	config, _, err := ParseConfig([]byte("[]\n"))
	if err != nil || len(config) != 0 {
		t.Fatalf("empty resource list was rejected: %#v %v", config, err)
	}
}

func TestParseConfigAcceptsResourceList(t *testing.T) {
	document := validConfig + strings.ReplaceAll(strings.ReplaceAll(validConfig, "deployment-ready", "deployment-replicas"), "status.readyReplicas", "status.replicas")
	config, _, err := ParseConfig([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	if len(config) != 2 || config[1].ID != "deployment-replicas" {
		t.Fatalf("unexpected resource list: %#v", config)
	}
}

func TestGenericGVRIsAcceptedBeforeRBACAuthorization(t *testing.T) {
	document := strings.ReplaceAll(validConfig, "apps/v1", "run.ai/v1")
	document = strings.ReplaceAll(document, "deployments", "workloads")
	if _, _, err := ParseConfig([]byte(document)); err != nil {
		t.Fatalf("generic CRD was rejected: %v", err)
	}
}

func TestClusterScopedResourceAllowsEmptyNamespace(t *testing.T) {
	document := strings.ReplaceAll(validConfig, "apps/v1", "v1")
	document = strings.ReplaceAll(document, "deployments", "nodes")
	document = strings.ReplaceAll(document, "namespace: payments", "namespace: \"\"")
	if _, _, err := ParseConfig([]byte(document)); err != nil {
		t.Fatalf("cluster-scoped configuration was rejected: %v", err)
	}
}
