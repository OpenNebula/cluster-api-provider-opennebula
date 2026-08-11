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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"
)

const planV1Alpha1GoldenDigest = "sha256-M55-5y1MVfp5TI27rz_ssKVui_0RNKYTrM_wVWSfRqg"

func TestPlanV1Alpha1CanonicalFixtureRemainsUnchanged(t *testing.T) {
	payload, err := os.ReadFile("testdata/oneks_application_plan_golden.json")
	if err != nil {
		t.Fatalf("read plan-v1alpha1 fixture: %v", err)
	}
	var spec applicationv1.OneKSApplicationSpec
	if err := json.Unmarshal(payload, &spec); err != nil {
		t.Fatalf("decode plan-v1alpha1 fixture: %v", err)
	}
	canonical, err := CanonicalPlan(spec)
	if err != nil {
		t.Fatalf("canonicalize plan-v1alpha1 fixture: %v", err)
	}
	want, err := os.ReadFile("testdata/oneks_application_plan_golden.canonical.json")
	if err != nil {
		t.Fatalf("read canonical plan-v1alpha1 fixture: %v", err)
	}
	if !bytes.Equal(canonical, want) {
		t.Fatalf("plan-v1alpha1 canonical bytes changed:\n got: %s\nwant: %s", canonical, want)
	}
	if got := digestForPlanV1Alpha2Test(canonical); got != planV1Alpha1GoldenDigest {
		t.Fatalf("plan-v1alpha1 digest changed: got %s, want %s", got, planV1Alpha1GoldenDigest)
	}
}

func TestPlanV1Alpha1RejectsNewFieldsAtRuntime(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplicationSpec)
	}{
		{"role", func(spec *applicationv1.OneKSApplicationSpec) { spec.Role = applicationv1.ApplicationRoleRoot }},
		{"dependencies", func(spec *applicationv1.OneKSApplicationSpec) {
			spec.Dependencies = []applicationv1.DependencyReference{}
		}},
		{"dependency plans", func(spec *applicationv1.OneKSApplicationSpec) {
			spec.DependencyPlans = []applicationv1.DependencyPlan{}
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			app := planV1Alpha1FixtureApplication(t)
			test.mutate(&app.Spec)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "InvalidPlanV1Alpha1Fields" {
				t.Fatalf("got error %#v, want InvalidPlanV1Alpha1Fields", err)
			}
		})
	}
}

func TestPlanV1Alpha1PreservesNamespaceReleaseAndProducerContracts(t *testing.T) {
	app := planV1Alpha1FixtureApplication(t)
	labels := producerLabels(app)
	if len(labels) != 5 || labels[LabelRootManagedBy] != RootManagedByValue || labels[LabelProducer] != ProducerValue {
		t.Fatalf("plan-v1alpha1 producer labels changed: %#v", labels)
	}
	if _, exists := labels[LabelRole]; exists {
		t.Fatalf("plan-v1alpha1 unexpectedly requires a role label: %#v", labels)
	}

	app.Spec.Release.TargetNamespace = "monitoring"
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "InvalidTargetNamespace" {
		t.Fatalf("plan-v1alpha1 target namespace error = %#v", err)
	}

	app = planV1Alpha1FixtureApplication(t)
	app.Spec.Release.CreateNamespace = true
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "InvalidCreateNamespace" {
		t.Fatalf("plan-v1alpha1 createNamespace error = %#v", err)
	}

	app = planV1Alpha1FixtureApplication(t)
	app.Spec.Release.RepositoryURL = ""
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "InvalidRepositoryURL" {
		t.Fatalf("plan-v1alpha1 repositoryURL error = %#v", err)
	}
}

func TestGeneratedCRDPreservesV1Alpha1AndBoundsV1Alpha2(t *testing.T) {
	payload, err := os.ReadFile("../../config/crd/bases/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatalf("read generated OneKSApplication CRD: %v", err)
	}
	text := string(payload)
	for _, required := range []string{
		"- oneks.opennebula.io/plan-v1alpha1",
		"- oneks.opennebula.io/plan-v1alpha2",
		"!has(self.role)",
		"!has(self.dependencies)",
		"!has(self.dependencyPlans)",
		"self.release.targetNamespace == 'oneks-poc-workloads'",
		"!self.release.createNamespace",
		"plan-v1alpha1 requires a non-empty HTTPS repositoryURL",
		"plan-v1alpha2 requires role",
		"plan-v1alpha2 release must use either an HTTPS repositoryURL",
		"each direct Root dependency must resolve to exactly one matching",
		"maxItems: 16",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("generated CRD is missing %q", required)
		}
	}

	external := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(payload, external); err != nil {
		t.Fatalf("decode generated OneKSApplication CRD: %v", err)
	}
	internal := &apiextensions.CustomResourceDefinition{}
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(external, internal, nil); err != nil {
		t.Fatalf("convert generated OneKSApplication CRD: %v", err)
	}
	internal.Status.StoredVersions = []string{"v1alpha1"}
	if errors := apiextensionsvalidation.ValidateCustomResourceDefinition(context.Background(), internal); len(errors) != 0 {
		t.Fatalf("generated OneKSApplication CRD is invalid: %v", errors.ToAggregate())
	}
}

func TestStatusAdvertisesBothPlanVersions(t *testing.T) {
	app := validPlanV1Alpha2Dependency(t)
	status := baseStatus(app, "test")
	want := []string{applicationv1.PlanVersionV1Alpha1, applicationv1.PlanVersionV1Alpha2}
	if len(status.SupportedPlanVersions) != len(want) {
		t.Fatalf("supported versions = %#v, want %#v", status.SupportedPlanVersions, want)
	}
	for index := range want {
		if status.SupportedPlanVersions[index] != want[index] {
			t.Fatalf("supported versions = %#v, want %#v", status.SupportedPlanVersions, want)
		}
	}
}

func TestPlanV1Alpha2DependencyPrometheusCanCreateMonitoringNamespace(t *testing.T) {
	app := validPlanV1Alpha2Dependency(t)
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
		t.Fatalf("valid plan-v1alpha2 Dependency rejected: %v", err)
	}
}

func TestPlanV1Alpha2AcceptsCompleteDependencyDAGs(t *testing.T) {
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	dLeaf := dependencyPlanForTest("oneks-d", "chart-d", nil)
	dWithE := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})

	tests := []struct {
		name         string
		dependencies []applicationv1.DependencyReference
		plans        []applicationv1.DependencyPlan
	}{
		{"root to D", []applicationv1.DependencyReference{dependencyReferenceForPlan(dLeaf)}, []applicationv1.DependencyPlan{dLeaf}},
		{"root to D to E", []applicationv1.DependencyReference{dependencyReferenceForPlan(dWithE)}, []applicationv1.DependencyPlan{dWithE, e}},
		{
			"shared DAG",
			[]applicationv1.DependencyReference{dependencyReferenceForPlan(dWithE), dependencyReferenceForPlan(e)},
			[]applicationv1.DependencyPlan{dWithE, e},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := validPlanV1Alpha2RootGraph(t, test.dependencies, test.plans)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
				t.Fatalf("valid dependency DAG rejected: %v", err)
			}
		})
	}
}

func TestDependencyPlanDigestCommitsToCanonicalChildSpec(t *testing.T) {
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})
	for _, plan := range []applicationv1.DependencyPlan{d, e} {
		child := dependencyPlanChildSpec("42", plan)
		canonical, err := CanonicalPlan(child)
		if err != nil {
			t.Fatalf("canonicalize child %s: %v", plan.Name, err)
		}
		if got := digestForPlanV1Alpha2Test(canonical); got != plan.PlanDigest {
			t.Fatalf("child %s digest = %s, want %s", plan.Name, got, plan.PlanDigest)
		}
		if child.ExecutionMode != applicationv1.ExecutionModeExecute || child.Role != applicationv1.ApplicationRoleDependency || child.DependencyPlans != nil {
			t.Fatalf("child %s materialized with wrong fixed fields: %#v", plan.Name, child)
		}
	}
}

func TestPlanV1Alpha2RejectsInvalidDependencyGraphs(t *testing.T) {
	validDigestA := "sha256-" + strings.Repeat("A", 43)
	validDigestB := "sha256-" + strings.Repeat("B", 43)
	tests := []struct {
		name   string
		build  func(*testing.T) *applicationv1.OneKSApplication
		reason string
	}{
		{
			name: "nested missing plan",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				missing := applicationv1.DependencyReference{Name: "oneks-e", CatalogueChartID: "chart-e", PlanDigest: validDigestA}
				d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{missing})
				return validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d})
			},
			reason: "UnresolvedDependency",
		},
		{
			name: "nested catalogue chart mismatch",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				e := dependencyPlanForTest("oneks-e", "chart-e", nil)
				reference := dependencyReferenceForPlan(e)
				reference.CatalogueChartID = "wrong-chart"
				d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{reference})
				return validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
			},
			reason: "UnresolvedDependency",
		},
		{
			name: "nested plan digest mismatch",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				e := dependencyPlanForTest("oneks-e", "chart-e", nil)
				reference := dependencyReferenceForPlan(e)
				reference.PlanDigest = validDigestB
				d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{reference})
				return validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
			},
			reason: "UnresolvedDependency",
		},
		{
			name: "child digest mismatch",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				d := dependencyPlanForTest("oneks-d", "chart-d", nil)
				app := validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d})
				app.Spec.DependencyPlans[0].Release.Version = "2.0.0"
				refreshPlanV1Alpha2TestDigest(app)
				return app
			},
			reason: "InvalidDependencyPlanDigest",
		},
		{
			name: "direct cycle",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				d := dependencyPlanForTest("oneks-d", "chart-d", nil)
				d.PlanDigest = validDigestA
				d.Dependencies = []applicationv1.DependencyReference{{Name: d.Name, CatalogueChartID: d.CatalogueChartID, PlanDigest: validDigestA}}
				return validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d})
			},
			reason: "DependencyCycle",
		},
		{
			name: "indirect cycle",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				d := dependencyPlanForTest("oneks-d", "chart-d", nil)
				e := dependencyPlanForTest("oneks-e", "chart-e", nil)
				d.PlanDigest = validDigestA
				e.PlanDigest = validDigestB
				d.Dependencies = []applicationv1.DependencyReference{{Name: e.Name, CatalogueChartID: e.CatalogueChartID, PlanDigest: validDigestB}}
				e.Dependencies = []applicationv1.DependencyReference{{Name: d.Name, CatalogueChartID: d.CatalogueChartID, PlanDigest: validDigestA}}
				return validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
			},
			reason: "DependencyCycle",
		},
		{
			name: "orphan plan",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				d := dependencyPlanForTest("oneks-d", "chart-d", nil)
				e := dependencyPlanForTest("oneks-e", "chart-e", nil)
				return validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
			},
			reason: "OrphanDependencyPlan",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := test.build(t)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != test.reason {
				t.Fatalf("got error %#v, want reason %s", err, test.reason)
			}
		})
	}
}

func TestPlanV1Alpha2ProducerLabelsAreRoleAware(t *testing.T) {
	root := validPlanV1Alpha2Root(t)
	if root.Labels[LabelRootManagedBy] != RootManagedByValue || root.Labels[LabelProducer] != ProducerValue || root.Labels[LabelRole] != RootRoleValue {
		t.Fatalf("Root producer labels are wrong: %#v", root.Labels)
	}
	if err := ValidatePlan(root, ValidationConfig{ClusterID: root.Spec.ClusterID}); err != nil {
		t.Fatalf("Root producer labels rejected: %v", err)
	}

	dependency := validPlanV1Alpha2Dependency(t)
	if dependency.Labels[LabelRootManagedBy] != ManagedByValue || dependency.Labels[LabelProducer] != ManagedByValue || dependency.Labels[LabelRole] != DependencyRoleValue {
		t.Fatalf("Dependency producer labels are wrong: %#v", dependency.Labels)
	}
	if err := ValidatePlan(dependency, ValidationConfig{ClusterID: dependency.Spec.ClusterID}); err != nil {
		t.Fatalf("Dependency producer labels rejected: %v", err)
	}

	dependency.Labels[LabelRootManagedBy] = RootManagedByValue
	dependency.Labels[LabelProducer] = ProducerValue
	dependency.Labels[LabelRole] = RootRoleValue
	if err := ValidatePlan(dependency, ValidationConfig{ClusterID: dependency.Spec.ClusterID}); err == nil || err.Reason != "InvalidProducerLabels" {
		t.Fatalf("Dependency accepted Root producer labels: %#v", err)
	}

	root.Labels[LabelRootManagedBy] = ManagedByValue
	root.Labels[LabelProducer] = ManagedByValue
	root.Labels[LabelRole] = DependencyRoleValue
	if err := ValidatePlan(root, ValidationConfig{ClusterID: root.Spec.ClusterID}); err == nil || err.Reason != "InvalidProducerLabels" {
		t.Fatalf("Root accepted Dependency producer labels: %#v", err)
	}
}

func TestPlanV1Alpha2AcceptsHTTPSAndOCIReleases(t *testing.T) {
	httpsDependency := validPlanV1Alpha2Dependency(t)
	if err := ValidatePlan(httpsDependency, ValidationConfig{ClusterID: httpsDependency.Spec.ClusterID}); err != nil {
		t.Fatalf("HTTPS Dependency release rejected: %v", err)
	}

	ociDependency := validPlanV1Alpha2Dependency(t)
	ociDependency.Spec.Release.RepositoryURL = ""
	ociDependency.Spec.Release.Chart = "oci://registry.example.test/oneks/prometheus"
	refreshPlanV1Alpha2TestDigest(ociDependency)
	if err := ValidatePlan(ociDependency, ValidationConfig{ClusterID: ociDependency.Spec.ClusterID}); err != nil {
		t.Fatalf("OCI Dependency release rejected: %v", err)
	}
	helm := desiredHelmChart(ociDependency)
	helmSpec, _, err := unstructured.NestedMap(helm.Object, "spec")
	if err != nil {
		t.Fatalf("read desired OCI HelmChart spec: %v", err)
	}
	if _, exists := helmSpec["repo"]; exists {
		t.Fatalf("OCI HelmChart contains an empty repo field: %#v", helmSpec)
	}

	ociPlan := dependencyPlanForTest("oneks-oci", "oci-chart", nil)
	ociPlan.Release.RepositoryURL = ""
	ociPlan.Release.Chart = "oci://registry.example.test/oneks/dependency"
	refreshDependencyPlanDigestForTest("42", &ociPlan)
	root := validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(ociPlan)}, []applicationv1.DependencyPlan{ociPlan})
	if err := ValidatePlan(root, ValidationConfig{ClusterID: root.Spec.ClusterID}); err != nil {
		t.Fatalf("OCI dependencyPlan release rejected: %v", err)
	}
}

func TestPlanV1Alpha2RejectsInvalidReleaseSourceCombinations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*applicationv1.ReleaseSpec)
		reason string
	}{
		{"empty repository with non OCI chart", func(release *applicationv1.ReleaseSpec) { release.RepositoryURL = "" }, "InvalidReleaseSource"},
		{"OCI chart with repository", func(release *applicationv1.ReleaseSpec) {
			release.Chart = "oci://registry.example.test/oneks/prometheus"
		}, "InvalidReleaseSource"},
		{"non HTTPS repository", func(release *applicationv1.ReleaseSpec) { release.RepositoryURL = "http://charts.example.test" }, "InvalidRepositoryURL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := validPlanV1Alpha2Dependency(t)
			test.mutate(&app.Spec.Release)
			refreshPlanV1Alpha2TestDigest(app)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != test.reason {
				t.Fatalf("got error %#v, want reason %s", err, test.reason)
			}
		})
	}
}

func TestPlanV1Alpha2RejectsInvalidRoleNamespaceAndDependencyContracts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplication)
		reason string
	}{
		{
			name: "missing role",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Role = ""
			},
			reason: "InvalidApplicationRole",
		},
		{
			name: "root namespace",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Role = applicationv1.ApplicationRoleRoot
				app.Spec.Release.TargetNamespace = "monitoring"
				app.Spec.Release.CreateNamespace = false
			},
			reason: "InvalidTargetNamespace",
		},
		{
			name: "dependency plans on dependency",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.DependencyPlans = []applicationv1.DependencyPlan{validDependencyPlan()}
			},
			reason: "InvalidDependencyPlans",
		},
		{
			name: "resources with namespace creation",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Resources = []applicationv1.ResourceSpec{{
					ID: "config", APIVersion: "v1", Kind: "ConfigMap", Namespace: "monitoring",
					Name: "prometheus-config", Data: map[string]string{}, DeletionPolicy: applicationv1.DeletionPolicyDelete,
				}}
			},
			reason: "InvalidDependencyResources",
		},
		{
			name: "invalid dependency name",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Dependencies = []applicationv1.DependencyReference{{
					Name: "Not_DNS", CatalogueChartID: "alertmanager", PlanDigest: validDependencyDigest(),
				}}
			},
			reason: "InvalidDependencyName",
		},
		{
			name: "invalid dependency chart ID",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Dependencies = []applicationv1.DependencyReference{{
					Name: "oneks-alertmanager", CatalogueChartID: "invalid/chart", PlanDigest: validDependencyDigest(),
				}}
			},
			reason: "InvalidDependencyCatalogueChartID",
		},
		{
			name: "invalid dependency digest",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Dependencies = []applicationv1.DependencyReference{{
					Name: "oneks-alertmanager", CatalogueChartID: "alertmanager", PlanDigest: "sha256-short",
				}}
			},
			reason: "InvalidDependencyPlanDigest",
		},
		{
			name: "duplicate dependency reference names",
			mutate: func(app *applicationv1.OneKSApplication) {
				dependency := validDependencyReference()
				app.Spec.Dependencies = []applicationv1.DependencyReference{dependency, dependency}
			},
			reason: "DuplicateDependencyName",
		},
		{
			name: "self dependency",
			mutate: func(app *applicationv1.OneKSApplication) {
				dependency := validDependencyReference()
				dependency.Name = app.Name
				app.Spec.Dependencies = []applicationv1.DependencyReference{dependency}
			},
			reason: "SelfDependency",
		},
		{
			name: "duplicate dependency plan names",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Role = applicationv1.ApplicationRoleRoot
				app.Spec.Release.TargetNamespace = WorkloadNamespace
				app.Spec.Release.CreateNamespace = false
				plan := validDependencyPlan()
				app.Spec.DependencyPlans = []applicationv1.DependencyPlan{plan, plan}
			},
			reason: "DuplicateDependencyPlanName",
		},
		{
			name: "unresolved root dependency",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Role = applicationv1.ApplicationRoleRoot
				app.Spec.Release.TargetNamespace = WorkloadNamespace
				app.Spec.Release.CreateNamespace = false
				app.Spec.Dependencies = []applicationv1.DependencyReference{validDependencyReference()}
			},
			reason: "UnresolvedDependency",
		},
		{
			name: "mismatched root dependency plan",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Role = applicationv1.ApplicationRoleRoot
				app.Spec.Release.TargetNamespace = WorkloadNamespace
				app.Spec.Release.CreateNamespace = false
				app.Spec.Dependencies = []applicationv1.DependencyReference{validDependencyReference()}
				plan := validDependencyPlan()
				plan.PlanDigest = "sha256-" + strings.Repeat("B", 43)
				app.Spec.DependencyPlans = []applicationv1.DependencyPlan{plan}
			},
			reason: "UnresolvedDependency",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := validPlanV1Alpha2Dependency(t)
			test.mutate(app)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != test.reason {
				t.Fatalf("got error %#v, want reason %s", err, test.reason)
			}
		})
	}
}

func TestPlanV1Alpha2DependencyFieldsAffectDigestWithoutArraySorting(t *testing.T) {
	app := validPlanV1Alpha2Root(t)
	canonical, err := CanonicalPlan(app.Spec)
	if err != nil {
		t.Fatalf("canonicalize base plan: %v", err)
	}
	baseDigest := digestForPlanV1Alpha2Test(canonical)

	mutations := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplicationSpec)
	}{
		{"role", func(spec *applicationv1.OneKSApplicationSpec) { spec.Role = applicationv1.ApplicationRoleDependency }},
		{"dependency", func(spec *applicationv1.OneKSApplicationSpec) {
			spec.Dependencies[0].CatalogueChartID = "alertmanager-v2"
		}},
		{"dependency plan", func(spec *applicationv1.OneKSApplicationSpec) { spec.DependencyPlans[0].Release.Version = "2.0.0" }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			spec := *app.Spec.DeepCopy()
			test.mutate(&spec)
			changed, err := CanonicalPlan(spec)
			if err != nil {
				t.Fatalf("canonicalize changed plan: %v", err)
			}
			if got := digestForPlanV1Alpha2Test(changed); got == baseDigest {
				t.Fatalf("%s did not affect the v1alpha2 digest", test.name)
			}
		})
	}

	spec := *app.Spec.DeepCopy()
	secondReference := applicationv1.DependencyReference{
		Name: "oneks-grafana", CatalogueChartID: "grafana", PlanDigest: "sha256-" + strings.Repeat("C", 43),
	}
	spec.Dependencies = append(spec.Dependencies, secondReference)
	forward, err := CanonicalPlan(spec)
	if err != nil {
		t.Fatalf("canonicalize forward dependencies: %v", err)
	}
	spec.Dependencies[0], spec.Dependencies[1] = spec.Dependencies[1], spec.Dependencies[0]
	reversed, err := CanonicalPlan(spec)
	if err != nil {
		t.Fatalf("canonicalize reversed dependencies: %v", err)
	}
	if bytes.Equal(forward, reversed) {
		t.Fatal("dependency array order was not preserved by canonicalization")
	}
}

func TestCanonicalPlanRejectsUnsupportedVersion(t *testing.T) {
	spec := validPlanV1Alpha2Dependency(t).Spec
	spec.PlanVersion = "oneks.opennebula.io/plan-v9"
	if _, err := CanonicalPlan(spec); err == nil {
		t.Fatal("unsupported plan version was canonicalized")
	}
	app := validPlanV1Alpha2Dependency(t)
	app.Spec.PlanVersion = "oneks.opennebula.io/plan-v9"
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "UnsupportedPlanVersion" {
		t.Fatalf("got error %#v, want UnsupportedPlanVersion", err)
	}
}

func TestPlanV1Alpha2NamespacePrecheckUsesTargetAndSkipsCreation(t *testing.T) {
	ctx := context.Background()
	missing := validPlanV1Alpha2Dependency(t)
	missing.Spec.Release.CreateNamespace = false
	refreshPlanV1Alpha2TestDigest(missing)
	reconciler, _ := testReconciler(t, missing)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: missing.Namespace, Name: missing.Name}}); err != nil {
		t.Fatalf("reconcile missing monitoring namespace: %v", err)
	}
	stored := &applicationv1.OneKSApplication{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: missing.Namespace, Name: missing.Name}, stored); err != nil {
		t.Fatalf("get application: %v", err)
	}
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "TargetNamespaceMissing" || !strings.Contains(stored.Status.LastError.Message, "monitoring") {
		t.Fatalf("missing target namespace status did not name monitoring: %#v", stored.Status.LastError)
	}

	creating := validPlanV1Alpha2Dependency(t)
	reconciler, _ = testReconciler(t, creating)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: creating.Namespace, Name: creating.Name}}); err != nil {
		t.Fatalf("reconcile namespace-creating dependency: %v", err)
	}
	stored = &applicationv1.OneKSApplication{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: creating.Namespace, Name: creating.Name}, stored); err != nil {
		t.Fatalf("get namespace-creating application: %v", err)
	}
	if stored.Status.LastError != nil && stored.Status.LastError.Reason == "TargetNamespaceMissing" {
		t.Fatalf("namespace existence was checked despite createNamespace=true: %#v", stored.Status.LastError)
	}
}

func validPlanV1Alpha2Dependency(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := &applicationv1.OneKSApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oneks-prometheus", Namespace: applicationv1.ApplicationNamespace,
			UID: types.UID("uid-oneks-prometheus"),
		},
		Spec: applicationv1.OneKSApplicationSpec{
			ClusterID: "42", CatalogueChartID: "prometheus", PlanVersion: applicationv1.PlanVersionV1Alpha2,
			ExecutionMode: applicationv1.ExecutionModeExecute, Role: applicationv1.ApplicationRoleDependency,
			Release: applicationv1.ReleaseSpec{
				ChartID: "prometheus", RepositoryURL: "https://prometheus-community.github.io/helm-charts",
				Chart: "kube-prometheus-stack", Version: "87.12.2", ReleaseName: "oneks-prometheus",
				TargetNamespace: "monitoring", CreateNamespace: true, ValuesContent: "grafana:\n  enabled: false\n",
			},
			Resources: []applicationv1.ResourceSpec{}, DeletionPolicy: applicationv1.DeletionPolicyDelete,
		},
	}
	refreshPlanV1Alpha2TestDigest(app)
	app.Labels = producerLabels(app)
	return app
}

func planV1Alpha1FixtureApplication(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	payload, err := os.ReadFile("testdata/oneks_application_plan_golden.json")
	if err != nil {
		t.Fatalf("read plan-v1alpha1 fixture: %v", err)
	}
	app := &applicationv1.OneKSApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oneks-prometheus", Namespace: applicationv1.ApplicationNamespace,
			UID: types.UID("uid-oneks-prometheus"),
		},
	}
	if err := json.Unmarshal(payload, &app.Spec); err != nil {
		t.Fatalf("decode plan-v1alpha1 fixture: %v", err)
	}
	app.Spec.PlanDigest = planV1Alpha1GoldenDigest
	app.Labels = producerLabels(app)
	return app
}

func validPlanV1Alpha2Root(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	plan := validDependencyPlan()
	return validPlanV1Alpha2RootGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
}

func validDependencyReference() applicationv1.DependencyReference {
	return dependencyReferenceForPlan(validDependencyPlan())
}

func validDependencyPlan() applicationv1.DependencyPlan {
	plan := applicationv1.DependencyPlan{
		Name: "oneks-alertmanager", CatalogueChartID: "alertmanager", PlanDigest: validDependencyDigest(),
		Release: applicationv1.ReleaseSpec{
			ChartID: "alertmanager", RepositoryURL: "https://prometheus-community.github.io/helm-charts",
			Chart: "alertmanager", Version: "1.0.0", ReleaseName: "oneks-alertmanager",
			TargetNamespace: "monitoring", CreateNamespace: true, ValuesContent: "{}\n",
		},
		Resources: []applicationv1.ResourceSpec{}, Dependencies: []applicationv1.DependencyReference{},
		DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
	refreshDependencyPlanDigestForTest("42", &plan)
	return plan
}

func validPlanV1Alpha2RootGraph(t *testing.T, dependencies []applicationv1.DependencyReference, plans []applicationv1.DependencyPlan) *applicationv1.OneKSApplication {
	t.Helper()
	app := validPlanV1Alpha2Dependency(t)
	app.Name = "oneks-root"
	app.UID = types.UID("uid-oneks-root")
	app.Spec.Role = applicationv1.ApplicationRoleRoot
	app.Spec.Release.TargetNamespace = WorkloadNamespace
	app.Spec.Release.CreateNamespace = false
	app.Spec.Dependencies = dependencies
	app.Spec.DependencyPlans = plans
	refreshPlanV1Alpha2TestDigest(app)
	app.Labels = producerLabels(app)
	return app
}

func dependencyPlanForTest(name, catalogueChartID string, dependencies []applicationv1.DependencyReference) applicationv1.DependencyPlan {
	plan := applicationv1.DependencyPlan{
		Name: name, CatalogueChartID: catalogueChartID,
		Release: applicationv1.ReleaseSpec{
			ChartID: catalogueChartID, RepositoryURL: "https://charts.example.test",
			Chart: catalogueChartID, Version: "1.0.0", ReleaseName: name,
			TargetNamespace: "monitoring", CreateNamespace: true, ValuesContent: "{}\n",
		},
		Resources: []applicationv1.ResourceSpec{}, Dependencies: dependencies,
		DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
	refreshDependencyPlanDigestForTest("42", &plan)
	return plan
}

func dependencyReferenceForPlan(plan applicationv1.DependencyPlan) applicationv1.DependencyReference {
	return applicationv1.DependencyReference{
		Name: plan.Name, CatalogueChartID: plan.CatalogueChartID, PlanDigest: plan.PlanDigest,
	}
}

func validDependencyDigest() string {
	return "sha256-" + strings.Repeat("A", 43)
}

func refreshPlanV1Alpha2TestDigest(app *applicationv1.OneKSApplication) {
	canonical, err := CanonicalPlan(app.Spec)
	if err != nil {
		panic(err)
	}
	app.Spec.PlanDigest = digestForPlanV1Alpha2Test(canonical)
	if app.Labels != nil {
		app.Labels[LabelPlanDigest] = app.Spec.PlanDigest
	}
}

func refreshDependencyPlanDigestForTest(clusterID string, plan *applicationv1.DependencyPlan) {
	canonical, err := canonicalPlanV1Alpha2(dependencyPlanChildSpec(clusterID, *plan))
	if err != nil {
		panic(err)
	}
	plan.PlanDigest = digestForPlanV1Alpha2Test(canonical)
}

func digestForPlanV1Alpha2Test(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return "sha256-" + base64.RawURLEncoding.EncodeToString(sum[:])
}
