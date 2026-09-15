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
	"context"
	"os"
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

func TestGeneratedCRDUsesOnlyCurrentPlanVersion(t *testing.T) {
	payload := generatedApplicationCRD(t)
	text := string(payload)
	for _, required := range []string{
		"- oneks.opennebula.io/plan-v1beta1",
		"plan-v1beta1 requires role",
		"each direct Root dependency must resolve to exactly one matching",
		"top-level uninstall is permitted only for Dependency applications",
		"dependency plans do not permit release.authSecret",
		"secretInputUID:",
		"patchJSON:",
		"maxLength: 16384",
		"maxItems: 8",
		"- kubernetesPatch",
		"- merge",
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
	if len(external.Spec.Versions) != 1 || external.Spec.Versions[0].Name != "v1beta1" || !external.Spec.Versions[0].Storage || !external.Spec.Versions[0].Served {
		t.Fatal("CRD must serve and store only v1beta1")
	}
	specSchema := external.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if _, exists := specSchema.Properties["planDigest"]; exists {
		t.Fatal("generated current plan schema still exposes planDigest")
	}
	immutable := false
	for _, validation := range specSchema.XValidations {
		if validation.Rule == "self == oldSelf" {
			immutable = true
			break
		}
	}
	if !immutable {
		t.Fatal("generated current plan schema does not make spec immutable")
	}
	boundedLists := map[string]apiextensionsv1.JSONSchemaProps{
		"dependencies":                 specSchema.Properties["dependencies"],
		"dependencyPlans":              specSchema.Properties["dependencyPlans"],
		"managedResources":             specSchema.Properties["managedResources"],
		"protectedSecrets":             specSchema.Properties["protectedSecrets"],
		"dependencyPlans.dependencies": specSchema.Properties["dependencyPlans"].Items.Schema.Properties["dependencies"],
		"managedResources.dependsOn":   specSchema.Properties["managedResources"].Items.Schema.Properties["dependsOn"],
	}
	for path, schema := range boundedLists {
		if schema.MaxItems == nil || *schema.MaxItems != 16 {
			t.Fatalf("generated current plan schema %s maxItems = %v, want 16", path, schema.MaxItems)
		}
	}
	combinedBound := false
	for _, validation := range specSchema.XValidations {
		if validation.Rule == "!has(self.managedResources) || !has(self.protectedSecrets) || size(self.managedResources) + size(self.protectedSecrets) <= 16" {
			combinedBound = true
			break
		}
	}
	if !combinedBound {
		t.Fatal("generated current plan schema does not bound combined managedResources and protectedSecrets")
	}
	if strings.Contains(text, "self.release.targetNamespace == '") {
		t.Fatal("generated current plan schema retains a fixed workload namespace rule")
	}
	if strings.Count(text, "uninstall:") < 2 {
		t.Fatal("generated current plan schema does not expose uninstall on both dependency representations")
	}
	if _, exists := specSchema.Properties["resources"]; exists {
		t.Fatal("generated current plan schema still exposes removed resources")
	}
	if _, exists := specSchema.Properties["executionMode"]; exists {
		t.Fatal("CRD still exposes executionMode")
	}
	managed := specSchema.Properties["managedResources"].Items.Schema
	if _, exists := managed.Properties["apiResource"]; exists {
		t.Fatal("CRD still exposes managed apiResource")
	}
	required := managed.Properties["readiness"].Properties["requiredResources"].Items.Schema
	if _, exists := required.Properties["apiResource"]; exists {
		t.Fatal("CRD still exposes readiness apiResource")
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
	internal.Status.StoredVersions = []string{"v1beta1"}
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
			assertPlanValid(t, validRootPlanGraph(t, test.dependencies, test.plans))
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
	plan.Name = "arbitrary-dependency"
	app := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	assertPlanError(t, app, "InvalidDependencyApplicationName")
}

func TestCurrentPlanDependencyRejectsArbitraryMetadataName(t *testing.T) {
	app := validDependencyPlanApplication(t)
	app.Name = "arbitrary-dependency"
	assertPlanError(t, app, "InvalidDependencyApplicationName")
}

func TestCurrentPlanDependencyMetadataNameUsesOnlyReleaseName(t *testing.T) {
	app := validDependencyPlanApplication(t)
	want := app.Name
	app.Spec.Release.TargetNamespace = "other-monitoring"
	app.Spec.Release.Version = "2.0.0"
	app.Spec.Release.ValuesContent = "mode: other\n"

	if got := dependencyApplicationName(app.Spec.Release.ReleaseName); got != want {
		t.Fatalf("changed release fields changed dependency metadata.name: got %q, want %q", got, want)
	}
	assertPlanValid(t, app)

	app.Name = "other-dependency"
	assertPlanError(t, app, "InvalidDependencyApplicationName")
}

func TestDependencyPlanMaterializesFixedChildFields(t *testing.T) {
	e := dependencyPlanForTest("oneks-e", "chart-e", nil)
	d := dependencyPlanForTest("oneks-d", "chart-d", []applicationv1.DependencyReference{dependencyReferenceForPlan(e)})
	for _, plan := range []applicationv1.DependencyPlan{d, e} {
		child := dependencyPlanChildSpec("42", plan)
		if child.Role != applicationv1.ApplicationRoleDependency || child.DependencyPlans != nil {
			t.Fatalf("child %s materialized with wrong fixed fields: %#v", plan.Name, child)
		}
	}
}

func TestCurrentPlanRejectsInvalidDependencyGraphs(t *testing.T) {
	tests := []struct {
		name   string
		build  func(*testing.T) *applicationv1.OneKSApplication
		reason string
	}{
		{
			name: "nested missing plan",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				missing := applicationv1.DependencyReference{Name: "oneks-e", CatalogueChartID: "chart-e"}
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
			name: "direct cycle",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				d := dependencyPlanForTest("oneks-d", "chart-d", nil)
				d.Dependencies = []applicationv1.DependencyReference{{Name: d.Name, CatalogueChartID: d.CatalogueChartID}}
				return validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(d)}, []applicationv1.DependencyPlan{d})
			},
			reason: "DependencyCycle",
		},
		{
			name: "indirect cycle",
			build: func(t *testing.T) *applicationv1.OneKSApplication {
				d := dependencyPlanForTest("oneks-d", "chart-d", nil)
				e := dependencyPlanForTest("oneks-e", "chart-e", nil)
				d.Dependencies = []applicationv1.DependencyReference{{Name: e.Name, CatalogueChartID: e.CatalogueChartID}}
				e.Dependencies = []applicationv1.DependencyReference{{Name: d.Name, CatalogueChartID: d.CatalogueChartID}}
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
			assertPlanError(t, test.build(t), test.reason)
		})
	}
}

func TestCurrentPlanProducerLabelsAreRoleAware(t *testing.T) {
	root := validRootPlan(t)
	if root.Labels[LabelRootManagedBy] != RootManagedByValue || root.Labels[LabelProducer] != ProducerValue || root.Labels[LabelRole] != RootRoleValue {
		t.Fatalf("Root producer labels are wrong: %#v", root.Labels)
	}
	assertPlanValid(t, root)

	dependency := validDependencyPlanApplication(t)
	if dependency.Labels[LabelRootManagedBy] != ManagedByValue || dependency.Labels[LabelProducer] != ManagedByValue || dependency.Labels[LabelRole] != DependencyRoleValue {
		t.Fatalf("Dependency producer labels are wrong: %#v", dependency.Labels)
	}
	assertPlanValid(t, dependency)

}

func TestCurrentPlanAcceptsHTTPSAndOCIReleases(t *testing.T) {
	httpsDependency := validDependencyPlanApplication(t)
	assertPlanValid(t, httpsDependency)

	ociDependency := validDependencyPlanApplication(t)
	ociDependency.Spec.Release.RepositoryURL = ""
	ociDependency.Spec.Release.Chart = "oci://registry.example.test/oneks/prometheus"
	assertPlanValid(t, ociDependency)
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
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(ociPlan)}, []applicationv1.DependencyPlan{ociPlan})
	assertPlanValid(t, root)
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
			assertPlanError(t, app, test.reason)
		})
	}
}

func TestCurrentPlanRejectsUnresolvedDependencyContracts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplication)
		reason string
	}{
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
				app.Spec.DependencyPlans = []applicationv1.DependencyPlan{plan}
				app.Spec.Dependencies[0].CatalogueChartID = "different-chart"
			},
			reason: "UnresolvedDependency",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := validDependencyPlanApplication(t)
			test.mutate(app)
			assertPlanError(t, app, test.reason)
		})
	}
}

func TestCurrentPlanNamespacePrecheckUsesTargetAndSkipsCreation(t *testing.T) {
	ctx := context.Background()
	missing := validDependencyPlanApplication(t)
	missing.Spec.Release.CreateNamespace = false
	reconciler, _ := testReconciler(t, missing)
	reconcileOnce(t, ctx, reconciler, missing)
	reconcileOnce(t, ctx, reconciler, missing)
	stored := &applicationv1.OneKSApplication{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: missing.Namespace, Name: missing.Name}, stored); err != nil {
		t.Fatalf("get application: %v", err)
	}
	if stored.Status.LastError == nil || stored.Status.LastError.Reason != "TargetNamespaceMissing" || !strings.Contains(stored.Status.LastError.Message, "monitoring") {
		t.Fatalf("missing target namespace status did not name monitoring: %#v", stored.Status.LastError)
	}

	creating := validDependencyPlanApplication(t)
	reconciler, _ = testReconciler(t, creating)
	reconcileOnce(t, ctx, reconciler, creating)
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
			Role: applicationv1.ApplicationRoleDependency,
			Release: applicationv1.ReleaseSpec{
				ChartID: "prometheus", RepositoryURL: "https://prometheus-community.github.io/helm-charts",
				Chart: "kube-prometheus-stack", Version: "87.12.2", ReleaseName: "oneks-prometheus",
				TargetNamespace: "monitoring", CreateNamespace: true, ValuesContent: "grafana:\n  enabled: false\n",
			},
			DeletionPolicy: applicationv1.DeletionPolicyDelete,
		},
	}
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
		Name: dependencyApplicationName("oneks-alertmanager"), CatalogueChartID: "alertmanager",
		Release: applicationv1.ReleaseSpec{
			ChartID: "alertmanager", RepositoryURL: "https://prometheus-community.github.io/helm-charts",
			Chart: "alertmanager", Version: "1.0.0", ReleaseName: "oneks-alertmanager",
			TargetNamespace: "monitoring", CreateNamespace: true, ValuesContent: "{}\n",
		},
		Dependencies:   []applicationv1.DependencyReference{},
		DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
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
	return plan
}

func dependencyReferenceForPlan(plan applicationv1.DependencyPlan) applicationv1.DependencyReference {
	return applicationv1.DependencyReference{
		Name: plan.Name, CatalogueChartID: plan.CatalogueChartID,
	}
}
