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
	PlanVersionV1Alpha1  = "oneks.opennebula.io/plan-v1alpha1"
	PlanVersionV1Alpha2  = "oneks.opennebula.io/plan-v1alpha2"
	PlanVersion          = PlanVersionV1Alpha1
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

type ApplicationRole string

const (
	ApplicationRoleRoot       ApplicationRole = "Root"
	ApplicationRoleDependency ApplicationRole = "Dependency"
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
// +kubebuilder:validation:XValidation:rule="self.planVersion != 'oneks.opennebula.io/plan-v1alpha1' || (!has(self.role) && !has(self.dependencies) && !has(self.dependencyPlans) && self.release.targetNamespace == 'oneks-poc-workloads' && !self.release.createNamespace && self.resources.all(r, r.__namespace__ == 'oneks-poc-workloads'))",message="plan-v1alpha1 does not permit role or dependency fields and requires the fixed workload namespace without namespace creation"
// +kubebuilder:validation:XValidation:rule="self.planVersion != 'oneks.opennebula.io/plan-v1alpha1' || (size(self.release.repositoryURL) >= 1 && self.release.repositoryURL.matches('^https://[^[:space:]]+$'))",message="plan-v1alpha1 requires a non-empty HTTPS repositoryURL"
// +kubebuilder:validation:XValidation:rule="self.planVersion != 'oneks.opennebula.io/plan-v1alpha2' || has(self.role)",message="plan-v1alpha2 requires role"
// +kubebuilder:validation:XValidation:rule="self.planVersion != 'oneks.opennebula.io/plan-v1alpha2' || ((size(self.release.repositoryURL) == 0 && self.release.chart.matches('^oci://[^[:space:]]+$')) || (self.release.repositoryURL.matches('^https://[^[:space:]]+$') && !self.release.chart.startsWith('oci://')))",message="plan-v1alpha2 release must use either an HTTPS repositoryURL with a non-OCI chart or an empty repositoryURL with an OCI chart"
// +kubebuilder:validation:XValidation:rule="self.planVersion != 'oneks.opennebula.io/plan-v1alpha2' || self.role != 'Root' || (self.release.targetNamespace == 'oneks-poc-workloads' && !self.release.createNamespace)",message="plan-v1alpha2 Root requires the fixed workload namespace without namespace creation"
// +kubebuilder:validation:XValidation:rule="self.planVersion != 'oneks.opennebula.io/plan-v1alpha2' || self.role != 'Dependency' || !has(self.dependencyPlans) || size(self.dependencyPlans) == 0",message="plan-v1alpha2 Dependency must not contain dependencyPlans"
// +kubebuilder:validation:XValidation:rule="self.planVersion != 'oneks.opennebula.io/plan-v1alpha2' || self.role != 'Dependency' || !self.release.createNamespace || size(self.resources) == 0",message="plan-v1alpha2 Dependency with createNamespace must not contain resources"
// +kubebuilder:validation:XValidation:rule="self.resources.all(r, r.__namespace__ == self.release.targetNamespace)",message="resources must use the release targetNamespace"
// +kubebuilder:validation:XValidation:rule="!has(self.dependencies) || self.dependencies.all(d, self.dependencies.filter(x, x.name == d.name).size() == 1)",message="dependency names must be unique"
// +kubebuilder:validation:XValidation:rule="!has(self.dependencyPlans) || self.dependencyPlans.all(p, self.dependencyPlans.filter(x, x.name == p.name).size() == 1)",message="dependency plan names must be unique"
// +kubebuilder:validation:XValidation:rule="self.planVersion != 'oneks.opennebula.io/plan-v1alpha2' || self.role != 'Root' || !has(self.dependencies) || (has(self.dependencyPlans) && self.dependencies.all(d, self.dependencyPlans.filter(p, p.name == d.name && p.catalogueChartID == d.catalogueChartID && p.planDigest == d.planDigest).size() == 1))",message="each direct Root dependency must resolve to exactly one matching dependencyPlan"
type OneKSApplicationSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	ClusterID string `json:"clusterID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	CatalogueChartID string `json:"catalogueChartID"`
	// +kubebuilder:validation:Enum=oneks.opennebula.io/plan-v1alpha1;oneks.opennebula.io/plan-v1alpha2
	PlanVersion string `json:"planVersion"`
	// +kubebuilder:validation:MaxLength=50
	// +kubebuilder:validation:Pattern=`^sha256-[A-Za-z0-9_-]{43}$`
	PlanDigest string `json:"planDigest"`
	// +kubebuilder:validation:Enum=Observe;Execute
	ExecutionMode ExecutionMode `json:"executionMode"`
	Release       ReleaseSpec   `json:"release"`
	// +kubebuilder:validation:MaxItems=16
	Resources []ResourceSpec `json:"resources"`
	// +kubebuilder:validation:Enum=Root;Dependency
	Role ApplicationRole `json:"role,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Dependencies []DependencyReference `json:"dependencies,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	DependencyPlans []DependencyPlan `json:"dependencyPlans,omitempty"`
	// +kubebuilder:validation:Enum=Delete;Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy"`
}

type ReleaseSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	ChartID string `json:"chartID"`
	// +kubebuilder:validation:MaxLength=2048
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
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
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
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
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

type DependencyReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	CatalogueChartID string `json:"catalogueChartID"`
	// +kubebuilder:validation:MaxLength=50
	// +kubebuilder:validation:Pattern=`^sha256-[A-Za-z0-9_-]{43}$`
	PlanDigest string `json:"planDigest"`
}

// +kubebuilder:validation:XValidation:rule="self.release.chartID == self.catalogueChartID",message="release.chartID must equal catalogueChartID"
// +kubebuilder:validation:XValidation:rule="(size(self.release.repositoryURL) == 0 && self.release.chart.matches('^oci://[^[:space:]]+$')) || (self.release.repositoryURL.matches('^https://[^[:space:]]+$') && !self.release.chart.startsWith('oci://'))",message="dependency release must use either an HTTPS repositoryURL with a non-OCI chart or an empty repositoryURL with an OCI chart"
// +kubebuilder:validation:XValidation:rule="self.resources.all(r, r.__namespace__ == self.release.targetNamespace)",message="resources must use the release targetNamespace"
// +kubebuilder:validation:XValidation:rule="!self.release.createNamespace || size(self.resources) == 0",message="dependency plan with createNamespace must not contain resources"
// +kubebuilder:validation:XValidation:rule="self.dependencies.all(d, self.dependencies.filter(x, x.name == d.name).size() == 1)",message="dependency names must be unique"
// +kubebuilder:validation:XValidation:rule="self.dependencies.all(d, d.name != self.name)",message="dependency plan must not directly reference itself"
type DependencyPlan struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	CatalogueChartID string `json:"catalogueChartID"`
	// +kubebuilder:validation:MaxLength=50
	// +kubebuilder:validation:Pattern=`^sha256-[A-Za-z0-9_-]{43}$`
	PlanDigest string      `json:"planDigest"`
	Release    ReleaseSpec `json:"release"`
	// +kubebuilder:validation:MaxItems=16
	Resources []ResourceSpec `json:"resources"`
	// +kubebuilder:validation:MaxItems=16
	Dependencies []DependencyReference `json:"dependencies"`
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
// +kubebuilder:validation:XValidation:rule="self.spec.planVersion != 'oneks.opennebula.io/plan-v1alpha2' || self.spec.role != 'Dependency' || !has(self.spec.dependencies) || self.spec.dependencies.all(d, d.name != self.metadata.name)",message="a Dependency application must not directly reference itself"
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
