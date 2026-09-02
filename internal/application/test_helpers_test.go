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

func assertPlanValid(t *testing.T, app *applicationv1.OneKSApplication) {
	t.Helper()
	if err := ValidatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}); err != nil {
		t.Fatalf("plan was rejected: %v", err)
	}
}

func assertPlanError(t *testing.T, app *applicationv1.OneKSApplication, reason string) *PlanError {
	return assertPlanErrorForCluster(t, app, app.Spec.ClusterID, reason)
}

func assertPlanErrorForCluster(t *testing.T, app *applicationv1.OneKSApplication, clusterID, reason string) *PlanError {
	t.Helper()
	err := ValidatePlan(app, ValidationConfig{ClusterID: clusterID})
	if err == nil || err.Reason != reason {
		t.Fatalf("plan error = %#v, want reason %s", err, reason)
	}
	return err
}
