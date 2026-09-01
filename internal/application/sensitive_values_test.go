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
	"testing"
)

func TestValidateNonSensitiveValues(t *testing.T) {
	tests := []struct {
		name, values string
		sensitive    bool
	}{
		{"reference leaves", "existingSecret: admin-creds\nexistingSecretName: admin-creds\nsecretName: admin-creds\n", false},
		{"reference containers", "imagePullSecrets:\n- name: registry-creds\nsecretKeys:\n  password: PASSWORD\nsecretKeyRef:\n  name: admin-creds\n  key: password\n", false},
		{"reference resets inherited sensitivity", "credentials:\n  secretRef:\n    name: admin-creds\n    tokenKey: API_TOKEN\n", false},
		{"neutral Secret targets", "credentials:\n  secretTargets:\n    enabled: true\n    authorizedSecrets:\n    - runai-ca-cert\n", false},
		{"case insensitive reference", "EXISTINGSECRET: admin-creds\n", false},
		{"password", "adminPassword: fake-value\n", true},
		{"token", "apiToken: fake-value\n", true},
		{"API key", "apiKey: fake-value\n", true},
		{"private key", "privateKey: fake-value\n", true},
		{"credentials", "credentials: fake-value\n", true},
		{"nested sensitive value", "auth:\n  password: fake-value\n", true},
		{"non-scalar reference leaf", "existingSecret:\n  password: fake-value\n", true},
		{"inexact reference key", "mySecretRef:\n  name: admin-creds\n", true},
		{"sensitive value below neutral container", "secretTargets:\n  password: fake-value\n", true},
		{"inexact neutral key", "mySecretTargets:\n  enabled: true\n", true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateNonSensitiveValues(test.values)
			if test.sensitive && (err == nil || err.Reason != "SensitiveValuesContent") {
				t.Fatalf("error = %#v, want SensitiveValuesContent", err)
			}
			if !test.sensitive && err != nil {
				t.Fatalf("safe Secret reference metadata rejected: %v", err)
			}
		})
	}
}

func TestSensitiveValuesValidationAppliesToDependencies(t *testing.T) {
	for _, test := range []struct {
		name, values string
		sensitive    bool
	}{
		{"references", "credentials:\n  existingSecretName: dependency-creds\n", false},
		{"secret material", "auth:\n  apiKey: fake-value\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := validRootPlan(t)
			plan := &app.Spec.DependencyPlans[0]
			plan.Release.ValuesContent = test.values
			refreshDependencyPlanDigestForTest(app.Spec.ClusterID, plan)
			app.Spec.Dependencies[0] = dependencyReferenceForPlan(*plan)
			refreshOwnedPlan(t, app)

			err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
			if test.sensitive && (err == nil || err.Reason != "SensitiveValuesContent") {
				t.Fatalf("error = %#v, want SensitiveValuesContent", err)
			}
			if !test.sensitive && err != nil {
				t.Fatalf("safe dependency references rejected: %v", err)
			}
		})
	}
}
