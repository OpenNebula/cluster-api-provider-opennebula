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
	"reflect"
	"sort"
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
	applicationRules := 0
	applicationVerbs := []string{}
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
			applicationRules++
			for _, verb := range rule.Verbs {
				if verb == "*" {
					t.Fatal("wildcard verb is forbidden")
				}
				applicationVerbs = append(applicationVerbs, verb)
			}
		}
	}
	wantApplicationVerbs := []string{"create", "delete", "get", "list", "patch", "watch"}
	sort.Strings(applicationVerbs)
	if applicationRules != 1 || !reflect.DeepEqual(applicationVerbs, wantApplicationVerbs) {
		t.Fatalf("OneKSApplication verbs = %#v across %d rules, want exactly %#v", applicationVerbs, applicationRules, wantApplicationVerbs)
	}

	helmRBAC, err := os.ReadFile("../../helm/v1alpha1/oneks-application-controller/templates/rbac.yaml")
	if err != nil {
		t.Fatalf("read Helm RBAC template: %v", err)
	}
	text := string(helmRBAC)
	if strings.Contains(text, "oneksapplications/finalizers") || strings.Contains(text, "resources: [\"*\"]") {
		t.Fatalf("Helm RBAC contains a forbidden permission")
	}
	if !strings.Contains(text, `verbs: ["get", "list", "watch", "create", "patch", "delete"]`) {
		t.Fatal("Helm RBAC lacks the exact required OneKSApplication verbs")
	}
}

func TestManagedResourceRBACIsKindAndVerbBounded(t *testing.T) {
	payload, err := os.ReadFile("../../kustomize/v1alpha1/application-controller/role_configmap.yaml")
	if err != nil {
		t.Fatalf("read managed-resource ClusterRole: %v", err)
	}
	role := rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(payload, &role); err != nil {
		t.Fatalf("decode managed-resource ClusterRole: %v", err)
	}
	if role.Kind != "ClusterRole" || len(role.Rules) != 7 {
		t.Fatalf("managed-resource permissions are not cluster-scoped and bounded: %#v", role)
	}
	managedVerbs := []string{"get", "list", "watch", "create", "patch", "update", "delete"}
	want := map[string]map[string][]string{
		"":                      {"namespaces": managedVerbs, "configmaps": managedVerbs, "services": {"get"}, "secrets": {"get", "create", "update", "delete"}},
		"helm.cattle.io":        {"helmchartconfigs": managedVerbs},
		"cert-manager.io":       {"clusterissuers": managedVerbs, "certificates": managedVerbs},
		"trust.cert-manager.io": {"bundles": managedVerbs},
		"networking.k8s.io":     {"ingressclasses": {"get"}},
	}
	seen := make(map[string]map[string][]string)
	for _, rule := range role.Rules {
		if len(rule.APIGroups) != 1 {
			t.Fatalf("unexpected API groups: %#v", rule)
		}
		group := rule.APIGroups[0]
		if seen[group] == nil {
			seen[group] = make(map[string][]string)
		}
		for _, resource := range rule.Resources {
			if resource == "*" || containsValue(rule.Verbs, "*") {
				t.Fatalf("wildcard managed-resource permission: %#v", rule)
			}
			seen[group][resource] = append([]string(nil), rule.Verbs...)
		}
	}
	for group, resources := range want {
		for resource, verbs := range resources {
			actual, found := seen[group][resource]
			if !found || strings.Join(actual, ",") != strings.Join(verbs, ",") {
				t.Fatalf("permission %s/%s = %#v, want %#v", group, resource, actual, verbs)
			}
			delete(seen[group], resource)
		}
	}
	for group, resources := range seen {
		if len(resources) != 0 {
			t.Fatalf("unexpected permissions in %q: %#v", group, resources)
		}
	}

	helmPayload, err := os.ReadFile("../../helm/v1alpha1/oneks-application-controller/templates/rbac.yaml")
	if err != nil {
		t.Fatalf("read Helm RBAC: %v", err)
	}
	for _, required := range []string{
		`resources: ["namespaces", "configmaps"]`, `resources: ["helmchartconfigs"]`,
		`resources: ["clusterissuers", "certificates"]`, `resources: ["bundles"]`,
		"- apiGroups: [\"\"]\n  resources: [\"services\"]\n  verbs: [\"get\"]",
		"- apiGroups: [\"\"]\n  resources: [\"secrets\"]\n  # Get reads readiness metadata and immutable input/target Secrets. Create,\n  # update, and delete implement protected Secret lifecycle without patching.\n  verbs: [\"get\", \"create\", \"update\", \"delete\"]",
		"- apiGroups: [\"networking.k8s.io\"]\n  resources: [\"ingressclasses\"]\n  verbs: [\"get\"]",
	} {
		if !strings.Contains(string(helmPayload), required) {
			t.Fatalf("Helm RBAC is missing %q", required)
		}
	}
	if strings.Contains(string(helmPayload), `resources: ["*"]`) || strings.Contains(string(helmPayload), `verbs: ["*"]`) {
		t.Fatal("Helm managed-resource RBAC contains a wildcard permission")
	}

	bindingPayload, err := os.ReadFile("../../kustomize/v1alpha1/application-controller/role_binding_configmap.yaml")
	if err != nil {
		t.Fatalf("read managed-resource ClusterRoleBinding: %v", err)
	}
	binding := rbacv1.ClusterRoleBinding{}
	if err := yaml.Unmarshal(bindingPayload, &binding); err != nil {
		t.Fatalf("decode managed-resource ClusterRoleBinding: %v", err)
	}
	if binding.Kind != "ClusterRoleBinding" || binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != role.Name {
		t.Fatalf("managed-resource ClusterRole binding mismatch: %#v", binding)
	}
}
