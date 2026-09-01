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
	"os"
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"
)

func TestGeneratedCRDUsesOnlyCurrentPlanVersion(t *testing.T) {
	payload := generatedApplicationCRD(t)
	text := string(payload)
	for _, required := range []string{
		"- oneks.opennebula.io/plan-v1alpha5",
		"plan-v1alpha5 requires role",
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
	specSchema := external.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if _, exists := specSchema.Properties["resources"]; exists {
		t.Fatal("generated current plan schema still exposes removed resources")
	}
	secretInput := specSchema.Properties["secretInputRef"]
	if _, exists := secretInput.Properties["uid"]; exists {
		t.Fatal("generated current plan schema still exposes spec secretInputRef.uid")
	}
	dependencyPlans := specSchema.Properties["dependencyPlans"]
	if _, exists := dependencyPlans.Items.Schema.Properties["resources"]; exists {
		t.Fatal("generated dependency plan schema still exposes removed resources")
	}
	internal := &apiextensions.CustomResourceDefinition{}
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(external, internal, nil); err != nil {
		t.Fatalf("convert generated OneKSApplication CRD: %v", err)
	}
	internal.Status.StoredVersions = []string{"v1alpha5"}
	if errors := apiextensionsvalidation.ValidateCustomResourceDefinition(context.Background(), internal); len(errors) != 0 {
		t.Fatalf("generated OneKSApplication CRD is invalid: %v", errors.ToAggregate())
	}
}

func generatedApplicationCRD(t *testing.T) []byte {
	t.Helper()
	payload, err := os.ReadFile("../../config/crd/bases/oneks.opennebula.io_oneksapplications.yaml")
	if err != nil {
		t.Fatalf("read generated OneKSApplication CRD: %v", err)
	}
	return payload
}

func TestStatusAdvertisesSupportedPlanVersions(t *testing.T) {
	app := validDependencyPlanApplication(t)
	status := baseStatus(app)
	want := []string{applicationv1.PlanVersion}
	if len(status.SupportedPlanVersions) != len(want) {
		t.Fatalf("supported versions = %#v, want %#v", status.SupportedPlanVersions, want)
	}
	for index := range want {
		if status.SupportedPlanVersions[index] != want[index] {
			t.Fatalf("supported versions = %#v, want %#v", status.SupportedPlanVersions, want)
		}
	}
}

func TestCurrentPlanDependencyPrometheusCanCreateMonitoringNamespace(t *testing.T) {
	app := validDependencyPlanApplication(t)
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
		t.Fatalf("valid current plan Dependency rejected: %v", err)
	}
}

func TestCurrentPlanAcceptsCompleteDependencyDAGs(t *testing.T) {
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
			app := validRootPlanGraph(t, test.dependencies, test.plans)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
				t.Fatalf("valid dependency DAG rejected: %v", err)
			}
		})
	}
}

func TestDependencyApplicationNameContract(t *testing.T) {
	const releaseName = "oneks-prometheus"
	const expected = "oneks-dep-oneks-prometheus-1cba8adc21bd540f9773"

	if got := dependencyApplicationName(releaseName); got != expected {
		t.Fatalf("fixed dependency application name = %q, want %q", got, expected)
	}
	if first, second := dependencyApplicationName(releaseName), dependencyApplicationName(releaseName); first != second {
		t.Fatalf("same releaseName produced different names: %q and %q", first, second)
	}
	if first, second := dependencyApplicationName("oneks-prometheus"), dependencyApplicationName("oneks-grafana"); first == second {
		t.Fatalf("different releaseNames produced the same name %q", first)
	}

	longName := dependencyApplicationName(strings.Repeat("a", 63))
	if len(longName) > 63 {
		t.Fatalf("dependency application name has %d characters: %q", len(longName), longName)
	}
	if errors := validation.IsDNS1123Label(longName); len(errors) != 0 {
		t.Fatalf("dependency application name %q is not DNS-1123: %v", longName, errors)
	}
}

func TestRootRejectsArbitraryDependencyApplicationName(t *testing.T) {
	plan := dependencyPlanForTest("shared-release", "shared-chart", nil)
	digestBeforeRename := plan.PlanDigest
	plan.Name = "arbitrary-dependency"
	app := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "InvalidDependencyApplicationName" {
		t.Fatalf("got error %#v, want InvalidDependencyApplicationName", err)
	}
	canonical, err := CanonicalPlan(dependencyPlanChildSpec("42", plan))
	if err != nil {
		t.Fatalf("canonicalize renamed dependency child: %v", err)
	}
	if got := Digest(canonical); got != digestBeforeRename {
		t.Fatalf("metadata name changed child spec digest: got %s, want %s", got, digestBeforeRename)
	}
}

func TestSharedReleaseNameCannotEscapeDependencyIdentity(t *testing.T) {
	first := dependencyPlanForTest("shared-release", "shared-chart", nil)
	second := first
	second.Release.TargetNamespace = "other-monitoring"
	second.Release.Version = "2.0.0"
	second.Release.ValuesContent = "mode: other\n"
	refreshDependencyPlanDigestForTest("42", &second)

	firstExpected := dependencyApplicationName(first.Release.ReleaseName)
	secondExpected := dependencyApplicationName(second.Release.ReleaseName)
	if firstExpected != secondExpected || first.Name != firstExpected || second.Name != secondExpected {
		t.Fatalf("same releaseName escaped shared identity: first=%q second=%q", firstExpected, secondExpected)
	}
	if first.PlanDigest == second.PlanDigest {
		t.Fatal("changed targetNamespace/version/values did not change child digest")
	}

	app := validRootPlanGraph(
		t,
		[]applicationv1.DependencyReference{dependencyReferenceForPlan(first)},
		[]applicationv1.DependencyPlan{first, second},
	)
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "DuplicateDependencyPlanName" {
		t.Fatalf("same releaseName escaped duplicate identity validation: %#v", err)
	}
}

func TestCurrentPlanDependencyRejectsArbitraryMetadataName(t *testing.T) {
	app := validDependencyPlanApplication(t)
	app.Name = "arbitrary-dependency"
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "InvalidDependencyApplicationName" {
		t.Fatalf("got error %#v, want InvalidDependencyApplicationName", err)
	}
}

func TestCurrentPlanDependencyMetadataNameUsesOnlyReleaseName(t *testing.T) {
	app := validDependencyPlanApplication(t)
	want := app.Name
	app.Spec.Release.TargetNamespace = "other-monitoring"
	app.Spec.Release.Version = "2.0.0"
	app.Spec.Release.ValuesContent = "mode: other\n"
	refreshPlanDigest(app)

	if got := dependencyApplicationName(app.Spec.Release.ReleaseName); got != want {
		t.Fatalf("changed release fields changed dependency metadata.name: got %q, want %q", got, want)
	}
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
		t.Fatalf("Dependency with unchanged deterministic metadata.name rejected: %v", err)
	}

	app.Name = "other-dependency"
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "InvalidDependencyApplicationName" {
		t.Fatalf("got error %#v, want InvalidDependencyApplicationName", err)
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
		if got := Digest(canonical); got != plan.PlanDigest {
			t.Fatalf("child %s digest = %s, want %s", plan.Name, got, plan.PlanDigest)
		}
		if child.ExecutionMode != applicationv1.ExecutionModeExecute || child.Role != applicationv1.ApplicationRoleDependency || child.DependencyPlans != nil {
			t.Fatalf("child %s materialized with wrong fixed fields: %#v", plan.Name, child)
		}
	}
}

func TestCurrentPlanRejectsInvalidDependencyGraphs(t *testing.T) {
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
				return validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d})
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
				return validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
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
				return validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
			},
			reason: "UnresolvedDependency",
		},
		{
			name: "child digest mismatch",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				d := dependencyPlanForTest("oneks-d", "chart-d", nil)
				app := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d})
				app.Spec.DependencyPlans[0].Release.Version = "2.0.0"
				refreshPlanDigest(app)
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
				return validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d})
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
				return validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
			},
			reason: "DependencyCycle",
		},
		{
			name: "orphan plan",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				d := dependencyPlanForTest("oneks-d", "chart-d", nil)
				e := dependencyPlanForTest("oneks-e", "chart-e", nil)
				return validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d, e})
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

func TestCurrentPlanProducerLabelsAreRoleAware(t *testing.T) {
	root := validRootPlan(t)
	if root.Labels[LabelRootManagedBy] != RootManagedByValue || root.Labels[LabelProducer] != ProducerValue || root.Labels[LabelRole] != RootRoleValue {
		t.Fatalf("Root producer labels are wrong: %#v", root.Labels)
	}
	if err := ValidatePlan(root, ValidationConfig{ClusterID: root.Spec.ClusterID}); err != nil {
		t.Fatalf("Root producer labels rejected: %v", err)
	}

	dependency := validDependencyPlanApplication(t)
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

func TestCurrentPlanAcceptsHTTPSAndOCIReleases(t *testing.T) {
	httpsDependency := validDependencyPlanApplication(t)
	if err := ValidatePlan(httpsDependency, ValidationConfig{ClusterID: httpsDependency.Spec.ClusterID}); err != nil {
		t.Fatalf("HTTPS Dependency release rejected: %v", err)
	}

	ociDependency := validDependencyPlanApplication(t)
	ociDependency.Spec.Release.RepositoryURL = ""
	ociDependency.Spec.Release.Chart = "oci://registry.example.test/oneks/prometheus"
	refreshPlanDigest(ociDependency)
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
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(ociPlan)}, []applicationv1.DependencyPlan{ociPlan})
	if err := ValidatePlan(root, ValidationConfig{ClusterID: root.Spec.ClusterID}); err != nil {
		t.Fatalf("OCI dependencyPlan release rejected: %v", err)
	}
}

func TestCurrentPlanRejectsInvalidReleaseSourceCombinations(t *testing.T) {
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
			app := validDependencyPlanApplication(t)
			test.mutate(&app.Spec.Release)
			refreshPlanDigest(app)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != test.reason {
				t.Fatalf("got error %#v, want reason %s", err, test.reason)
			}
		})
	}
}

func TestCurrentPlanRejectsInvalidRoleNamespaceAndDependencyContracts(t *testing.T) {
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
			name: "invalid root namespace",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Role = applicationv1.ApplicationRoleRoot
				app.Spec.Release.TargetNamespace = "Monitoring"
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
				app.Spec.Release.TargetNamespace = "catalogue-workloads"
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
				app.Spec.Release.TargetNamespace = "catalogue-workloads"
				app.Spec.Release.CreateNamespace = false
				app.Spec.Dependencies = []applicationv1.DependencyReference{validDependencyReference()}
			},
			reason: "UnresolvedDependency",
		},
		{
			name: "mismatched root dependency plan",
			mutate: func(app *applicationv1.OneKSApplication) {
				app.Spec.Role = applicationv1.ApplicationRoleRoot
				app.Spec.Release.TargetNamespace = "catalogue-workloads"
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
			app := validDependencyPlanApplication(t)
			test.mutate(app)
			if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != test.reason {
				t.Fatalf("got error %#v, want reason %s", err, test.reason)
			}
		})
	}
}

func TestCurrentPlanDependencyFieldsAffectDigestWithoutArraySorting(t *testing.T) {
	app := validRootPlan(t)
	canonical, err := CanonicalPlan(app.Spec)
	if err != nil {
		t.Fatalf("canonicalize base plan: %v", err)
	}
	baseDigest := Digest(canonical)

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
			if got := Digest(changed); got == baseDigest {
				t.Fatalf("%s did not affect the current plan digest", test.name)
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
	spec := validDependencyPlanApplication(t).Spec
	spec.PlanVersion = "oneks.opennebula.io/plan-v9"
	if _, err := CanonicalPlan(spec); err == nil {
		t.Fatal("unsupported plan version was canonicalized")
	}
	app := validDependencyPlanApplication(t)
	app.Spec.PlanVersion = "oneks.opennebula.io/plan-v9"
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err == nil || err.Reason != "UnsupportedPlanVersion" {
		t.Fatalf("got error %#v, want UnsupportedPlanVersion", err)
	}
}

func TestCurrentPlanNamespacePrecheckUsesTargetAndSkipsCreation(t *testing.T) {
	ctx := context.Background()
	missing := validDependencyPlanApplication(t)
	missing.Spec.Release.CreateNamespace = false
	refreshPlanDigest(missing)
	reconciler, _ := testReconciler(t, missing)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: missing.Namespace, Name: missing.Name}}); err != nil {
		t.Fatalf("reconcile missing monitoring namespace: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: missing.Namespace, Name: missing.Name}}); err != nil {
		t.Fatalf("reconcile missing monitoring namespace after finalizer: %v", err)
	}
	stored := &applicationv1.OneKSApplication{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: missing.Namespace, Name: missing.Name}, stored); err != nil {
		t.Fatalf("get application: %v", err)
	}
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "TargetNamespaceMissing" || !strings.Contains(stored.Status.LastError.Message, "monitoring") {
		t.Fatalf("missing target namespace status did not name monitoring: %#v", stored.Status.LastError)
	}

	creating := validDependencyPlanApplication(t)
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

func validDependencyPlanApplication(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := &applicationv1.OneKSApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name: dependencyApplicationName("oneks-prometheus"), Namespace: applicationv1.ApplicationNamespace,
			UID: types.UID("uid-oneks-prometheus"),
		},
		Spec: applicationv1.OneKSApplicationSpec{
			ClusterID: "42", CatalogueChartID: "prometheus", PlanVersion: applicationv1.PlanVersion,
			ExecutionMode: applicationv1.ExecutionModeExecute, Role: applicationv1.ApplicationRoleDependency,
			Release: applicationv1.ReleaseSpec{
				ChartID: "prometheus", RepositoryURL: "https://prometheus-community.github.io/helm-charts",
				Chart: "kube-prometheus-stack", Version: "87.12.2", ReleaseName: "oneks-prometheus",
				TargetNamespace: "monitoring", CreateNamespace: true, ValuesContent: "grafana:\n  enabled: false\n",
			},
			DeletionPolicy: applicationv1.DeletionPolicyDelete,
		},
	}
	refreshPlanDigest(app)
	app.Labels = producerLabels(app)
	return app
}

func validRootPlan(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	plan := validDependencyPlan()
	return validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
}

func validDependencyReference() applicationv1.DependencyReference {
	return dependencyReferenceForPlan(validDependencyPlan())
}

func validDependencyPlan() applicationv1.DependencyPlan {
	plan := applicationv1.DependencyPlan{
		Name: dependencyApplicationName("oneks-alertmanager"), CatalogueChartID: "alertmanager", PlanDigest: validDependencyDigest(),
		Release: applicationv1.ReleaseSpec{
			ChartID: "alertmanager", RepositoryURL: "https://prometheus-community.github.io/helm-charts",
			Chart: "alertmanager", Version: "1.0.0", ReleaseName: "oneks-alertmanager",
			TargetNamespace: "monitoring", CreateNamespace: true, ValuesContent: "{}\n",
		},
		Dependencies:   []applicationv1.DependencyReference{},
		DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
	refreshDependencyPlanDigestForTest("42", &plan)
	return plan
}

func validRootPlanGraph(t *testing.T, dependencies []applicationv1.DependencyReference, plans []applicationv1.DependencyPlan) *applicationv1.OneKSApplication {
	t.Helper()
	app := validDependencyPlanApplication(t)
	app.Name = "oneks-root"
	app.UID = types.UID("uid-oneks-root")
	app.Spec.Role = applicationv1.ApplicationRoleRoot
	app.Spec.Release.TargetNamespace = "catalogue-workloads"
	app.Spec.Release.CreateNamespace = false
	app.Spec.Dependencies = dependencies
	app.Spec.DependencyPlans = plans
	refreshPlanDigest(app)
	app.Labels = producerLabels(app)
	return app
}

func dependencyPlanForTest(releaseName, catalogueChartID string, dependencies []applicationv1.DependencyReference) applicationv1.DependencyPlan {
	plan := applicationv1.DependencyPlan{
		Name: dependencyApplicationName(releaseName), CatalogueChartID: catalogueChartID,
		Release: applicationv1.ReleaseSpec{
			ChartID: catalogueChartID, RepositoryURL: "https://charts.example.test",
			Chart: catalogueChartID, Version: "1.0.0", ReleaseName: releaseName,
			TargetNamespace: "monitoring", CreateNamespace: true, ValuesContent: "{}\n",
		},
		Dependencies:   dependencies,
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

func refreshPlanDigest(app *applicationv1.OneKSApplication) {
	canonical, err := CanonicalPlan(app.Spec)
	if err != nil {
		panic(err)
	}
	app.Spec.PlanDigest = Digest(canonical)
	if app.Labels != nil {
		app.Labels[LabelPlanDigest] = app.Spec.PlanDigest
	}
}

func refreshDependencyPlanDigestForTest(clusterID string, plan *applicationv1.DependencyPlan) {
	canonical, err := canonicalPlan(dependencyPlanChildSpec(clusterID, *plan))
	if err != nil {
		panic(err)
	}
	plan.PlanDigest = Digest(canonical)
}
