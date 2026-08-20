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
	if role.Kind != "ClusterRole" || len(role.Rules) != 12 {
		t.Fatalf("managed-resource permissions are not cluster-scoped and bounded: %#v", role)
	}
	managedVerbs := []string{"get", "list", "watch", "create", "patch", "update", "delete"}
	want := map[string]map[string][]string{
		"":                      {"namespaces": managedVerbs, "configmaps": managedVerbs, "services": {"get"}, "secrets": {"get", "create", "update", "delete"}, "pods": {"list"}, "endpoints": {"get"}},
		"apps":                  {"deployments": {"list"}},
		"apiextensions.k8s.io":  {"customresourcedefinitions": {"get"}},
		"helm.cattle.io":        {"helmchartconfigs": managedVerbs},
		"cert-manager.io":       {"clusterissuers": managedVerbs, "certificates": managedVerbs},
		"trust.cert-manager.io": {"bundles": managedVerbs},
		"networking.k8s.io":     {"ingressclasses": {"get"}},
		"longhorn.io":           {"settings": {"get", "patch"}},
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
			if group == "longhorn.io" && resource == "settings" &&
				!reflect.DeepEqual(rule.ResourceNames, []string{"deleting-confirmation-flag"}) {
				t.Fatalf("Longhorn Setting permission is not identity-bounded: %#v", rule)
			}
			if group == "apiextensions.k8s.io" && resource == "customresourcedefinitions" &&
				!reflect.DeepEqual(rule.ResourceNames, []string{
					"certificates.cert-manager.io", "certificaterequests.cert-manager.io",
					"issuers.cert-manager.io", "clusterissuers.cert-manager.io",
				}) {
				t.Fatalf("cert-manager CRD reads are not identity-bounded: %#v", rule)
			}
			if group == "" && resource == "endpoints" &&
				!reflect.DeepEqual(rule.ResourceNames, []string{"cert-manager-webhook"}) {
				t.Fatalf("cert-manager Endpoints read is not identity-bounded: %#v", rule)
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
		"- apiGroups: [\"longhorn.io\"]\n  resources: [\"settings\"]\n  resourceNames: [\"deleting-confirmation-flag\"]\n  verbs: [\"get\", \"patch\"]",
		"- apiGroups: [\"apiextensions.k8s.io\"]\n  resources: [\"customresourcedefinitions\"]\n  resourceNames:\n  - certificates.cert-manager.io\n  - certificaterequests.cert-manager.io\n  - issuers.cert-manager.io\n  - clusterissuers.cert-manager.io\n  verbs: [\"get\"]",
		"- apiGroups: [\"apps\"]\n  resources: [\"deployments\"]\n  verbs: [\"list\"]",
		"- apiGroups: [\"\"]\n  resources: [\"pods\"]\n  verbs: [\"list\"]",
		"- apiGroups: [\"\"]\n  resources: [\"endpoints\"]\n  resourceNames: [\"cert-manager-webhook\"]\n  verbs: [\"get\"]",
		"kind: ClusterRoleBinding\nmetadata:\n  name: oneks-application-controller-managed-resources\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: oneks-application-controller-managed-resources",
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
	if binding.Kind != "ClusterRoleBinding" || binding.Name != "oneks-application-controller-managed-resources" ||
		binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != "oneks-application-controller-managed-resources" {
		t.Fatalf("managed-resource ClusterRole binding mismatch: %#v", binding)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" ||
		binding.Subjects[0].Name != "oneks-application-controller" || binding.Subjects[0].Namespace != "oneks-system" {
		t.Fatalf("managed-resource ClusterRole binding subject mismatch: %#v", binding.Subjects)
	}
}
