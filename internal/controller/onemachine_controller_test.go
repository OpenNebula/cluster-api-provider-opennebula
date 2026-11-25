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

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/v1beta1"
)

func testCluster(initialized bool) *clusterv1.Cluster {
	cluster := &clusterv1.Cluster{
		Spec: clusterv1.ClusterSpec{ControlPlaneRef: &corev1.ObjectReference{Kind: "TalosControlPlane", Name: "cp"}},
	}
	if initialized {
		conditions.MarkTrue(cluster, clusterv1.ControlPlaneInitializedCondition)
	}
	return cluster
}

func testONECluster(readyEarly bool) *infrav1.ONECluster {
	return &infrav1.ONECluster{Spec: infrav1.ONEClusterSpec{
		MachinesReadyBeforeControlPlaneInitialized: readyEarly,
	}}
}

func TestWaitForControlPlaneInitialized(t *testing.T) {
	tests := []struct {
		name        string
		initialized bool
		readyEarly  bool
		want        bool
	}{
		{"default waits until initialized", false, false, true},
		{"default stops waiting once initialized", true, false, false},
		{"opt in does not wait", false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := waitForControlPlaneInitialized(testCluster(tt.initialized), testONECluster(tt.readyEarly))
			if got != tt.want {
				t.Errorf("waitForControlPlaneInitialized() = %v, want %v", got, tt.want)
			}
		})
	}
}
