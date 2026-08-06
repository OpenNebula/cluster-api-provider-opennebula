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

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	PlanVersion          = "oneks.opennebula.io/plan-v1alpha1"
	ApplicationNamespace = "oneks-system"
	ApplicationFinalizer = "applications.oneks.opennebula.io/finalizer"
	FieldManager         = "oneks-application-controller"
	LeaderElectionID     = "oneks-application-controller-leader"
)

type ExecutionMode string

const (
	ExecutionModeObserve ExecutionMode = "Observe"
	ExecutionModeExecute ExecutionMode = "Execute"
)

type DeletionPolicy string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

type ApplicationPhase string

const (
	PhasePending    ApplicationPhase = "Pending"
	PhaseInstalling ApplicationPhase = "Installing"
	PhaseReady      ApplicationPhase = "Ready"
	PhaseFailed     ApplicationPhase = "Failed"
	PhaseDeleting   ApplicationPhase = "Deleting"
	PhaseObserving  ApplicationPhase = "Observing"
)

// OneKSApplicationSpec is an immutable, compiled application plan.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type OneKSApplicationSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	ClusterID string `json:"clusterID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	CatalogueChartID string `json:"catalogueChartID"`
	// +kubebuilder:validation:Enum=oneks.opennebula.io/plan-v1alpha1
	PlanVersion string `json:"planVersion"`
	// +kubebuilder:validation:Pattern=`^sha256-[A-Za-z0-9_-]{43}$`
	PlanDigest string `json:"planDigest"`
	// +kubebuilder:validation:Enum=Observe;Execute
	ExecutionMode ExecutionMode `json:"executionMode"`
	Release       ReleaseSpec   `json:"release"`
	// +kubebuilder:validation:MaxItems=16
	Resources []ResourceSpec `json:"resources"`
	// +kubebuilder:validation:Enum=Delete;Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy"`
}

// +kubebuilder:validation:XValidation:rule="!self.createNamespace",message="createNamespace must be false"
type ReleaseSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	ChartID string `json:"chartID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[^[:space:]]+$`
	RepositoryURL string `json:"repositoryURL"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Chart string `json:"chart"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Version string `json:"version"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ReleaseName string `json:"releaseName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Enum=oneks-poc-workloads
	TargetNamespace string `json:"targetNamespace"`
	CreateNamespace bool   `json:"createNamespace"`
	// +kubebuilder:validation:MaxLength=65536
	ValuesContent string `json:"valuesContent"`
}

type ResourceSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	ID string `json:"id"`
	// +kubebuilder:validation:Enum=v1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:Enum=ConfigMap
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Enum=oneks-poc-workloads
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MaxProperties=128
	Data map[string]string `json:"data"`
	// +kubebuilder:validation:Enum=Delete;Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy"`
}

type ApplicationProgress struct {
	// +kubebuilder:validation:Minimum=0
	Completed int32 `json:"completed"`
	// +kubebuilder:validation:Minimum=0
	Total int32 `json:"total"`
	// +kubebuilder:validation:MaxLength=128
	Current string `json:"current"`
}

type HelmChartReference struct {
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type ResourceStatus struct {
	// +kubebuilder:validation:MaxLength=63
	ID string `json:"id"`
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Enum=Pending;Applying;Ready;Failed;Deleting;Retained;Observing
	Phase string `json:"phase"`
	// +kubebuilder:validation:MaxLength=128
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=512
	Message string `json:"message,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type ApplicationError struct {
	// +kubebuilder:validation:MaxLength=128
	Reason string `json:"reason"`
	// +kubebuilder:validation:MaxLength=512
	Message string `json:"message"`
}

type OneKSApplicationStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:MaxLength=50
	ObservedPlanDigest string `json:"observedPlanDigest,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	ControllerVersion string `json:"controllerVersion,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=128
	SupportedPlanVersions []string `json:"supportedPlanVersions,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Installing;Ready;Failed;Deleting;Observing
	Phase    ApplicationPhase    `json:"phase,omitempty"`
	Progress ApplicationProgress `json:"progress,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:XValidation:rule="self.all(c, size(c.type) <= 128 && size(c.reason) <= 128 && size(c.message) <= 512)",message="condition type, reason, or message exceeds the OneKS status limit"
	Conditions   []metav1.Condition  `json:"conditions,omitempty"`
	HelmChartRef *HelmChartReference `json:"helmChartRef,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Resources []ResourceStatus  `json:"resources,omitempty"`
	LastError *ApplicationError `json:"lastError,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=oneksapp
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.executionMode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Chart",type=string,JSONPath=`.spec.release.chart`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type OneKSApplication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OneKSApplicationSpec   `json:"spec"`
	Status OneKSApplicationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type OneKSApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OneKSApplication `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OneKSApplication{}, &OneKSApplicationList{})
}
