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

const goldenDigest = "sha256-M55-5y1MVfp5TI27rz_ssKVui_0RNKYTrM_wVWSfRqg"
const goldenStatusSHA256 = "d8324dbaf89d79e8cd5d25f29a797aac3eed81bba114ef05712f4681de5ce1b5"
const canonicalCasesSHA256 = "f651dc5403c1e85b454543e6b8bfa56d9c46036f8b887c0a6a80250228ae7beb"
const invalidCasesSHA256 = "8124715742b355ad188131b0846e23fcb50611472c57a6678a3c29ba42ac2ca5"

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

type invalidCaseMatrix struct {
	Version string `json:"version"`
	Cases   []struct {
		Name     string `json:"name"`
		Mutation string `json:"mutation"`
		Error    string `json:"error"`
	} `json:"cases"`
}

func TestGoldenPlanDigest(t *testing.T) {
	spec := goldenSpec(t)
	canonical, err := CanonicalPlan(spec)
	if err != nil {
		t.Fatalf("canonical plan: %v", err)
	}
	if got := Digest(canonical); got != goldenDigest {
		t.Fatalf("digest mismatch: got %s", got)
	}
	expected, err := os.ReadFile("testdata/oneks_application_plan_golden.canonical.json")
	if err != nil {
		t.Fatalf("read canonical fixture: %v", err)
	}
	if !bytes.Equal(canonical, expected) {
		t.Fatalf("canonical bytes differ:\n got: %s\nwant: %s", canonical, expected)
	}
	digestFixture, err := os.ReadFile("testdata/oneks_application_plan_golden.digest")
	if err != nil {
		t.Fatalf("read digest fixture: %v", err)
	}
	if strings.TrimSpace(string(digestFixture)) != goldenDigest {
		t.Fatalf("digest fixture does not match %s", goldenDigest)
	}
	if strings.Contains(string(canonical), "\n  ") {
		t.Fatalf("canonical JSON contains insignificant formatting whitespace: %q", canonical)
	}
}

func TestCanonicalPlanUsesASCIIAndStableMapOrder(t *testing.T) {
	spec := goldenSpec(t)
	spec.Release.ValuesContent = "name: café\n"
	spec.Resources[0].Data = map[string]string{"z": "last", "a": "first"}
	canonical, err := CanonicalPlan(spec)
	if err != nil {
		t.Fatalf("canonical plan: %v", err)
	}
	text := string(canonical)
	if !strings.Contains(text, `caf\u00e9`) || strings.Contains(text, "é") {
		t.Fatalf("canonical JSON is not ASCII-only: %s", text)
	}
	if strings.Index(text, `"a":"first"`) > strings.Index(text, `"z":"last"`) {
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

func TestSharedInvalidPlanCaseMatrix(t *testing.T) {
	payload := readCheckedFixture(t, "testdata/oneks_invalid_plan_cases.json", invalidCasesSHA256)
	var matrix invalidCaseMatrix
	if err := json.Unmarshal(payload, &matrix); err != nil {
		t.Fatalf("decode invalid cases: %v", err)
	}
	for _, fixture := range matrix.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			app := goldenApplication(t)
			canonicalizable := applySharedInvalidCase(t, app, fixture.Mutation)
			if canonicalizable {
				refreshDigest(t, app)
			}
			err := ValidatePlan(app, validationConfig())
			if err == nil {
				t.Fatalf("expected rejection corresponding to %q", fixture.Error)
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
		{"resources", func(app *applicationv1.OneKSApplication) {
			resource := app.Spec.Resources[0].DeepCopy()
			app.Spec.Resources = make([]applicationv1.ResourceSpec, maxResources)
			for index := range app.Spec.Resources {
				app.Spec.Resources[index] = *resource.DeepCopy()
				app.Spec.Resources[index].ID = fmt.Sprintf("resource-%d", index)
				app.Spec.Resources[index].Name = fmt.Sprintf("resource-%d", index)
			}
		}},
		{"ConfigMap entries", func(app *applicationv1.OneKSApplication) {
			app.Spec.Resources[0].Data = make(map[string]string, maxConfigMapEntries)
			for index := 0; index < maxConfigMapEntries; index++ {
				app.Spec.Resources[0].Data[fmt.Sprintf("k%03d", index)] = ""
			}
		}},
		{"ConfigMap key", func(app *applicationv1.OneKSApplication) {
			app.Spec.Resources[0].Data = map[string]string{
				strings.Repeat("k", maxConfigMapKeyBytes): "",
			}
		}},
		{"ConfigMap value", func(app *applicationv1.OneKSApplication) {
			app.Spec.Resources[0].Data = map[string]string{
				"payload": strings.Repeat("x", maxConfigMapValueBytes),
			}
		}},
		{"ConfigMap aggregate", func(app *applicationv1.OneKSApplication) {
			app.Spec.Resources[0].Data = map[string]string{
				"a": strings.Repeat("x", 32767),
				"b": strings.Repeat("x", 32767),
			}
		}},
		{"chart and version", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.Chart = strings.Repeat("c", 1024)
			app.Spec.Release.Version = strings.Repeat("v", 253)
		}},
		{"retain deletion policies", func(app *applicationv1.OneKSApplication) {
			app.Spec.DeletionPolicy = applicationv1.DeletionPolicyRetain
			app.Spec.Resources[0].DeletionPolicy = applicationv1.DeletionPolicyRetain
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
		{"ConfigMap entries max plus one", func(app *applicationv1.OneKSApplication) {
			app.Spec.Resources[0].Data = make(map[string]string, maxConfigMapEntries+1)
			for index := 0; index <= maxConfigMapEntries; index++ {
				app.Spec.Resources[0].Data[fmt.Sprintf("k%03d", index)] = ""
			}
		}, "ConfigMapDataTooLarge"},
		{"invalid UTF-8 chart", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.Chart = string([]byte{0xff})
		}, "InvalidChart"},
		{"invalid UTF-8 version", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.Version = string([]byte{0xff})
		}, "InvalidChartVersion"},
		{"invalid UTF-8 values", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.ValuesContent = string([]byte{0xff})
		}, "InvalidValuesContent"},
		{"invalid UTF-8 ConfigMap key", func(app *applicationv1.OneKSApplication) {
			app.Spec.Resources[0].Data = map[string]string{string([]byte{0xff}): "value"}
		}, "InvalidConfigMapKey"},
		{"invalid UTF-8 ConfigMap value", func(app *applicationv1.OneKSApplication) {
			app.Spec.Resources[0].Data = map[string]string{"payload": string([]byte{0xff})}
		}, "InvalidConfigMapValue"},
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
		{"wrong target namespace", func(app *applicationv1.OneKSApplication) { app.Spec.Release.TargetNamespace = "other" }, "InvalidTargetNamespace"},
		{"namespace creation", func(app *applicationv1.OneKSApplication) { app.Spec.Release.CreateNamespace = true }, "InvalidCreateNamespace"},
		{"unsupported resource", func(app *applicationv1.OneKSApplication) { app.Spec.Resources[0].Kind = "Secret" }, "UnsupportedResource"},
		{"values placeholder", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.ValuesContent = "endpoint: ${host}\n"
		}, "UnresolvedPlaceholder"},
		{"sensitive values", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.ValuesContent = "adminPassword: fake-value\n"
		}, "SensitiveValuesContent"},
		{"config placeholder", func(app *applicationv1.OneKSApplication) {
			app.Spec.Resources[0].Data["endpoint"] = "${host}"
		}, "UnresolvedPlaceholder"},
		{"sensitive config data", func(app *applicationv1.OneKSApplication) {
			app.Spec.Resources[0].Data["apiToken"] = "fake-value"
		}, "SensitiveConfigMapData"},
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
	spec.PlanDigest = goldenDigest
	app := &applicationv1.OneKSApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oneks-prometheus", Namespace: applicationv1.ApplicationNamespace,
			UID: types.UID("application-uid"), Generation: 1,
		},
		Spec: spec,
	}
	app.Labels = producerLabels(app)
	return app
}

func goldenSpec(t *testing.T) applicationv1.OneKSApplicationSpec {
	t.Helper()
	payload, err := os.ReadFile("testdata/oneks_application_plan_golden.json")
	if err != nil {
		t.Fatalf("read golden plan: %v", err)
	}
	var spec applicationv1.OneKSApplicationSpec
	if err := json.Unmarshal(payload, &spec); err != nil {
		t.Fatalf("decode golden plan: %v", err)
	}
	return spec
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

func applySharedInvalidCase(t *testing.T, app *applicationv1.OneKSApplication, mutation string) bool {
	t.Helper()
	resource := &app.Spec.Resources[0]
	switch mutation {
	case "repository-http":
		app.Spec.Release.RepositoryURL = "http://charts.example.test"
	case "repository-userinfo":
		app.Spec.Release.RepositoryURL = "https://user:fake@charts.example.test"
	case "repository-fragment":
		app.Spec.Release.RepositoryURL = "https://charts.example.test/index#fragment"
	case "target-namespace":
		app.Spec.Release.TargetNamespace = "other"
	case "create-namespace":
		app.Spec.Release.CreateNamespace = true
	case "resource-id":
		resource.ID = "not/a/label"
	case "resource-name":
		resource.Name = "Invalid_Name"
	case "deletion-policy":
		resource.DeletionPolicy = applicationv1.DeletionPolicy("Orphan")
	case "configmap-key-syntax":
		resource.Data = map[string]string{"bad/key": "value"}
	case "configmap-key-size":
		resource.Data = map[string]string{strings.Repeat("k", maxConfigMapKeyBytes+1): "value"}
	case "configmap-value-size":
		resource.Data = map[string]string{"payload": strings.Repeat("x", maxConfigMapValueBytes+1)}
	case "configmap-data-size":
		resource.Data = map[string]string{
			"a": strings.Repeat("x", 21845),
			"b": strings.Repeat("x", 21845),
			"c": strings.Repeat("x", 21845),
		}
	case "values-placeholder":
		app.Spec.Release.ValuesContent = "endpoint: ${unresolved}\n"
	case "values-sensitive":
		app.Spec.Release.ValuesContent = "apiToken: fake-value\n"
	case "configmap-placeholder":
		resource.Data = map[string]string{"endpoint": "${unresolved}"}
	case "configmap-sensitive":
		resource.Data = map[string]string{"apiToken": "fake-value"}
	case "duplicate-resource-id":
		duplicate := *resource.DeepCopy()
		duplicate.Name = "different-name"
		app.Spec.Resources = append(app.Spec.Resources, duplicate)
	case "duplicate-resource-identity":
		duplicate := *resource.DeepCopy()
		duplicate.ID = "different-id"
		app.Spec.Resources = append(app.Spec.Resources, duplicate)
	case "resource-count":
		app.Spec.Resources = make([]applicationv1.ResourceSpec, maxResources+1)
		for index := range app.Spec.Resources {
			app.Spec.Resources[index] = *resource.DeepCopy()
			app.Spec.Resources[index].ID = fmt.Sprintf("resource-%d", index)
			app.Spec.Resources[index].Name = fmt.Sprintf("resource-%d", index)
		}
	case "values-size":
		app.Spec.Release.ValuesContent = strings.Repeat("x", maxValuesContentBytes+1)
	case "canonical-size":
		app.Spec.Resources = make([]applicationv1.ResourceSpec, 4)
		for index := range app.Spec.Resources {
			app.Spec.Resources[index] = *resource.DeepCopy()
			app.Spec.Resources[index].ID = fmt.Sprintf("large-resource-%d", index)
			app.Spec.Resources[index].Name = fmt.Sprintf("large-resource-%d", index)
			app.Spec.Resources[index].Data = map[string]string{
				"payload": strings.Repeat("x", maxConfigMapValueBytes),
			}
		}
	default:
		t.Fatalf("unknown shared invalid-plan mutation %q", mutation)
	}
	return true
}
