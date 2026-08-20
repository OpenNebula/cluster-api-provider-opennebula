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
	payload, err := os.ReadFile("../../helm/v1alpha1/oneks-application-controller/values.schema.json")
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
	template, err := os.ReadFile("../../helm/v1alpha1/oneks-application-controller/templates/configmap.yaml")
	if err != nil {
		t.Fatalf("read Helm ConfigMap template: %v", err)
	}
	if !strings.Contains(string(template), `required "clusterID is required" .Values.clusterID`) {
		t.Fatal("Helm ConfigMap does not fail rendering when clusterID is absent")
	}
	base, err := os.ReadFile("../../kustomize/v1alpha1/application-controller/configmap.yaml")
	if err != nil {
		t.Fatalf("read kustomize ConfigMap: %v", err)
	}
	if !strings.Contains(string(base), "cluster-id: replace-me") {
		t.Fatal("manual kustomize base no longer exposes its replacement marker")
	}
}

func TestHelmAndKustomizeUseTheSameGeneratedCRD(t *testing.T) {
	kustomizeCRD, err := os.ReadFile("../../kustomize/v1alpha1/application-controller/crd/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatalf("read kustomize CRD: %v", err)
	}
	helmCRD, err := os.ReadFile("../../helm/v1alpha1/oneks-application-controller/crds/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatalf("read Helm CRD: %v", err)
	}
	if !bytes.Equal(kustomizeCRD, helmCRD) {
		t.Fatal("Helm and kustomize CRDs differ")
	}
}

func TestHelmKeepsControllerNamespacesOnUninstall(t *testing.T) {
	payload, err := os.ReadFile("../../helm/v1alpha1/oneks-application-controller/templates/namespaces.yaml")
	if err != nil {
		t.Fatalf("read Helm Namespace template: %v", err)
	}
	text := string(payload)
	for _, namespace := range []string{"oneks-system", "oneks-poc-workloads"} {
		if !strings.Contains(text, "name: "+namespace) {
			t.Fatalf("Helm Namespace template is missing %s", namespace)
		}
	}
	if count := strings.Count(text, "helm.sh/resource-policy: keep"); count != 2 {
		t.Fatalf("kept Helm Namespace count = %d, want 2", count)
	}
}

func TestNamespacePermissionSupportsManagedClusterResources(t *testing.T) {
	for _, path := range []string{
		"../../kustomize/v1alpha1/application-controller/role_configmap.yaml",
		"../../helm/v1alpha1/oneks-application-controller/templates/rbac.yaml",
	} {
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
}

func TestManagedResourceBindingUsesUpgradeSafeIdentity(t *testing.T) {
	want := "kind: ClusterRoleBinding\nmetadata:\n  name: oneks-application-controller-managed-resources\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: oneks-application-controller-managed-resources"
	for _, path := range []string{
		"../../kustomize/v1alpha1/application-controller/role_binding_configmap.yaml",
		"../../helm/v1alpha1/oneks-application-controller/templates/rbac.yaml",
	} {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(payload)
		if strings.Count(text, "kind: ClusterRoleBinding") != 1 || !strings.Contains(text, want) {
			t.Fatalf("%s does not define only the upgrade-safe managed-resource binding", path)
		}
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
