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

package cloud

import (
	"encoding/xml"
	"fmt"

	goca "github.com/OpenNebula/one/src/oca/go/src/goca"
	goca_dyn "github.com/OpenNebula/one/src/oca/go/src/goca/dynamic"
)

type Cleanup struct {
	ctrl        *goca.Controller
	clusterName string
}

func NewCleanup(clients *Clients, clusterName string) (*Cleanup, error) {
	if clients == nil {
		return nil, fmt.Errorf("clients reference is nil")
	}

	return &Cleanup{ctrl: goca.NewController(clients.RPC2), clusterName: clusterName}, nil
}

func (c *Cleanup) getVirtualRouterName() string {
	return fmt.Sprintf("%s-lb", c.clusterName)
}

func (c *Cleanup) DeleteLBVirtualRouter() error {
	vrID, err := c.ctrl.VirtualRouterByName(c.getVirtualRouterName())
	if err != nil && err.Error() != "resource not found" {
		return err
	}
	if vrID < 0 {
		return nil
	}

	return c.ctrl.VirtualRouter(vrID).Delete()
}

func (c *Cleanup) getVRReservationName() string {
	return fmt.Sprintf("%s-vr", c.clusterName)
}

func (c *Cleanup) DeleteVRReservation() error {
	vnID, err := c.ctrl.VirtualNetworks().ByName(c.getVRReservationName())
	if err != nil && err.Error() != "resource not found" {
		return err
	}
	if vnID < 0 {
		return nil
	}

	return c.ctrl.VirtualNetwork(vnID).Delete()
}

func (c *Cleanup) getLBReservationName() string {
	return fmt.Sprintf("%s-lb", c.clusterName)
}

func (c *Cleanup) DeleteLBReservation() error {
	vnID, err := c.ctrl.VirtualNetworks().ByName(c.getLBReservationName())
	if err != nil && err.Error() != "resource not found" {
		return err
	}
	if vnID < 0 {
		return nil
	}

	vn, err := c.ctrl.VirtualNetwork(vnID).Info(true)
	if err != nil {
		return nil
	}
	for _, ar := range vn.ARs {
		release := &goca_dyn.Vector{XMLName: xml.Name{Local: "LEASES"}}
		release.AddPair("IP", ar.IP)
		if err := c.ctrl.VirtualNetwork(vn.ID).Release(release.String()); err != nil {
			return err
		}
	}

	return c.ctrl.VirtualNetwork(vnID).Delete()
}
