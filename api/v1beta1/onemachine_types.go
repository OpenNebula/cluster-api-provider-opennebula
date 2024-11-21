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
	MachineFinalizer = "onemachine.infrastructure.cluster.x-k8s.io"
)

// ONEMachineSpec defines the desired state of ONEMachine
type ONEMachineSpec struct {
	// +optional
	ProviderID *string `json:"providerID,omitempty"`

	// +required
	TemplateName string `json:"templateName"`
}

// ONEMachineStatus defines the observed state of ONEMachine
type ONEMachineStatus struct {
	// +optional
	Ready bool `json:"ready"`

	// +optional
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ONEMachine is the Schema for the onemachines API
type ONEMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ONEMachineSpec   `json:"spec,omitempty"`
	Status ONEMachineStatus `json:"status,omitempty"`
}

func (c *ONEMachine) GetConditions() clusterv1.Conditions {
	return c.Status.Conditions
}

func (c *ONEMachine) SetConditions(conditions clusterv1.Conditions) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// ONEMachineList contains a list of ONEMachine
type ONEMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ONEMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ONEMachine{}, &ONEMachineList{})
}
