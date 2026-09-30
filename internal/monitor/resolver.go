/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	goca "github.com/OpenNebula/one/src/oca/go/src/goca"
	goca_vm "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

// PlacementResolver maps an OpenNebula VM to its owning OneKS nodegroup.
//
// The default implementation reads USER_TEMPLATE.ONEKS.CLUSTER_ID and
// USER_TEMPLATE.ONEKS.GROUP_ID. It rejects VMs owned by another OneKS cluster
// and caches successful results because placement is assumed immutable for the
// lifetime of a VM.
type PlacementResolver interface {
	// GroupForVM returns the OneKS group ID that owns vmID.
	GroupForVM(context.Context, int) (int, error)
}

type gocaPlacementResolver struct {
	ctrl      *goca.Controller
	clusterID int
	groups    sync.Map
}

// NewPlacementResolver constructs an OpenNebula-backed resolver. Unlike OneKS
// callback authentication, the OpenNebula client credential is read once at
// startup and therefore requires a monitor restart after rotation.
func NewPlacementResolver(config Config) (PlacementResolver, error) {
	credential, err := readCredential(config.AuthFile)
	if err != nil {
		return nil, fmt.Errorf("configure OpenNebula client authentication: %w", err)
	}
	client := goca.NewDefaultClient(goca.OneConfig{
		Endpoint: config.OpenNebulaEndpoint,
		Token:    credential,
	})
	return &gocaPlacementResolver{
		ctrl:      goca.NewController(client),
		clusterID: config.ClusterID,
	}, nil
}

// GroupForVM implements PlacementResolver.GroupForVM.
func (r *gocaPlacementResolver) GroupForVM(ctx context.Context, vmID int) (int, error) {
	// VM IDs and their OneKS placement are immutable for the VM lifetime, so a
	// successful lookup can be safely reused by node events and pod snapshots
	if groupID, found := r.groups.Load(vmID); found {
		return groupID.(int), nil
	}
	vm, err := r.ctrl.VM(vmID).InfoContext(ctx, false)
	if err != nil {
		return 0, fmt.Errorf("fetch OpenNebula VM %d: %w", vmID, err)
	}
	placement, err := placementFromVM(vm)
	if err != nil {
		return 0, fmt.Errorf("resolve OneKS placement from OpenNebula VM %d: %w", vmID, err)
	}
	if placement.clusterID != r.clusterID {
		// Never route data from a foreign cluster through this monitor instance,
		// even if the Kubernetes provider ID points to a valid OpenNebula VM.
		return 0, fmt.Errorf(
			"OpenNebula VM %d belongs to OneKS cluster %d, expected %d",
			vmID, placement.clusterID, r.clusterID,
		)
	}
	r.groups.Store(vmID, placement.groupID)
	return placement.groupID, nil
}

type nodePlacement struct {
	clusterID int
	groupID   int
}

func placementFromVM(vm *goca_vm.VM) (nodePlacement, error) {
	clusterID, err := oneKSID(vm, "CLUSTER_ID")
	if err != nil {
		return nodePlacement{}, err
	}
	groupID, err := oneKSID(vm, "GROUP_ID")
	if err != nil {
		return nodePlacement{}, err
	}
	return nodePlacement{clusterID: clusterID, groupID: groupID}, nil
}

func oneKSID(vm *goca_vm.VM, key string) (int, error) {
	value, err := vm.UserTemplate.GetStrFromVec("ONEKS", key)
	if err != nil {
		return 0, fmt.Errorf("read USER_TEMPLATE.ONEKS.%s: %w", key, err)
	}
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || id < 0 {
		return 0, fmt.Errorf("USER_TEMPLATE.ONEKS.%s must be a non-negative integer: %q", key, value)
	}
	return id, nil
}
