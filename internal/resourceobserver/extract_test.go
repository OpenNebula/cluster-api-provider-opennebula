package resourceobserver

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestExtractValue(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"readyReplicas": int64(3)},
	}}
	value, err := ExtractValue(object, "status.readyReplicas")
	if err != nil || value != int64(3) {
		t.Fatalf("unexpected extracted value: %#v, %v", value, err)
	}
	missing, err := ExtractValue(object, "status.missing")
	if err != nil || missing != nil {
		t.Fatalf("missing path did not produce nil: %#v, %v", missing, err)
	}
}

func TestExtractValueRejectsComplexValues(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"nested": map[string]any{"unsafe": true}},
	}}
	if _, err := ExtractValue(object, "status.nested"); err == nil {
		t.Fatal("complex value was accepted")
	}
}
