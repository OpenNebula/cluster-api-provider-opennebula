package application

import (
	"encoding/json"
	"os"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
)

// These fixtures are also checked against Ruby by hack/check-oneks-canonical.rb.
func TestCanonicalPlanDigestCompatibility(t *testing.T) {
	data, err := os.ReadFile("testdata/canonical-plans.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name      string
		Spec      applicationv1.OneKSApplicationSpec
		Canonical string
		Digest    string
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			canonical := canonicalSpec(t, fixture.Spec)
			if string(canonical) != fixture.Canonical || Digest(canonical) != fixture.Digest {
				t.Fatal("canonical plan differs from shared Ruby/Go fixture")
			}
		})
	}
}
