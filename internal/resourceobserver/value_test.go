package resourceobserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResourceValueContractAndIdentity(t *testing.T) {
	spec := ResourceSpec{
		ID: "deployment-ready", APIVersion: "apps/v1", Resource: "deployments",
		Namespace: "payments", Name: "api", Path: "status.readyReplicas",
	}
	value, err := NewResourceValue(spec, int64(3), time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if !strings.Contains(string(encoded), `"kind":"ResourceValue"`) || value.ObservedAt != "2026-08-28T12:00:00Z" {
		t.Fatalf("unexpected resource value: %s", encoded)
	}
	identity := value.Identity()
	if len(identity) != len("ResourceValue/")+64 {
		t.Fatalf("queue identity is not bounded: %q", identity)
	}
	value.Value = int64(4)
	value.ObservedAt = time.Now().UTC().Format(time.RFC3339)
	if value.Identity() != identity {
		t.Fatal("value or timestamp changed queue identity")
	}
}
