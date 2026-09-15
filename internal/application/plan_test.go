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

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestValidatePlanAcceptsGoldenPlan(t *testing.T) {
	assertPlanValid(t, goldenApplication(t))
}

func TestValidatePlanRejectsRuntimeContractViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*applicationv1.OneKSApplication)
		reason string
	}{
		{"http repository", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.RepositoryURL = "http://charts.example.test"
		}, "InvalidRepositoryURL"},
		{"foreign cluster", func(app *applicationv1.OneKSApplication) { app.Spec.ClusterID = "43" }, "ClusterIDMismatch"},
		{"values placeholder", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.ValuesContent = "endpoint: ${host}\n"
		}, "UnresolvedPlaceholder"},
		{"sensitive values", func(app *applicationv1.OneKSApplication) {
			app.Spec.Release.ValuesContent = "adminPassword: fake-value\n"
		}, "SensitiveValuesContent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := goldenApplication(t)
			test.mutate(app)
			assertPlanErrorForCluster(t, app, "42", test.reason)
		})
	}
}

func goldenApplication(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	spec := goldenSpec()
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

func goldenSpec() applicationv1.OneKSApplicationSpec {
	return applicationv1.OneKSApplicationSpec{
		ClusterID: "42", CatalogueChartID: "d511b694-d868-4e40-8224-fdf6a0ca3383",
		PlanVersion: applicationv1.PlanVersion, Role: applicationv1.ApplicationRoleRoot,
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
