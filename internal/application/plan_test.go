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
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestCanonicalPlanUsesASCIIAndStableMapOrder(t *testing.T) {
	spec := goldenSpec(t)
	spec.Release.ValuesContent = "name: café\n"
	canonical, err := CanonicalPlan(spec)
	if err != nil {
		t.Fatalf("canonical plan: %v", err)
	}
	text := string(canonical)
	if !strings.Contains(text, `caf\u00e9`) || strings.Contains(text, "é") {
		t.Fatalf("canonical JSON is not ASCII-only: %s", text)
	}
	if strings.Index(text, `"catalogueChartID"`) > strings.Index(text, `"clusterID"`) {
		t.Fatalf("map keys are not sorted: %s", text)
	}
}

func TestCanonicalJSONUsesOnlyContractEscapes(t *testing.T) {
	var output strings.Builder
	buffer := bytes.NewBuffer(nil)
	value := "\x01\b\t\n\f\r/\"é🙂"
	if err := writeCanonicalJSON(buffer, value); err != nil {
		t.Fatalf("canonical string: %v", err)
	}
	output.Write(buffer.Bytes())
	want := `"\u0001\b\t\n\f\r/\"\u00e9\ud83d\ude42"`
	if output.String() != want {
		t.Fatalf("unexpected escaping: got %s, want %s", output.String(), want)
	}
	if strings.Contains(output.String(), `\x`) || strings.Contains(output.String(), `\U`) {
		t.Fatalf("Go-specific escape emitted: %s", output.String())
	}
}

func TestValidatePlanAcceptsGoldenPlan(t *testing.T) {
	assertPlanValid(t, goldenApplication(t))
}

func TestValidatePlanAcceptsExactRuntimeLimits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplication)
	}{
		{"values content", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.ValuesContent = yamlMappingOfSize(t, maxValuesContentBytes)
		}},
		{"chart and version", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.Chart = strings.Repeat("c", 1024)
			app.Spec.Release.Version = strings.Repeat("v", 253)
		}},
		{"retain deletion policies", func(app *applicationv1.OneKSApplication) {
			app.Spec.DeletionPolicy = applicationv1.DeletionPolicyRetain
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := goldenApplication(t)
			test.mutate(app)
			refreshDigest(t, app)
			assertPlanValid(t, app)
		})
	}
}

func TestValidatePlanRejectsAdditionalRuntimeBoundaryViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplication)
		reason string
	}{
		{"invalid UTF-8 chart", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.Chart = string([]byte{0xff})
		}, "InvalidChart"},
		{"invalid UTF-8 version", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.Version = string([]byte{0xff})
		}, "InvalidChartVersion"},
		{"invalid UTF-8 values", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.ValuesContent = string([]byte{0xff})
		}, "InvalidValuesContent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := goldenApplication(t)
			test.mutate(app)
			assertPlanError(t, app, test.reason)
		})
	}
}

func TestValidatePlanRejectsSecurityAndSchemaViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplication)
		reason string
	}{
		{"owner reference", func(app *applicationv1.OneKSApplication) {
			app.OwnerReferences = []metav1.OwnerReference{{Name: "owner"}}
		}, "InvalidOwnerReferences"},
		{"foreign finalizer", func(app *applicationv1.OneKSApplication) { app.Finalizers = []string{"example.com/foreign"} }, "InvalidFinalizers"},
		{"duplicate finalizer", func(app *applicationv1.OneKSApplication) {
			app.Finalizers = []string{applicationv1.ApplicationFinalizer, applicationv1.ApplicationFinalizer}
		}, "InvalidFinalizers"},
		{"missing producer label", func(app *applicationv1.OneKSApplication) {
			delete(app.Labels, LabelProducer)
		}, "InvalidProducerLabels"},
		{"mismatched producer digest label", func(app *applicationv1.OneKSApplication) {
			app.Labels[LabelPlanDigest] = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		}, "InvalidProducerLabels"},
		{"http repository", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.RepositoryURL = "http://charts.example.test"
		}, "InvalidRepositoryURL"},
		{"unsafe application name", func(app *applicationv1.OneKSApplication) { app.Name = strings.Repeat("a", 64) }, "InvalidApplicationName"},
		{"foreign cluster", func(app *applicationv1.OneKSApplication) { app.Spec.ClusterID = "43" }, "ClusterIDMismatch"},
		{"values placeholder", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.ValuesContent = "endpoint: ${host}\n"
		}, "UnresolvedPlaceholder"},
		{"sensitive values", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.ValuesContent = "adminPassword: fake-value\n"
		}, "SensitiveValuesContent"},
		{"oversized chart", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.Chart = strings.Repeat("a", 1025)
		}, "InvalidChart"},
		{"oversized version", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.Version = strings.Repeat("v", 254)
		}, "InvalidChartVersion"},
		{"digest mismatch", func(app *applicationv1.OneKSApplication) {
			app.Spec.PlanDigest = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			app.Labels[LabelPlanDigest] = app.Spec.PlanDigest
		}, "PlanDigestMismatch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := goldenApplication(t)
			test.mutate(app)
			if test.reason != "PlanDigestMismatch" {
				refreshDigest(t, app)
			}
			assertPlanErrorForCluster(t, app, "42", test.reason)
		})
	}
}

func TestValidatePlanAllowsOnlyControllerFinalizer(t *testing.T) {
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	assertPlanValid(t, app)
}

func TestValidatePlanAllowsUnrelatedRootLabels(t *testing.T) {
	app := goldenApplication(t)
	app.Labels["example.test/injected"] = "allowed"
	assertPlanValid(t, app)
}

func goldenApplication(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	spec := goldenSpec(t)
	app := &applicationv1.OneKSApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oneks-prometheus", Namespace: applicationv1.ApplicationNamespace,
			UID: types.UID("application-uid"), Generation: 1,
		},
		Spec: spec,
	}
	refreshDigest(t, app)
	app.Labels = producerLabels(app)
	return app
}

func goldenSpec(t *testing.T) applicationv1.OneKSApplicationSpec {
	t.Helper()
	return applicationv1.OneKSApplicationSpec{
		ClusterID: "42", CatalogueChartID: "d511b694-d868-4e40-8224-fdf6a0ca3383",
		PlanVersion: applicationv1.PlanVersion, ExecutionMode: applicationv1.ExecutionModeExecute,
		Role: applicationv1.ApplicationRoleRoot,
		Release: applicationv1.ReleaseSpec{
			ChartID:       "d511b694-d868-4e40-8224-fdf6a0ca3383",
			RepositoryURL: "https://prometheus-community.github.io/helm-charts",
			Chart:         "kube-prometheus-stack", Version: "v87.12.2", ReleaseName: "oneks-prometheus",
			TargetNamespace: "catalogue-workloads", ValuesContent: "grafana:\n  enabled: false\n",
		},
		Dependencies:    []applicationv1.DependencyReference{},
		DependencyPlans: []applicationv1.DependencyPlan{},
		ManagedResources: []applicationv1.ManagedResourceSpec{
			managedConfigMap("operator-smoke-config", "catalogue-workloads", "oneks-prometheus-smoke", nil),
		},
		DeletionPolicy: applicationv1.DeletionPolicyDelete,
	}
}

func refreshDigest(t *testing.T, app *applicationv1.OneKSApplication) {
	t.Helper()
	previous := app.Spec.PlanDigest
	canonical, err := CanonicalPlan(app.Spec)
	if err != nil {
		t.Fatalf("canonical plan: %v", err)
	}
	app.Spec.PlanDigest = Digest(canonical)
	if app.Labels != nil && app.Labels[LabelPlanDigest] == previous {
		app.Labels[LabelPlanDigest] = app.Spec.PlanDigest
	}
}

func validationConfig() ValidationConfig {
	return ValidationConfig{ClusterID: "42"}
}

func yamlMappingOfSize(t *testing.T, size int) string {
	t.Helper()
	const prefix = "note: "
	if size < len(prefix)+1 {
		t.Fatalf("requested YAML mapping size %d is too small", size)
	}
	return prefix + strings.Repeat("x", size-len(prefix)-1) + "\n"
}
