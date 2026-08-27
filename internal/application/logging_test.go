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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMeaningfulStatusTransitionsDetectPhaseChange(t *testing.T) {
	previous := applicationv1.OneKSApplicationStatus{Phase: applicationv1.PhaseInstalling}
	current := applicationv1.OneKSApplicationStatus{
		Phase: applicationv1.PhaseReady,
		Conditions: []metav1.Condition{{
			Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "ApplicationReady",
		}},
	}

	transition := meaningfulStatusTransitions(previous, current).phase
	if transition == nil || transition.oldPhase != applicationv1.PhaseInstalling ||
		transition.newPhase != applicationv1.PhaseReady || transition.reason != "ApplicationReady" {
		t.Fatalf("unexpected phase transition: %#v", transition)
	}
}

func TestMeaningfulStatusTransitionsIgnoreUnchangedPhaseAndCondition(t *testing.T) {
	status := applicationv1.OneKSApplicationStatus{
		Phase: applicationv1.PhaseInstalling,
		Conditions: []metav1.Condition{{
			Type: ConditionReady, Status: metav1.ConditionFalse, Reason: "ApplicationProgressing",
		}},
	}

	transitions := meaningfulStatusTransitions(status, *status.DeepCopy())
	if transitions.phase != nil || len(transitions.conditions) != 0 || len(transitions.resources) != 0 {
		t.Fatalf("unchanged status produced transitions: %#v", transitions)
	}
}

func TestMeaningfulStatusTransitionsDetectConditionChange(t *testing.T) {
	previous := applicationv1.OneKSApplicationStatus{Conditions: []metav1.Condition{{
		Type: ConditionDependenciesReady, Status: metav1.ConditionFalse, Reason: "DependencyInstalling",
	}}}
	current := applicationv1.OneKSApplicationStatus{Conditions: []metav1.Condition{{
		Type: ConditionDependenciesReady, Status: metav1.ConditionTrue, Reason: "DependenciesReady",
	}}}

	transitions := meaningfulStatusTransitions(previous, current)
	if len(transitions.conditions) != 1 {
		t.Fatalf("condition transitions = %#v", transitions.conditions)
	}
	transition := transitions.conditions[0]
	if transition.condition != ConditionDependenciesReady ||
		transition.oldStatus != metav1.ConditionFalse || transition.newStatus != metav1.ConditionTrue ||
		transition.oldReason != "DependencyInstalling" || transition.newReason != "DependenciesReady" {
		t.Fatalf("unexpected condition transition: %#v", transition)
	}
}

func TestMeaningfulStatusTransitionsHandleEmptyPreviousStatus(t *testing.T) {
	current := applicationv1.OneKSApplicationStatus{
		Phase: applicationv1.PhaseInstalling,
		Conditions: []metav1.Condition{{
			Type: ConditionPlanValid, Status: metav1.ConditionTrue, Reason: "Validated",
		}},
	}

	transitions := meaningfulStatusTransitions(applicationv1.OneKSApplicationStatus{}, current)
	if transitions.phase == nil || transitions.phase.oldPhase != "" || transitions.phase.newPhase != applicationv1.PhaseInstalling {
		t.Fatalf("missing initial phase transition: %#v", transitions.phase)
	}
	if len(transitions.conditions) != 1 || transitions.conditions[0].oldStatus != "" ||
		transitions.conditions[0].newStatus != metav1.ConditionTrue {
		t.Fatalf("missing initial condition transition: %#v", transitions.conditions)
	}
}
