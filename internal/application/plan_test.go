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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const goldenStatusSHA256 = "8ff4aa7069f884947505f6de943870bbfc113204ec99ded2b09ae8da847f3d30"
const canonicalCasesSHA256 = "f651dc5403c1e85b454543e6b8bfa56d9c46036f8b887c0a6a80250228ae7beb"

type canonicalCaseMatrix struct {
	Version string `json:"version"`
	Cases   []struct {
		Name   string          `json:"name"`
		Input  json.RawMessage `json:"input"`
		Repeat *struct {
			Key       string `json:"key"`
			Codepoint string `json:"codepoint"`
			Count     int    `json:"count"`
		} `json:"repeat"`
		Canonical string `json:"canonical"`
		Digest    string `json:"digest"`
		Bytes     int    `json:"bytes"`
	} `json:"cases"`
}

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

func TestSharedCanonicalCaseMatrix(t *testing.T) {
	payload := readCheckedFixture(t, "testdata/oneks_canonical_cases.json", canonicalCasesSHA256)
	var matrix canonicalCaseMatrix
	if err := json.Unmarshal(payload, &matrix); err != nil {
		t.Fatalf("decode canonical cases: %v", err)
	}
	for _, fixture := range matrix.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var input any
			if fixture.Repeat != nil {
				input = map[string]any{fixture.Repeat.Key: strings.Repeat(fixture.Repeat.Codepoint, fixture.Repeat.Count)}
			} else {
				decoder := json.NewDecoder(bytes.NewReader(fixture.Input))
				decoder.UseNumber()
				if err := decoder.Decode(&input); err != nil {
					t.Fatalf("decode input: %v", err)
				}
			}
			var output bytes.Buffer
			if err := writeCanonicalJSON(&output, input); err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if output.Len() != fixture.Bytes {
				t.Fatalf("got %d bytes, want %d", output.Len(), fixture.Bytes)
			}
			expected := fixture.Canonical
			if expected == "" {
				expected = string(output.Bytes())
			}
			assertCanonicalContract(t, output.Bytes(), expected, fixture.Digest)
			if fixture.Name == "exact-canonical-maximum" && output.Len() > maxCanonicalPlanBytes {
				t.Fatal("exact maximum rejected")
			}
			if fixture.Name == "canonical-maximum-plus-one" && output.Len() <= maxCanonicalPlanBytes {
				t.Fatal("maximum plus one accepted")
			}
		})
	}
}

func TestValidatePlanAcceptsGoldenPlan(t *testing.T) {
	app := goldenApplication(t)
	if err := ValidatePlan(app, validationConfig()); err != nil {
		t.Fatalf("golden plan rejected: %v", err)
	}
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
			if err := ValidatePlan(app, validationConfig()); err != nil {
				t.Fatalf("exact limit rejected: %#v", err)
			}
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
			err := ValidatePlan(app, validationConfig())
			if err == nil || err.Reason != test.reason {
				t.Fatalf("expected %s, got %#v", test.reason, err)
			}
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
			err := ValidatePlan(app, validationConfig())
			if err == nil || err.Reason != test.reason {
				t.Fatalf("expected %s, got %#v", test.reason, err)
			}
		})
	}
}

func TestValidatePlanAllowsOnlyControllerFinalizer(t *testing.T) {
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	if err := ValidatePlan(app, validationConfig()); err != nil {
		t.Fatalf("controller retry finalizer rejected: %v", err)
	}
}

func TestValidatePlanAllowsUnrelatedRootLabels(t *testing.T) {
	app := goldenApplication(t)
	app.Labels["example.test/injected"] = "allowed"
	if err := ValidatePlan(app, validationConfig()); err != nil {
		t.Fatalf("unrelated Kubernetes label rejected: %v", err)
	}
}

func TestGoldenStatusUsesTheApprovedJSONContract(t *testing.T) {
	payload, err := os.ReadFile("testdata/oneks_application_status_golden.json")
	if err != nil {
		t.Fatalf("read status fixture: %v", err)
	}
	checksum := sha256.Sum256(payload)
	if got := fmt.Sprintf("%x", checksum); got != goldenStatusSHA256 {
		t.Fatalf("status fixture checksum differs: got %s, want %s", got, goldenStatusSHA256)
	}
	var status applicationv1.OneKSApplicationStatus
	if err := json.Unmarshal(payload, &status); err != nil {
		t.Fatalf("decode status fixture: %v", err)
	}
	if status.HelmChartRef == nil || status.HelmChartRef.Name != "oneks-prometheus" {
		t.Fatalf("helmChartRef was not decoded: %#v", status.HelmChartRef)
	}
	if len(status.Resources) != 1 || status.Resources[0].Phase != "Ready" || status.Resources[0].ResourceVersion != "100" {
		t.Fatalf("resource status does not match the approved shape: %#v", status.Resources)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("decode raw fixture: %v", err)
	}
	if _, exists := raw["helmChart"]; exists {
		t.Fatal("obsolete helmChart field is present")
	}
	if _, exists := raw["helmChartRef"]; !exists {
		t.Fatal("helmChartRef field is absent")
	}
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

func readCheckedFixture(t *testing.T, path, expectedChecksum string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	checksum := sha256.Sum256(payload)
	if got := fmt.Sprintf("%x", checksum); got != expectedChecksum {
		t.Fatalf("fixture %s checksum got %s, want %s", path, got, expectedChecksum)
	}
	return payload
}

func assertCanonicalContract(t *testing.T, output []byte, expected, expectedDigest string) {
	t.Helper()
	if string(output) != expected {
		t.Fatalf("canonical bytes got %s, want %s", output, expected)
	}
	if !json.Valid(output) {
		t.Fatalf("canonical output is not JSON: %q", output)
	}
	for _, value := range output {
		if value > 0x7f {
			t.Fatalf("canonical output is not ASCII: %q", output)
		}
	}
	text := string(output)
	if strings.Contains(text, `\x`) || strings.Contains(text, `\U`) || regexp.MustCompile(`\\[0-7]`).MatchString(text) {
		t.Fatalf("canonical output contains a forbidden Go escape: %s", text)
	}
	if got := Digest(output); got != expectedDigest {
		t.Fatalf("digest got %s, want %s", got, expectedDigest)
	}
}

func yamlMappingOfSize(t *testing.T, size int) string {
	t.Helper()
	const prefix = "note: "
	if size < len(prefix)+1 {
		t.Fatalf("requested YAML mapping size %d is too small", size)
	}
	return prefix + strings.Repeat("x", size-len(prefix)-1) + "\n"
}
