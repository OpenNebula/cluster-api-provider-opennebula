/*
Copyright 2024, OpenNebula Project, OpenNebula Systems.

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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

const (
	ClusterFinalizer = "onecluster.infrastructure.cluster.x-k8s.io"
)

// ONEClusterSpec defines the desired state of ONECluster
type ONEClusterSpec struct {
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint"`

	// +required
	SecretName string `json:"secretName"`

	// +optional
	VirtualRouter *ONEVirtualRouter `json:"virtualRouter,omitempty"`

	// +optional
	PublicNetwork *ONEVirtualNetwork `json:"publicNetwork,omitempty"`

	// +optional
	PrivateNetwork *ONEVirtualNetwork `json:"privateNetwork,omitempty"`

	// +optional
	Images []*ONEImage `json:"images,omitempty"`

	// +optional
	Templates []*ONETemplate `json:"templates,omitempty"`
}

type ONEVirtualRouter struct {
	// +required
	TemplateName string `json:"templateName"`

	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// +optional
	ListenerPorts []int32 `json:"listenerPorts,omitempty"`

	// +optional
	ExtraContext map[string]string `json:"extraContext,omitempty"`
}

type ONEVirtualNetwork struct {
	// +required
	Name string `json:"name"`

	// +optional
	FloatingIP *string `json:"floatingIP,omitempty"`

	// +optional
	FloatingOnly *bool `json:"floatingOnly,omitempty"`

	// +optional
	Gateway *string `json:"gateway,omitempty"`

	// +optional
	DNS *string `json:"dns,omitempty"`
}

type ONETemplate struct {
	// +required
	TemplateName string `json:"templateName"`

	// +required
	TemplateContent string `json:"templateContent,omitempty"`
}

type ONEImage struct {
	// +required
	ImageName string `json:"imageName,omitempty"`

	// +required
	ImageContent string `json:"imageContent,omitempty"`
}

// ONEClusterStatus defines the observed state of ONECluster
type ONEClusterStatus struct {
	// +optional
	Ready bool `json:"ready"`

	// +optional
	FailureDomains clusterv1.FailureDomains `json:"failureDomains,omitempty"`

	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ONECluster is the Schema for the oneclusters API
type ONECluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ONEClusterSpec   `json:"spec,omitempty"`
	Status ONEClusterStatus `json:"status,omitempty"`
}

// GetConditions returns the set of conditions for this object.
func (c *ONECluster) GetConditions() clusterv1.Conditions {
	return c.Status.Conditions
}

// SetConditions sets the conditions on this object.
func (c *ONECluster) SetConditions(conditions clusterv1.Conditions) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// ONEClusterList contains a list of ONECluster
type ONEClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ONECluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ONECluster{}, &ONEClusterList{})
}
