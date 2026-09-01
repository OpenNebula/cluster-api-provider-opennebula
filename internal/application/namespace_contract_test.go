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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRootTargetNamespaceContract(t *testing.T) {
	for _, createNamespace := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "create"}[createNamespace], func(t *testing.T) {
			app := validProtectedRootPlan(t)
			app.Spec.Release.TargetNamespace = "runai-backend"
			app.Spec.Release.CreateNamespace = createNamespace
			refreshDigest(t, app)
			if err := ValidatePlan(app, validationConfig()); err != nil {
				t.Fatalf("Root namespace contract rejected: %v", err)
			}
		})
	}
}

func TestCanonicalPlanPreservesNamespaceContract(t *testing.T) {
	app := validProtectedRootPlan(t)
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
		t.Fatalf("canonical release namespace contract = %#v", release)
	}
	if got := Digest(canonical); got != app.Spec.PlanDigest {
		t.Fatalf("canonical digest = %q, planDigest = %q", got, app.Spec.PlanDigest)
	}
}

func TestRootRejectsInvalidTargetNamespace(t *testing.T) {
	for _, namespace := range []string{"RunAI", strings.Repeat("a", 64)} {
		t.Run(namespace, func(t *testing.T) {
			app := validProtectedRootPlan(t)
			app.Spec.Release.TargetNamespace = namespace
			refreshDigest(t, app)
			if err := ValidatePlan(app, validationConfig()); err == nil || err.Reason != "InvalidTargetNamespace" {
				t.Fatalf("invalid target namespace error = %#v", err)
			}
		})
	}
}

func TestDesiredHelmChartPreservesNamespaceContract(t *testing.T) {
	app := validProtectedRootPlan(t)
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
}

func TestGeneratedCRDHasNoFixedWorkloadNamespace(t *testing.T) {
	crd := string(generatedApplicationCRD(t))
	if strings.Contains(crd, "self.release.targetNamespace == '") {
		t.Fatal("generated CRD retains a fixed workload namespace rule")
	}
}
