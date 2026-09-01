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
	"strings"
	"testing"
	"unicode/utf8"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNormalizeStatusEnforcesEveryRuntimeBound(t *testing.T) {
	status := applicationv1.OneKSApplicationStatus{
		ObservedPlanDigest:    strings.Repeat("d", 60),
		SupportedPlanVersions: repeatValue(10, strings.Repeat("p", 140)),
		Progress:              applicationv1.ApplicationProgress{Current: strings.Repeat("c", 140)},
		Conditions: repeatValue(10, metav1.Condition{
			Type: strings.Repeat("t", 140), Reason: strings.Repeat("r", 140),
			Message: strings.Repeat("m", 540),
		}),
		HelmChartRef: &applicationv1.HelmChartReference{
			Namespace: strings.Repeat("n", 70), Name: strings.Repeat("h", 260),
			UID: strings.Repeat("u", 140), ResourceVersion: strings.Repeat("r", 70),
		},
		Resources: repeatValue(20, applicationv1.ResourceStatus{
			ID: strings.Repeat("i", 70), Phase: strings.Repeat("p", 40),
			Reason: strings.Repeat("r", 140), Message: strings.Repeat("m", 540),
			ResourceVersion: strings.Repeat("v", 70),
		}),
		LastError: &applicationv1.ApplicationError{
			Reason: strings.Repeat("r", 140), Message: strings.Repeat("m", 540),
		},
	}
	normalizeStatus(&status)

	if len(status.ObservedPlanDigest) != 50 || len(status.Progress.Current) != 128 {
		t.Fatalf("top-level status bounds were not applied: %#v", status)
	}
	if len(status.SupportedPlanVersions) != 8 || len(status.SupportedPlanVersions[0]) != 128 {
		t.Fatalf("supported versions were not bounded: %#v", status.SupportedPlanVersions)
	}
	if len(status.Conditions) != 8 || len(status.Conditions[0].Type) != 128 || len(status.Conditions[0].Reason) != 128 || len(status.Conditions[0].Message) != 512 {
		t.Fatalf("conditions were not bounded: %#v", status.Conditions[0])
	}
	if len(status.Resources) != 16 || len(status.Resources[0].ID) != 63 || len(status.Resources[0].Phase) != 32 || len(status.Resources[0].Reason) != 128 || len(status.Resources[0].Message) != 512 || len(status.Resources[0].ResourceVersion) != 64 {
		t.Fatalf("resources were not bounded: %#v", status.Resources[0])
	}
	if len(status.LastError.Reason) != 128 || len(status.LastError.Message) != 512 {
		t.Fatalf("lastError was not bounded: %#v", status.LastError)
	}
}

func TestTruncatePreservesValidUTF8(t *testing.T) {
	value := strings.Repeat("a", 510) + "🚀"
	got := truncate(value, 512)
	if !utf8.ValidString(got) || len(got) > 512 {
		t.Fatalf("truncate produced invalid bounded UTF-8: %q", got)
	}
}

func repeatValue[T any](count int, value T) []T {
	result := make([]T, count)
	for index := range result {
		result[index] = value
	}
	return result
}
