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
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestApplicationRoleHasOnlyRequiredApplicationWrites(t *testing.T) {
	payload, err := os.ReadFile("../../kustomize/v1alpha1/application-controller/role_application.yaml")
	if err != nil {
		t.Fatalf("read application Role: %v", err)
	}
	role := rbacv1.Role{}
	if err := yaml.Unmarshal(payload, &role); err != nil {
		t.Fatalf("decode application Role: %v", err)
	}
	foundRootUpdate := false
	foundDependencyCreate := false
	foundDependencyDelete := false
	for _, rule := range role.Rules {
		for _, resource := range rule.Resources {
			if resource == "*" || resource == "secrets" || resource == "namespaces" {
				t.Fatalf("forbidden application Role resource %q", resource)
			}
			if resource == "oneksapplications/finalizers" {
				t.Fatal("unused finalizers subresource permission remains")
			}
			if resource != "oneksapplications" {
				continue
			}
			for _, verb := range rule.Verbs {
				if verb == "*" {
					t.Fatal("wildcard verb is forbidden")
				}
				if verb == "update" {
					foundRootUpdate = true
				}
				if verb == "create" {
					foundDependencyCreate = true
				}
				if verb == "delete" {
					foundDependencyDelete = true
				}
			}
		}
	}
	if !foundRootUpdate {
		t.Fatal("root OneKSApplication update is required for Client.Update finalizers")
	}
	if !foundDependencyCreate {
		t.Fatal("OneKSApplication create is required for dependency materialization")
	}
	if !foundDependencyDelete {
		t.Fatal("OneKSApplication delete is required for shared dependency garbage collection")
	}

	helmRBAC, err := os.ReadFile("../../helm/v1alpha1/oneks-application-controller/templates/rbac.yaml")
	if err != nil {
		t.Fatalf("read Helm RBAC template: %v", err)
	}
	text := string(helmRBAC)
	if strings.Contains(text, "oneksapplications/finalizers") || strings.Contains(text, "resources: [\"*\"]") || strings.Contains(text, "resources: [\"secrets\"]") {
		t.Fatalf("Helm RBAC contains a forbidden permission")
	}
	if !strings.Contains(text, `verbs: ["get", "list", "watch", "create", "update", "delete"]`) {
		t.Fatal("Helm RBAC lacks required OneKSApplication create/update/delete permissions")
	}
}

func TestConfigMapRBACSupportsCompiledDependencyNamespaces(t *testing.T) {
	payload, err := os.ReadFile("../../kustomize/v1alpha1/application-controller/role_configmap.yaml")
	if err != nil {
		t.Fatalf("read ConfigMap ClusterRole: %v", err)
	}
	role := rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(payload, &role); err != nil {
		t.Fatalf("decode ConfigMap ClusterRole: %v", err)
	}
	if role.Kind != "ClusterRole" || len(role.Rules) != 1 || !containsValue(role.Rules[0].Resources, "configmaps") {
		t.Fatalf("ConfigMap permissions are not cluster-scoped and resource-bounded: %#v", role)
	}
	for _, forbidden := range []string{"*", "secrets"} {
		if containsValue(role.Rules[0].Resources, forbidden) {
			t.Fatalf("ConfigMap ClusterRole grants forbidden resource %q", forbidden)
		}
	}

	bindingPayload, err := os.ReadFile("../../kustomize/v1alpha1/application-controller/role_binding_configmap.yaml")
	if err != nil {
		t.Fatalf("read ConfigMap ClusterRoleBinding: %v", err)
	}
	binding := rbacv1.ClusterRoleBinding{}
	if err := yaml.Unmarshal(bindingPayload, &binding); err != nil {
		t.Fatalf("decode ConfigMap ClusterRoleBinding: %v", err)
	}
	if binding.Kind != "ClusterRoleBinding" || binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != role.Name {
		t.Fatalf("ConfigMap ClusterRole binding mismatch: %#v", binding)
	}
}
