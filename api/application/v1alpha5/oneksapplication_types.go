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

package v1alpha5

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	PlanVersion          = "oneks.opennebula.io/plan-v1alpha5"
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
// +kubebuilder:validation:XValidation:rule="has(self.role)",message="plan-v1alpha5 requires role"
// +kubebuilder:validation:XValidation:rule="self.role != 'Dependency' || ((!has(self.dependencyPlans) || size(self.dependencyPlans) == 0) && (!has(self.managedResources) || size(self.managedResources) == 0) && !has(self.secretInputRef) && (!has(self.protectedSecrets) || size(self.protectedSecrets) == 0) && !has(self.release.authSecret))",message="Dependency must not contain Root-only dependencyPlans, managed resources, or protected Secret fields"
// +kubebuilder:validation:XValidation:rule="((size(self.release.repositoryURL) == 0 && self.release.chart.matches('^oci://[^[:space:]]+$')) || (self.release.repositoryURL.matches('^https://[^[:space:]]+$') && !self.release.chart.startsWith('oci://')))",message="release must use either an HTTPS repositoryURL with a non-OCI chart or an empty repositoryURL with an OCI chart"
// +kubebuilder:validation:XValidation:rule="!has(self.release.authSecret) || self.release.repositoryURL.matches('^https://[^[:space:]]+$')",message="release.authSecret requires an HTTPS repositoryURL"
// +kubebuilder:validation:XValidation:rule="!has(self.release.authSecret) || (has(self.protectedSecrets) && self.protectedSecrets.filter(p, p.builderType == 'basicAuthSecret' && p.__namespace__ == 'kube-system' && p.name == self.release.authSecret.name).size() == 1)",message="release.authSecret must match exactly one protected basicAuthSecret in kube-system"
// +kubebuilder:validation:XValidation:rule="((!has(self.protectedSecrets) || size(self.protectedSecrets) == 0) ? !has(self.secretInputRef) : has(self.secretInputRef))",message="plan-v1alpha5 requires secretInputRef exactly when protectedSecrets are present"
// +kubebuilder:validation:XValidation:rule="!has(self.managedResources) || !has(self.protectedSecrets) || size(self.managedResources) + size(self.protectedSecrets) <= 16",message="plan-v1alpha5 permits at most 16 combined managedResources and protectedSecrets"
// +kubebuilder:validation:XValidation:rule="!has(self.dependencies) || self.dependencies.all(d, self.dependencies.filter(x, x.name == d.name).size() == 1)",message="dependency names must be unique"
// +kubebuilder:validation:XValidation:rule="!has(self.dependencyPlans) || self.dependencyPlans.all(p, self.dependencyPlans.filter(x, x.name == p.name).size() == 1)",message="dependency plan names must be unique"
// +kubebuilder:validation:XValidation:rule="self.role != 'Root' || !has(self.dependencies) || (has(self.dependencyPlans) && self.dependencies.all(d, self.dependencyPlans.filter(p, p.name == d.name && p.catalogueChartID == d.catalogueChartID && p.planDigest == d.planDigest).size() == 1))",message="each direct Root dependency must resolve to exactly one matching dependencyPlan"
// +kubebuilder:validation:XValidation:rule="!has(self.protectedSecrets) || self.protectedSecrets.all(p, self.protectedSecrets.filter(x, x.id == p.id).size() == 1)",message="protected Secret IDs must be unique"
// +kubebuilder:validation:XValidation:rule="!has(self.protectedSecrets) || self.protectedSecrets.all(p, self.protectedSecrets.filter(x, x.__namespace__ == p.__namespace__ && x.name == p.name).size() == 1)",message="protected Secret target identities must be unique"
// +kubebuilder:validation:XValidation:rule="!has(self.managedResources) || !has(self.protectedSecrets) || self.protectedSecrets.all(p, self.managedResources.filter(m, m.id == p.id).size() == 0)",message="protected Secret IDs must not collide with managed resource IDs"
// +kubebuilder:validation:XValidation:rule="!has(self.uninstall) || (has(self.role) && self.role == 'Dependency')",message="top-level uninstall is permitted only for Dependency applications"
// +kubebuilder:validation:XValidation:rule="!has(self.externalDetection) || (has(self.role) && self.role == 'Dependency')",message="top-level externalDetection is permitted only for Dependency applications"
type OneKSApplicationSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	ClusterID string `json:"clusterID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	CatalogueChartID string `json:"catalogueChartID"`
	// +kubebuilder:validation:Enum=oneks.opennebula.io/plan-v1alpha5
	PlanVersion string `json:"planVersion"`
	// +kubebuilder:validation:MaxLength=50
	// +kubebuilder:validation:Pattern=`^sha256-[A-Za-z0-9_-]{43}$`
	PlanDigest string `json:"planDigest"`
	// +kubebuilder:validation:Enum=Observe;Execute
	ExecutionMode ExecutionMode `json:"executionMode"`
	Release       ReleaseSpec   `json:"release"`
	// +kubebuilder:validation:Enum=Root;Dependency
	Role ApplicationRole `json:"role,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Dependencies []DependencyReference `json:"dependencies,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	DependencyPlans []DependencyPlan `json:"dependencyPlans,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	ManagedResources []ManagedResourceSpec `json:"managedResources,omitempty"`
	SecretInputRef   *SecretInputReference `json:"secretInputRef,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	ProtectedSecrets  []ProtectedSecretSpec  `json:"protectedSecrets,omitempty"`
	Uninstall         *UninstallSpec         `json:"uninstall,omitempty"`
	ExternalDetection *ExternalDetectionSpec `json:"externalDetection,omitempty"`
	// +kubebuilder:validation:Enum=Delete;Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy"`
}

type ExternalDetector string

const ExternalDetectorCertManager ExternalDetector = "cert-manager"

type ExternalDetectionSpec struct {
	// +kubebuilder:validation:Enum=cert-manager
	Detector ExternalDetector `json:"detector"`
}

type UninstallSpec struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	PreActions []UninstallPreAction `json:"preActions"`
}

type UninstallPreActionType string

const UninstallPreActionKubernetesPatch UninstallPreActionType = "kubernetesPatch"

type KubernetesPatchType string

const KubernetesPatchTypeMerge KubernetesPatchType = "merge"

type KubernetesPatchResource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Za-z0-9]*$`
	Kind string `json:"kind"`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
}

type UninstallPreAction struct {
	// +kubebuilder:validation:Enum=kubernetesPatch
	Type     UninstallPreActionType  `json:"type"`
	Resource KubernetesPatchResource `json:"resource"`
	// +kubebuilder:validation:Enum=merge
	PatchType KubernetesPatchType `json:"patchType"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16384
	PatchJSON string `json:"patchJSON"`
}

type SecretInputReference struct {
	// +kubebuilder:validation:Enum=oneks-system
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
}

type ProtectedSecretBuilderType string

const (
	ProtectedSecretBuilderBasicAuth        ProtectedSecretBuilderType = "basicAuthSecret"
	ProtectedSecretBuilderOpaque           ProtectedSecretBuilderType = "opaqueSecret"
	ProtectedSecretBuilderDockerConfigJSON ProtectedSecretBuilderType = "dockerConfigJsonSecret"
)

// +kubebuilder:validation:XValidation:rule="self.builderType != 'basicAuthSecret' || (has(self.username) && has(self.passwordInputKey) && !has(self.opaqueData) && !has(self.registry) && !has(self.email))",message="basicAuthSecret requires username and passwordInputKey only"
// +kubebuilder:validation:XValidation:rule="self.builderType != 'opaqueSecret' || (has(self.opaqueData) && size(self.opaqueData) > 0 && !has(self.username) && !has(self.passwordInputKey) && !has(self.registry) && !has(self.email))",message="opaqueSecret requires opaqueData only"
// +kubebuilder:validation:XValidation:rule="self.builderType != 'dockerConfigJsonSecret' || (has(self.registry) && has(self.username) && has(self.passwordInputKey) && has(self.email) && !has(self.opaqueData))",message="dockerConfigJsonSecret requires registry, username, passwordInputKey, and email only"
// +kubebuilder:validation:XValidation:rule="!has(self.opaqueData) || self.opaqueData.all(d, self.opaqueData.filter(x, x.key == d.key).size() == 1)",message="opaqueData target keys must be unique"
type ProtectedSecretSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	ID string `json:"id"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=basicAuthSecret;opaqueSecret;dockerConfigJsonSecret
	BuilderType ProtectedSecretBuilderType `json:"builderType"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Username string `json:"username,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	PasswordInputKey string `json:"passwordInputKey,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	OpaqueData []ProtectedSecretDataMapping `json:"opaqueData,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Registry string `json:"registry,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=320
	Email string `json:"email,omitempty"`
	// +kubebuilder:validation:Enum=Delete;Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy"`
}

type ProtectedSecretDataMapping struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	InputKey string `json:"inputKey"`
}

type ManagedResourceScope string

const (
	ManagedResourceScopeNamespaced ManagedResourceScope = "namespaced"
	ManagedResourceScopeCluster    ManagedResourceScope = "cluster"
)

type ManagedResourceSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ID string `json:"id"`
	// +kubebuilder:validation:Enum=namespaced;cluster
	Scope ManagedResourceScope `json:"scope"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Kind string `json:"kind"`
	// +kubebuilder:validation:MaxLength=128
	APIResource string `json:"apiResource,omitempty"`
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=131072
	ManifestJSON string `json:"manifestJSON"`
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=63
	DependsOn []string                 `json:"dependsOn"`
	Readiness ManagedResourceReadiness `json:"readiness"`
	// +kubebuilder:validation:Enum=Delete;Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy"`
}

type ManagedResourceReadiness struct {
	// +kubebuilder:validation:MaxItems=16
	Conditions []ManagedResourceCondition `json:"conditions"`
	// +kubebuilder:validation:MaxItems=16
	RequiredResources []ManagedResourceReference `json:"requiredResources"`
	// +kubebuilder:validation:MaxItems=16
	Checks []ManagedResourceCheck `json:"checks"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds int32 `json:"timeoutSeconds"`
}

type ManagedResourceCondition struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Type string `json:"type"`
	// +kubebuilder:validation:Enum=True;False;Unknown
	Status string `json:"status"`
}

type ManagedResourceReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Kind string `json:"kind"`
	// +kubebuilder:validation:MaxLength=128
	APIResource string `json:"apiResource,omitempty"`
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

type ManagedResourceCheck struct {
	// +kubebuilder:validation:Enum=DNSMatchesService
	Type ManagedResourceCheckType `json:"type"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Hostname string                          `json:"hostname"`
	Service  ManagedResourceServiceReference `json:"service"`
}

type ManagedResourceCheckType string

const ManagedResourceCheckDNSMatchesService ManagedResourceCheckType = "DNSMatchesService"

type ManagedResourceServiceReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

type HelmAuthSecretReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
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
	TargetNamespace string                   `json:"targetNamespace"`
	CreateNamespace bool                     `json:"createNamespace"`
	AuthSecret      *HelmAuthSecretReference `json:"authSecret,omitempty"`
	// +kubebuilder:validation:MaxLength=65536
	ValuesContent string `json:"valuesContent"`
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
// +kubebuilder:validation:XValidation:rule="self.dependencies.all(d, self.dependencies.filter(x, x.name == d.name).size() == 1)",message="dependency names must be unique"
// +kubebuilder:validation:XValidation:rule="self.dependencies.all(d, d.name != self.name)",message="dependency plan must not directly reference itself"
// +kubebuilder:validation:XValidation:rule="!has(self.release.authSecret)",message="dependency plans do not permit release.authSecret"
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
	Dependencies      []DependencyReference  `json:"dependencies"`
	Uninstall         *UninstallSpec         `json:"uninstall,omitempty"`
	ExternalDetection *ExternalDetectionSpec `json:"externalDetection,omitempty"`
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
	ResourceVersion    string       `json:"resourceVersion,omitempty"`
	ReadinessStartedAt *metav1.Time `json:"readinessStartedAt,omitempty"`
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
	// SecretInputUID is bound by the controller before plan execution.
	// +kubebuilder:validation:MaxLength=128
	SecretInputUID string `json:"secretInputUID,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="self.spec.role != 'Dependency' || !has(self.spec.dependencies) || self.spec.dependencies.all(d, d.name != self.metadata.name)",message="a Dependency application must not directly reference itself"
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
