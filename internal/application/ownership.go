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
	"context"
	"fmt"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelRootManagedBy        = "app.kubernetes.io/managed-by"
	LabelProducer             = "applications.oneks.opennebula.io/producer"
	LabelCatalogueChartID     = "applications.oneks.opennebula.io/catalogue-chart-id"
	LabelApplicationName      = "applications.oneks.opennebula.io/name"
	LabelApplicationNamespace = "applications.oneks.opennebula.io/namespace"
	LabelApplicationUID       = "applications.oneks.opennebula.io/uid"
	LabelClusterID            = "applications.oneks.opennebula.io/cluster-id"
	LabelRole                 = "applications.oneks.opennebula.io/role"
	LabelManagedBy            = "applications.oneks.opennebula.io/managed-by"
	ManagedByValue            = "oneks-application-controller"
	RootManagedByValue        = "oneks"
	ProducerValue             = "oneks-server"
	RootRoleValue             = "root"
	DependencyRoleValue       = "dependency"
)

func producerLabels(app *applicationv1.OneKSApplication) map[string]string {
	labels := map[string]string{
		LabelRootManagedBy:    RootManagedByValue,
		LabelProducer:         ProducerValue,
		LabelClusterID:        app.Spec.ClusterID,
		LabelCatalogueChartID: app.Spec.CatalogueChartID,
	}
	switch app.Spec.Role {
	case applicationv1.ApplicationRoleRoot:
		labels[LabelRole] = RootRoleValue
	case applicationv1.ApplicationRoleDependency:
		labels[LabelRootManagedBy] = ManagedByValue
		labels[LabelProducer] = ManagedByValue
		labels[LabelRole] = DependencyRoleValue
	}
	return labels
}

func producerLabelsMatch(app *applicationv1.OneKSApplication) bool {
	return labelSubsetMatches(app.GetLabels(), producerLabels(app))
}

func ownershipLabels(app *applicationv1.OneKSApplication) map[string]string {
	return map[string]string{
		LabelApplicationName:      app.Name,
		LabelApplicationNamespace: app.Namespace,
		LabelApplicationUID:       string(app.UID),
		LabelClusterID:            app.Spec.ClusterID,
		LabelManagedBy:            ManagedByValue,
	}
}

func ownershipMatches(app *applicationv1.OneKSApplication, object metav1.Object) bool {
	return labelSubsetMatches(object.GetLabels(), ownershipLabels(app))
}

func labelSubsetMatches(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
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

func (r *Reconciler) preflightInstallOwnership(ctx context.Context, app *applicationv1.OneKSApplication, managedAPIs managedAPIPreflightMode) error {
	if err := r.preflightManagedOwnership(ctx, app, false, managedAPIs); err != nil {
		return err
	}
	if err := r.preflightProtectedSecretOwnership(ctx, app, false); err != nil {
		return err
	}
	if usesExternalDetection(app) {
		selection, err := externalSelection(app)
		if err != nil || selection == ExternalSelectionExternal {
			return err
		}
	}
	return r.preflightHelmOwnership(ctx, app)
}

func (r *Reconciler) preflightDeleteOwnership(ctx context.Context, app *applicationv1.OneKSApplication) error {
	if app.Spec.DeletionPolicy == applicationv1.DeletionPolicyRetain {
		return nil
	}
	if usesExternalDetection(app) {
		selection, err := externalSelection(app)
		if err != nil {
			return err
		}
		if selection != ExternalSelectionManaged {
			return nil
		}
	}
	return r.preflightHelmOwnership(ctx, app)
}

func (r *Reconciler) requestsForOwnedChild(_ context.Context, object client.Object) []ctrl.Request {
	labels := object.GetLabels()
	name := labels[LabelApplicationName]
	namespace := labels[LabelApplicationNamespace]
	if labels[LabelManagedBy] != ManagedByValue || name == "" || namespace != applicationv1.ApplicationNamespace {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}
