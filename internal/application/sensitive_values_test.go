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

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
)

func TestValidateNonSensitiveValuesAllowsSecretReferenceMetadata(t *testing.T) {
	tests := map[string]string{
		"RunAI references": `global:
  imagePullSecrets:
    - name: runai-backend-registry-creds
tenantsManager:
  config:
    existingSecret: runai-backend-admin-credentials
    secretKeys:
      adminPasswordKey: ADMIN_PASSWORD
keycloakx:
  imagePullSecrets:
    - name: runai-backend-registry-creds
`,
		"reference leaves": `existingSecret: admin-creds
existingSecretName: admin-creds
secretName: admin-creds
`,
		"image pull Secrets": `imagePullSecrets:
  - name: registry-creds
`,
		"Secret keys": `secretKeys:
  adminPasswordKey: ADMIN_PASSWORD
`,
		"Secret key reference": `secretKeyRef:
  name: admin-creds
  key: password
`,
		"Secret reference": `secretRef:
  name: admin-creds
  tokenKey: API_TOKEN
`,
		"reference container resets inherited sensitivity": `credentials:
  secretRef:
    name: admin-creds
    key: password
`,
		"trust manager Secret targets": `secretTargets:
  enabled: true
  authorizedSecretsAll: false
  authorizedSecrets:
    - runai-ca-cert
app:
  trust:
    namespace: cert-manager
`,
		"neutral container resets inherited sensitivity": `credentials:
  secretTargets:
    enabled: true
    authorizedSecrets:
      - creds
`,
		"case insensitive exact keys": `ExistingSecret: admin-creds
SECRETKEYREF:
  name: admin-creds
  key: password
`,
	}

	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateNonSensitiveValues(values); err != nil {
				t.Fatalf("safe Secret reference metadata rejected: %v", err)
			}
		})
	}
}

func TestValidateNonSensitiveValuesRejectsActualSecretMaterial(t *testing.T) {
	tests := map[string]string{
		"admin password":         "adminPassword: fake-value\n",
		"API token":              "apiToken: fake-value\n",
		"API key":                "apiKey: fake-value\n",
		"private key":            "privateKey: fake-value\n",
		"nested password":        "credentials:\n  password: fake-value\n",
		"nested sensitive path":  "auth:\n  settings:\n    apiToken: fake-value\n",
		"non-scalar leaf":        "existingSecret:\n  password: fake-value\n",
		"inexact reference key":  "mySecretRef:\n  name: admin-creds\n  password: fake-value\n",
		"neutral password":       "secretTargets:\n  password: actual-secret\n",
		"neutral API token":      "secretTargets:\n  apiToken: actual-secret\n",
		"inexact neutral key":    "mySecretTargets:\n  enabled: true\n",
		"inexact neutral secret": "mySecretTargets:\n  password: actual-secret\n",
	}

	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateNonSensitiveValues(values)
			if err == nil || err.Reason != "SensitiveValuesContent" {
				t.Fatalf("sensitive values error = %#v, want SensitiveValuesContent", err)
			}
		})
	}
}

func TestSecretReferenceValuesValidationAppliesToRootAndDependencies(t *testing.T) {
	t.Run("protected Root", func(t *testing.T) {
		app := runAIProtectedPlan(t)
		if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
			t.Fatalf("protected Root with safe Secret references rejected: %v", err)
		}
	})

	t.Run("dependency safe references", func(t *testing.T) {
		plan := validDependencyPlan()
		plan.Release.ValuesContent = "credentials:\n  existingSecretName: dependency-creds\n  secretKeys:\n    password: PASSWORD\n"
		refreshDependencyPlanDigestForTest("42", &plan)
		app := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
		if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
			t.Fatalf("dependency with safe Secret references rejected: %v", err)
		}
	})

	t.Run("dependency secret material", func(t *testing.T) {
		plan := validDependencyPlan()
		plan.Release.ValuesContent = "auth:\n  apiKey: fake-value\n"
		refreshDependencyPlanDigestForTest("42", &plan)
		app := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
		err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID})
		if err == nil || err.Reason != "SensitiveValuesContent" {
			t.Fatalf("dependency sensitive values error = %#v, want SensitiveValuesContent", err)
		}
	})

	t.Run("trust manager dependency", func(t *testing.T) {
		plan := validDependencyPlan()
		plan.Release.ValuesContent = `secretTargets:
  enabled: true
  authorizedSecretsAll: false
  authorizedSecrets:
    - runai-ca-cert
app:
  trust:
    namespace: cert-manager
`
		refreshDependencyPlanDigestForTest("42", &plan)
		app := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
		if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
			t.Fatalf("trust-manager dependency metadata rejected: %v", err)
		}
	})
}
