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

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

type phaseStatusTransition struct {
	oldPhase applicationv1.ApplicationPhase
	newPhase applicationv1.ApplicationPhase
	reason   string
}

type conditionStatusTransition struct {
	condition string
	oldStatus metav1.ConditionStatus
	newStatus metav1.ConditionStatus
	oldReason string
	newReason string
}

type resourceStatusTransition struct {
	id       string
	oldPhase string
	newPhase string
	reason   string
}

type applicationStatusTransitions struct {
	phase      *phaseStatusTransition
	conditions []conditionStatusTransition
	resources  []resourceStatusTransition
}

func applicationLogger(ctx context.Context, app *applicationv1.OneKSApplication) logr.Logger {
	return ctrl.LoggerFrom(ctx).WithValues(
		"application", app.Name,
		"namespace", app.Namespace,
		"generation", app.Generation,
		"planVersion", app.Spec.PlanVersion,
		"role", app.Spec.Role,
		"releaseName", app.Spec.Release.ReleaseName,
		"targetNamespace", app.Spec.Release.TargetNamespace,
		"createNamespace", app.Spec.Release.CreateNamespace,
	)
}

func contextWithApplicationLogger(ctx context.Context, app *applicationv1.OneKSApplication) context.Context {
	return logr.NewContext(ctx, applicationLogger(ctx, app))
}

func meaningfulStatusTransitions(previous, current applicationv1.OneKSApplicationStatus) applicationStatusTransitions {
	transitions := applicationStatusTransitions{}
	if previous.Phase != current.Phase {
		transitions.phase = &phaseStatusTransition{
			oldPhase: previous.Phase,
			newPhase: current.Phase,
			reason:   applicationPhaseReason(current),
		}
	}

	previousConditions := make(map[string]metav1.Condition, len(previous.Conditions))
	for _, condition := range previous.Conditions {
		previousConditions[condition.Type] = condition
	}
	for _, condition := range current.Conditions {
		old, found := previousConditions[condition.Type]
		if found && old.Status == condition.Status && old.Reason == condition.Reason {
			continue
		}
		transitions.conditions = append(transitions.conditions, conditionStatusTransition{
			condition: condition.Type,
			oldStatus: old.Status,
			newStatus: condition.Status,
			oldReason: old.Reason,
			newReason: condition.Reason,
		})
	}

	previousResources := make(map[string]applicationv1.ResourceStatus, len(previous.Resources))
	for _, resource := range previous.Resources {
		previousResources[resource.ID] = resource
	}
	for _, resource := range current.Resources {
		old, found := previousResources[resource.ID]
		if found && old.Phase == resource.Phase && old.Reason == resource.Reason {
			continue
		}
		transitions.resources = append(transitions.resources, resourceStatusTransition{
			id: resource.ID, oldPhase: old.Phase, newPhase: resource.Phase,
			reason: resource.Reason,
		})
	}
	return transitions
}

func applicationPhaseReason(status applicationv1.OneKSApplicationStatus) string {
	if status.Phase == applicationv1.PhaseFailed && status.LastError != nil {
		return status.LastError.Reason
	}
	if status.Phase == applicationv1.PhaseDeleting {
		return ""
	}
	condition := meta.FindStatusCondition(status.Conditions, ConditionReady)
	if condition == nil {
		return ""
	}
	return condition.Reason
}

func logStatusTransitions(
	ctx context.Context,
	app *applicationv1.OneKSApplication,
	previous, current applicationv1.OneKSApplicationStatus,
) {
	logger := ctrl.LoggerFrom(ctx)
	transitions := meaningfulStatusTransitions(previous, current)
	if transitions.phase != nil {
		logger.Info(
			"application phase changed",
			"oldPhase", transitions.phase.oldPhase,
			"newPhase", transitions.phase.newPhase,
			"reason", transitions.phase.reason,
			"observedGeneration", current.ObservedGeneration,
			"ready", current.Progress.Completed,
			"total", current.Progress.Total,
		)
		if transitions.phase.newPhase == applicationv1.PhaseDeleting {
			logger.Info("application deletion started")
		}
		if app.Spec.Role == applicationv1.ApplicationRoleDependency && transitions.phase.newPhase == applicationv1.PhaseReady {
			logger.Info("dependency ready", "dependency", app.Name)
		}
	}
	for _, transition := range transitions.conditions {
		message := "application condition changed"
		extra := []any{}
		if transition.newStatus == metav1.ConditionTrue {
			switch transition.condition {
			case ConditionPlanValid:
				message = "application plan validated"
			case ConditionDependenciesReady:
				message = "dependencies ready"
			case ConditionResourcesReady:
				message = "application resources ready"
			case ConditionProtectedSecretsReady:
				message = "protected Secrets ready"
			case ConditionHelmReleaseReady:
				message = "Helm release ready"
				extra = append(extra,
					"release", app.Spec.Release.ReleaseName,
					"releaseNamespace", app.Spec.Release.TargetNamespace,
				)
			case ConditionReady:
				message = "application ready"
			}
		}
		fields := []any{
			"condition", transition.condition,
			"oldStatus", transition.oldStatus,
			"newStatus", transition.newStatus,
			"oldReason", transition.oldReason,
			"reason", transition.newReason,
			"observedGeneration", current.ObservedGeneration,
		}
		fields = append(fields, extra...)
		logger.Info(message, fields...)
	}
	for _, transition := range transitions.resources {
		logResourceStatusTransition(logger, app, transition)
	}
}

func logResourceStatusTransition(logger logr.Logger, app *applicationv1.OneKSApplication, transition resourceStatusTransition) {
	if transition.newPhase != "Ready" && transition.newPhase != "Retained" {
		return
	}
	for _, resource := range app.Spec.ManagedResources {
		if resource.ID != transition.id {
			continue
		}
		message := "managed resource ready"
		if transition.newPhase == "Retained" {
			message = "retaining managed resource"
		}
		logger.Info(message,
			"resourceID", resource.ID,
			"apiVersion", resource.APIVersion,
			"kind", resource.Kind,
			"resourceNamespace", resource.Namespace,
			"name", resource.Name,
			"reason", transition.reason,
		)
		return
	}
	for _, resource := range app.Spec.ProtectedSecrets {
		if resource.ID != transition.id {
			continue
		}
		message := "protected Secret ready"
		if transition.newPhase == "Retained" {
			message = "retaining protected Secret"
		}
		logger.Info(message,
			"resourceID", resource.ID,
			"resourceNamespace", resource.Namespace,
			"name", resource.Name,
			"reason", transition.reason,
		)
		return
	}
}
