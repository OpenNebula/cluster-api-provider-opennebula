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

package application

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRootTargetNamespaceContractByPlanVersion(t *testing.T) {
	legacy := goldenApplication(t)
	if err := ValidatePlan(legacy, validationConfig()); err != nil {
		t.Fatalf("valid plan-v1alpha1 rejected: %v", err)
	}

	legacy.Spec.Release.TargetNamespace = "monitoring"
	refreshDigest(t, legacy)
	if err := ValidatePlan(legacy, validationConfig()); err == nil || err.Reason != "InvalidTargetNamespace" {
		t.Fatalf("plan-v1alpha1 target namespace error = %#v", err)
	}

	legacy = goldenApplication(t)
	legacy.Spec.Release.CreateNamespace = true
	refreshDigest(t, legacy)
	if err := ValidatePlan(legacy, validationConfig()); err == nil || err.Reason != "InvalidCreateNamespace" {
		t.Fatalf("plan-v1alpha1 createNamespace error = %#v", err)
	}

	for _, version := range []string{
		applicationv1.PlanVersionV1Alpha2,
		applicationv1.PlanVersionV1Alpha3,
		applicationv1.PlanVersionV1Alpha4,
		applicationv1.PlanVersionV1Alpha5,
	} {
		for _, createNamespace := range []bool{false, true} {
			t.Run(version+"/create="+map[bool]string{false: "false", true: "true"}[createNamespace], func(t *testing.T) {
				app := modernRootForNamespaceTest(t, version)
				namespace := map[string]map[bool]string{
					applicationv1.PlanVersionV1Alpha2: {false: "custom-v2", true: "custom-v2-created"},
					applicationv1.PlanVersionV1Alpha3: {false: "custom-v3", true: "custom-v3-created"},
					applicationv1.PlanVersionV1Alpha4: {false: "custom-v4", true: "runai-backend"},
					applicationv1.PlanVersionV1Alpha5: {false: "custom-v5", true: "runai-v5"},
				}[version][createNamespace]
				app.Spec.Release.TargetNamespace = namespace
				app.Spec.Release.CreateNamespace = createNamespace
				refreshDigest(t, app)
				if err := ValidatePlan(app, validationConfig()); err != nil {
					t.Fatalf("modern Root namespace contract rejected: %v", err)
				}
			})
		}
	}
}

func TestPlanV1Alpha4CanonicalPreservesModernNamespaceContract(t *testing.T) {
	app := validPlanV1Alpha4(t)
	app.Spec.Release.TargetNamespace = "runai-backend"
	app.Spec.Release.CreateNamespace = true
	refreshDigest(t, app)

	canonical, err := CanonicalPlan(app.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal(canonical, &input); err != nil {
		t.Fatal(err)
	}
	release, ok := input["release"].(map[string]any)
	if !ok || release["targetNamespace"] != "runai-backend" || release["createNamespace"] != true {
		t.Fatalf("canonical v4 release namespace contract = %#v", release)
	}
	if got := Digest(canonical); got != app.Spec.PlanDigest {
		t.Fatalf("canonical v4 digest = %q, planDigest = %q", got, app.Spec.PlanDigest)
	}
}

func TestModernRootRejectsInvalidTargetNamespace(t *testing.T) {
	for _, version := range []string{
		applicationv1.PlanVersionV1Alpha2,
		applicationv1.PlanVersionV1Alpha3,
		applicationv1.PlanVersionV1Alpha4,
		applicationv1.PlanVersionV1Alpha5,
	} {
		for _, namespace := range []string{"RunAI", strings.Repeat("a", 64)} {
			t.Run(version+"/"+namespace, func(t *testing.T) {
				app := modernRootForNamespaceTest(t, version)
				app.Spec.Release.TargetNamespace = namespace
				refreshDigest(t, app)
				if err := ValidatePlan(app, validationConfig()); err == nil || err.Reason != "InvalidTargetNamespace" {
					t.Fatalf("invalid target namespace error = %#v", err)
				}
			})
		}
	}
}

func TestDesiredHelmChartPreservesModernNamespaceContract(t *testing.T) {
	for _, version := range []string{
		applicationv1.PlanVersionV1Alpha2,
		applicationv1.PlanVersionV1Alpha3,
		applicationv1.PlanVersionV1Alpha4,
		applicationv1.PlanVersionV1Alpha5,
	} {
		t.Run(version, func(t *testing.T) {
			app := modernRootForNamespaceTest(t, version)
			app.Spec.Release.TargetNamespace = "runai-backend"
			app.Spec.Release.CreateNamespace = true

			helm := desiredHelmChart(app)
			targetNamespace, found, err := unstructured.NestedString(helm.Object, "spec", "targetNamespace")
			if err != nil || !found || targetNamespace != "runai-backend" {
				t.Fatalf("HelmChart targetNamespace = %q, %t, %v", targetNamespace, found, err)
			}
			createNamespace, found, err := unstructured.NestedBool(helm.Object, "spec", "createNamespace")
			if err != nil || !found || !createNamespace {
				t.Fatalf("HelmChart createNamespace = %t, %t, %v", createNamespace, found, err)
			}
		})
	}
}

func TestGeneratedCRDFixesOnlyPlanV1Alpha1RootNamespace(t *testing.T) {
	payload, err := os.ReadFile("../../config/crd/bases/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := string(payload)
	if count := strings.Count(crd, "self.release.targetNamespace == 'oneks-poc-workloads'"); count != 1 {
		t.Fatalf("fixed workload namespace CEL rule count = %d, want 1", count)
	}
	for _, forbidden := range []string{
		"plan-v1alpha2 Root requires the fixed workload namespace",
		"plan-v1alpha3 Root requires the fixed workload namespace",
		"plan-v1alpha4 Root requires the fixed workload namespace",
		"plan-v1alpha5 Root requires the fixed workload namespace",
	} {
		if strings.Contains(crd, forbidden) {
			t.Fatalf("generated CRD retains modern fixed-namespace rule %q", forbidden)
		}
	}
}

func modernRootForNamespaceTest(t *testing.T, version string) *applicationv1.OneKSApplication {
	t.Helper()
	switch version {
	case applicationv1.PlanVersionV1Alpha2:
		return validPlanV1Alpha2Root(t)
	case applicationv1.PlanVersionV1Alpha3:
		return validPlanV1Alpha3(t)
	case applicationv1.PlanVersionV1Alpha4:
		return validPlanV1Alpha4(t)
	case applicationv1.PlanVersionV1Alpha5:
		return validPlanV1Alpha5(t)
	default:
		t.Fatalf("unsupported test plan version %q", version)
		return nil
	}
}
