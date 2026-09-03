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

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
)

func contextWithApplicationLogger(ctx context.Context, app *applicationv1.OneKSApplication) context.Context {
	logger := ctrl.LoggerFrom(ctx).WithValues(
		"application", app.Name,
		"namespace", app.Namespace,
		"generation", app.Generation,
		"planVersion", app.Spec.PlanVersion,
		"role", app.Spec.Role,
		"releaseName", app.Spec.Release.ReleaseName,
		"targetNamespace", app.Spec.Release.TargetNamespace,
		"createNamespace", app.Spec.Release.CreateNamespace,
	)
	return logr.NewContext(ctx, logger)
}
