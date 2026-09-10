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

	goca "github.com/OpenNebula/one/src/oca/go/src/goca"
	goca_vm "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

type nodeDestinationResolver interface {
	Resolve(context.Context, int) (NodeGroupEventDestination, error)
}

type gocaNodeDestinationResolver struct {
	ctrl *goca.Controller
}

func newGocaNodeDestinationResolver(config Config) (nodeDestinationResolver, error) {
	credential, err := readCredential(config.AuthFile)
	if err != nil {
		return nil, fmt.Errorf("configure OpenNebula client authentication: %w", err)
	}
	client := goca.NewDefaultClient(goca.OneConfig{
		Endpoint: config.OpenNebulaEndpoint,
		Token:    credential,
	})
	return &gocaNodeDestinationResolver{ctrl: goca.NewController(client)}, nil
}

func (r *gocaNodeDestinationResolver) Resolve(ctx context.Context, vmID int) (NodeGroupEventDestination, error) {
	vm, err := r.ctrl.VM(vmID).InfoContext(ctx, false)
	if err != nil {
		return NodeGroupEventDestination{}, fmt.Errorf("fetch OpenNebula VM %d: %w", vmID, err)
	}
	destination, err := destinationFromVM(vm)
	if err != nil {
		return NodeGroupEventDestination{}, fmt.Errorf("resolve nodegroup event destination from OpenNebula VM %d: %w", vmID, err)
	}
	return destination, nil
}

func destinationFromVM(vm *goca_vm.VM) (NodeGroupEventDestination, error) {
	clusterID, err := oneKSID(vm, "CLUSTER_ID")
	if err != nil {
		return NodeGroupEventDestination{}, err
	}
	groupID, err := oneKSID(vm, "GROUP_ID")
	if err != nil {
		return NodeGroupEventDestination{}, err
	}
	return NodeGroupEventDestination{ClusterID: clusterID, GroupID: groupID}, nil
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
