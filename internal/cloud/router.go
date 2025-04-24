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
	"fmt"
	"net"
	"slices"
	"strconv"

	infrav1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/v1beta1"

	goca "github.com/OpenNebula/one/src/oca/go/src/goca"
	goca_vr "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/virtualrouter"
)

type Router struct {
	ctrl        *goca.Controller
	ID          int
	Name        string
	Replicas    int
	FloatingIPs []string
}

type RouterOption func(*Router)

func WithRouterName(name string) RouterOption {
	return func(r *Router) {
		r.Name = name
	}
}
func WithRouterReplicas(replicas int) RouterOption {
	return func(r *Router) {
		r.Replicas = replicas
	}
}

func NewRouter(clients *Clients, options ...RouterOption) (*Router, error) {
	if clients == nil {
		return nil, fmt.Errorf("clients reference is nil")
	}

	r := &Router{ctrl: goca.NewController(clients.RPC2), ID: -1, Replicas: 1}
	for _, option := range options {
		option(r)
	}
	return r, nil
}

func (r *Router) Exists() bool {
	return r.ID >= 0
}

func (r *Router) ByID(vrID int) error {
	vr, err := r.ctrl.VirtualRouter(vrID).Info(true)
	if err != nil {
		return fmt.Errorf("Failed to fetch VR: %w", err)
	}
	r.ID = vr.ID
	r.Name = vr.Name

	for _, nicVec := range getNICs(&vr.Template) {
		if vrIP, err := nicVec.GetStr("VROUTER_IP"); err == nil {
			r.FloatingIPs = append(r.FloatingIPs, vrIP)
		}
	}

	return nil
}

func (r *Router) ByName(vrName string) error {
	vrID, err := r.ctrl.VirtualRouterByName(vrName)
	if err != nil {
		return fmt.Errorf("Failed to fetch VR: %w", err)
	}

	return r.ByID(vrID)
}

func (r *Router) FromTemplate(
	virtualRouter *infrav1.ONEVirtualRouter,
	publicNetwork, privateNetwork *infrav1.ONEVirtualNetwork) error {

	if r.Exists() {
		return nil
	}

	vmTemplateID, err := r.ctrl.Templates().ByName(virtualRouter.TemplateName)
	if err != nil {
		return fmt.Errorf("Failed to find VR template: %w", err)
	}
	vmTemplate, err := r.ctrl.Template(vmTemplateID).Info(false, true)
	if err != nil {
		return fmt.Errorf("Failed to fetch VR template: %w", err)
	}

	vrTemplate := goca_vr.NewTemplate()
	vrTemplate.Add("NAME", r.Name)

	// Overwrite NIC 0 or 0 and 1, leave others intact.
	nicIndex := -1
	if publicNetwork != nil {
		nicIndex++
		nicVec := ensureNIC(vrTemplate, nicIndex)
		nicVec.AddPair("NETWORK", publicNetwork.Name)
		nicVec.AddPair("FLOATING_IP", "YES")
		if publicNetwork.FloatingOnly == nil || *publicNetwork.FloatingOnly {
			// Avoid allocating extra IPs in public networks by default.
			nicVec.AddPair("FLOATING_ONLY", "YES")
		} else {
			nicVec.AddPair("FLOATING_ONLY", "NO")
		}
		if publicNetwork.FloatingIP != nil && net.ParseIP(*publicNetwork.FloatingIP) != nil {
			nicVec.AddPair("IP", *publicNetwork.FloatingIP)
		}
	}
	if privateNetwork != nil {
		nicIndex++
		nicVec := ensureNIC(vrTemplate, nicIndex)
		nicVec.AddPair("NETWORK", privateNetwork.Name)
		nicVec.AddPair("FLOATING_IP", "YES")
		if privateNetwork.FloatingOnly == nil || !*privateNetwork.FloatingOnly {
			nicVec.AddPair("FLOATING_ONLY", "NO")
		} else {
			nicVec.AddPair("FLOATING_ONLY", "YES")
		}
		if privateNetwork.FloatingIP != nil && net.ParseIP(*privateNetwork.FloatingIP) != nil {
			nicVec.AddPair("IP", *privateNetwork.FloatingIP)
		}
	}

	vrID, err := r.ctrl.VirtualRouters().Create(vrTemplate.String())
	if err != nil {
		return fmt.Errorf("Failed to create VR: %w", err)
	}
	if err := r.ByID(vrID); err != nil {
		return fmt.Errorf("Failed to create VR: %w", err)
	}

	contextVec, err := vmTemplate.Template.GetVector("CONTEXT")
	if err != nil {
		return fmt.Errorf("Failed to get context vector: %w", err)
	}
	contextMap := map[string]string{}
	contextMap["ONEAPP_VNF_HAPROXY_ENABLED"] = "YES"
	contextMap["ONEAPP_VNF_HAPROXY_ONEGATE_ENABLED"] = "YES"
	if len(virtualRouter.ListenerPorts) == 0 {
		//defaults to kubernets api port load balancing
		contextMap["ONEAPP_VNF_HAPROXY_LB0_IP"] = "<ETH0_EP0>"
		contextMap["ONEAPP_VNF_HAPROXY_LB0_PORT"] = "6443"
	}
	slices.Sort(virtualRouter.ListenerPorts)
	for idx, port := range virtualRouter.ListenerPorts {
		contextMap[fmt.Sprintf("ONEAPP_VNF_HAPROXY_LB%d_IP", idx)] = "<ETH0_EP0>"
		contextMap[fmt.Sprintf("ONEAPP_VNF_HAPROXY_LB%d_PORT", idx)] = strconv.Itoa(int(port))
	}
	updateContext(contextVec, &contextMap)
	if virtualRouter.ExtraContext != nil {
		updateContext(contextVec, &virtualRouter.ExtraContext)
	}
	if _, err := r.ctrl.VirtualRouter(r.ID).Instantiate(
		r.Replicas,
		vmTemplateID,
		"",    // name
		false, // hold
		vmTemplate.Template.String(),
	); err != nil {
		return fmt.Errorf("Failed to create VR: %w", err)
	}

	return nil
}

func (r *Router) Delete() error {
	if !r.Exists() {
		return nil
	}

	if err := r.ctrl.VirtualRouter(r.ID).Delete(); err != nil {
		return fmt.Errorf("Failed to delete VR: %w", err)
	}

	r.ID = -1
	return nil
}
