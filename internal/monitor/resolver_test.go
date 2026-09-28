/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"testing"

	goca_vm "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

func TestPlacementFromVM(t *testing.T) {
	vm := &goca_vm.VM{ID: 2}
	oneKS := vm.UserTemplate.AddVector("ONEKS")
	oneKS.AddPair("CLUSTER_ID", "15")
	oneKS.AddPair("CLUSTER_NAME", "test-demo")
	oneKS.AddPair("GROUP_ID", "16")
	oneKS.AddPair("TYPE", "ControlPlane")

	placement, err := placementFromVM(vm)
	if err != nil {
		t.Fatal(err)
	}
	if placement.clusterID != 15 || placement.groupID != 16 {
		t.Fatalf("unexpected placement: %#v", placement)
	}
}

func TestPlacementFromVMRejectsMissingOrInvalidIDs(t *testing.T) {
	for _, test := range []struct {
		name      string
		clusterID string
		groupID   string
	}{
		{name: "missing cluster ID", groupID: "16"},
		{name: "invalid cluster ID", clusterID: "cluster", groupID: "16"},
		{name: "missing group ID", clusterID: "15"},
		{name: "invalid group ID", clusterID: "15", groupID: "group"},
	} {
		t.Run(test.name, func(t *testing.T) {
			vm := &goca_vm.VM{ID: 2}
			oneKS := vm.UserTemplate.AddVector("ONEKS")
			if test.clusterID != "" {
				oneKS.AddPair("CLUSTER_ID", test.clusterID)
			}
			if test.groupID != "" {
				oneKS.AddPair("GROUP_ID", test.groupID)
			}
			if _, err := placementFromVM(vm); err == nil {
				t.Fatal("expected an invalid destination error")
			}
		})
	}
}
