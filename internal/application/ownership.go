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
	"fmt"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	LabelRootManagedBy        = "app.kubernetes.io/managed-by"
	LabelProducer             = "applications.oneks.opennebula.io/producer"
	LabelCatalogueChartID     = "applications.oneks.opennebula.io/catalogue-chart-id"
	LabelApplicationName      = "applications.oneks.opennebula.io/name"
	LabelApplicationNamespace = "applications.oneks.opennebula.io/namespace"
	LabelApplicationUID       = "applications.oneks.opennebula.io/uid"
	LabelClusterID            = "applications.oneks.opennebula.io/cluster-id"
	LabelPlanDigest           = "applications.oneks.opennebula.io/plan-digest"
	LabelManagedBy            = "applications.oneks.opennebula.io/managed-by"
	ManagedByValue            = "oneks-application-controller"
	RootManagedByValue        = "oneks"
	ProducerValue             = "oneks-server"
	ChartIDAnnotation         = "oneks.opennebula.io/chart-id"
)

func producerLabels(app *applicationv1.OneKSApplication) map[string]string {
	return map[string]string{
		LabelRootManagedBy:    RootManagedByValue,
		LabelProducer:         ProducerValue,
		LabelClusterID:        app.Spec.ClusterID,
		LabelPlanDigest:       app.Spec.PlanDigest,
		LabelCatalogueChartID: app.Spec.CatalogueChartID,
	}
}

func producerLabelsMatch(app *applicationv1.OneKSApplication) bool {
	actual := app.GetLabels()
	for key, expected := range producerLabels(app) {
		if actual[key] != expected {
			return false
		}
	}
	return true
}

func ownershipLabels(app *applicationv1.OneKSApplication) map[string]string {
	return map[string]string{
		LabelApplicationName:      app.Name,
		LabelApplicationNamespace: app.Namespace,
		LabelApplicationUID:       string(app.UID),
		LabelClusterID:            app.Spec.ClusterID,
		LabelPlanDigest:           app.Spec.PlanDigest,
		LabelManagedBy:            ManagedByValue,
	}
}

func ownershipMatches(app *applicationv1.OneKSApplication, object metav1.Object) bool {
	actual := object.GetLabels()
	for key, expected := range ownershipLabels(app) {
		if actual[key] != expected {
			return false
		}
	}
	return true
}

type OwnershipConflictError struct {
	Kind      string
	Namespace string
	Name      string
}

func (e *OwnershipConflictError) Error() string {
	return fmt.Sprintf("%s %s/%s exists without exact OneKS application ownership", e.Kind, e.Namespace, e.Name)
}
