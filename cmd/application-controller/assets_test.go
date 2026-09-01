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
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestHelmClusterIDValueContract(t *testing.T) {
	payload, err := os.ReadFile("../../helm/v1alpha5/oneks-application-controller/values.schema.json")
	if err != nil {
		t.Fatalf("read Helm values schema: %v", err)
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type      string `json:"type"`
			MinLength int    `json:"minLength"`
			MaxLength int    `json:"maxLength"`
			Pattern   string `json:"pattern"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(payload, &schema); err != nil {
		t.Fatalf("decode Helm values schema: %v", err)
	}
	clusterID, exists := schema.Properties["clusterID"]
	if !exists || !containsValue(schema.Required, "clusterID") || clusterID.Type != "string" || clusterID.MinLength != 1 || clusterID.MaxLength != 63 || clusterID.Pattern == "" {
		t.Fatalf("clusterID is not a required bounded label value: %#v", clusterID)
	}
	template, err := os.ReadFile("../../helm/v1alpha5/oneks-application-controller/templates/configmap.yaml")
	if err != nil {
		t.Fatalf("read Helm ConfigMap template: %v", err)
	}
	if !strings.Contains(string(template), `required "clusterID is required" .Values.clusterID`) {
		t.Fatal("Helm ConfigMap does not fail rendering when clusterID is absent")
	}
}

func TestHelmUsesTheGeneratedCRD(t *testing.T) {
	generatedCRD, err := os.ReadFile("../../config/crd/bases/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatalf("read generated CRD: %v", err)
	}
	helmCRD, err := os.ReadFile("../../helm/v1alpha5/oneks-application-controller/crds/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatalf("read Helm CRD: %v", err)
	}
	if !bytes.Equal(generatedCRD, helmCRD) {
		t.Fatal("generated and Helm CRDs differ")
	}
}

func TestChartDoesNotCreateSharedWorkloadNamespaces(t *testing.T) {
	path := "../../helm/v1alpha5/oneks-application-controller/templates/namespaces.yaml"
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("obsolete Namespace manifest %s still exists: %v", path, err)
	}
}

func TestNamespacePermissionSupportsManagedClusterResources(t *testing.T) {
	path := "../../helm/v1alpha5/oneks-application-controller/templates/rbac-role-managed-resources.yaml"
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(payload)
	if !strings.Contains(text, "- apiGroups: [\"\"]\n  resources: [\"namespaces\", \"configmaps\"]\n  verbs: [\"get\", \"list\", \"watch\", \"create\", \"patch\", \"update\", \"delete\"]") {
		t.Fatalf("%s lacks bounded managed Namespace permissions", path)
	}
	if strings.Contains(text, `resources: ["*"]`) {
		t.Fatalf("%s grants wildcard resources", path)
	}
}

func TestManagedResourceBindingUsesUpgradeSafeIdentity(t *testing.T) {
	want := "kind: ClusterRoleBinding\nmetadata:\n  name: oneks-application-controller-managed-resources\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: oneks-application-controller-managed-resources"
	path := "../../helm/v1alpha5/oneks-application-controller/templates/rbac-role-binding-managed-resources.yaml"
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(payload)
	if strings.Count(text, "kind: ClusterRoleBinding") != 1 || !strings.Contains(text, want) {
		t.Fatalf("%s does not define only the upgrade-safe managed-resource binding", path)
	}
}

func containsValue(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
