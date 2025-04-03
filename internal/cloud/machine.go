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
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"

	infrav1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/v1beta1"

	goca "github.com/OpenNebula/one/src/oca/go/src/goca"
	goca_vm "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
	goca_vm_keys "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm/keys"
)

type Machine struct {
	ctrl     *goca.Controller
	ID       int
	Name     string
	Address4 string
}

type MachineOption func(*Machine)

func NewMachine(clients *Clients, options ...MachineOption) (*Machine, error) {

	if clients == nil {
		return nil, fmt.Errorf("clients reference is nil")
	}

	m := &Machine{ctrl: goca.NewController(clients.RPC2), ID: -1}
	for _, option := range options {
		option(m)
	}
	return m, nil
}

func WithMachineName(name string) MachineOption {
	return func(m *Machine) {
		m.Name = name
	}
}

func (m *Machine) Exists() bool {
	return m.ID >= 0
}

func (m *Machine) ByID(vmID int) error {
	vm, err := m.ctrl.VM(vmID).Info(true)
	if err != nil {
		return fmt.Errorf("Failed to fetch VM: %w", err)
	}
	m.ID = vm.ID
	m.Name = vm.Name

	address4, err := vm.Template.GetStrFromVec("CONTEXT", "ETH0_IP")
	if err != nil {
		return fmt.Errorf("Failed to fetch VM: %w", err)
	}
	m.Address4 = address4

	return nil
}

func (m *Machine) ByName(vmName string) error {
	vmID, err := m.ctrl.VMs().ByName(vmName)
	if err != nil {
		return fmt.Errorf("Failed to fetch VM: %w", err)
	}

	return m.ByID(vmID)
}

func (m *Machine) FromTemplate(templateName string, userData *string, network *infrav1.ONEVirtualNetwork, router *infrav1.ONEVirtualRouter) error {
	if m.Exists() {
		return nil
	}

	vmTemplateID, err := m.ctrl.Templates().ByName(templateName)
	if err != nil {
		return fmt.Errorf("Failed to find VM template: %w", err)
	}
	vmTemplate, err := m.ctrl.Template(vmTemplateID).Info(false, true)
	if err != nil {
		return fmt.Errorf("Failed to fetch VM template: %w", err)
	}

	if len(m.Name) > 0 {
		vmTemplate.Template.Add("NAME", m.Name)
	}

	if network != nil {
		// Overwrite NIC 0, leave others intact.
		nicVec := ensureNIC(&vmTemplate.Template, 0)
		nicVec.Del("NETWORK")
		nicVec.AddPair("NETWORK", network.Name)
		if network.Gateway != nil {
			nicVec.Del("GATEWAY")
			nicVec.AddPair("GATEWAY", *network.Gateway)
		}
		if network.DNS != nil {
			nicVec.Del("DNS")
			nicVec.AddPair("DNS", *network.DNS)
		}
	}

	contextVec, err := vmTemplate.Template.GetVector("CONTEXT")
	if err != nil {
		return fmt.Errorf("Failed to get context vector: %w", err)
	}
	contextMap := map[string]string{}
	if router != nil {
		// Mark this machine as a Control-Plane backend in the VR (dynamic LB).
		contextMap["BACKEND"] = "YES"
	}
	if userData != nil {
		contextMap["USER_DATA_ENCODING"] = "base64"
		contextMap["USER_DATA"] = base64.StdEncoding.EncodeToString([]byte(*userData))
	}
	updateContext(contextVec, &contextMap)

	vmID, err := m.ctrl.VMs().Create(vmTemplate.Template.String(), false)
	if err != nil {
		return fmt.Errorf("Failed to create VM: %w", err)
	}
	if err := m.ByID(vmID); err != nil {
		return fmt.Errorf("Failed to create VM: %w", err)
	}

	if router != nil {
		// Mark this machine as a Control-Plane backend in the VR (dynamic LB).
		update := generateVMTemplateVRouterLBParams(router, m.Address4)
		if err := m.ctrl.VM(m.ID).Update(update.String(), 1); err != nil {
			return fmt.Errorf("Failed to update VM: %w", err)
		}
	}

	return nil
}

func generateVMTemplateVRouterLBParams(router *infrav1.ONEVirtualRouter, serverAddress string) *goca_vm.Template {
	update := goca_vm.NewTemplate()
	if len(router.ListenerPorts) == 0 {
		//defaults to kubernetes api port load balancing
		update.Add("ONEGATE_HAPROXY_LB0_IP", "<ETH0_EP0>")
		update.Add("ONEGATE_HAPROXY_LB0_PORT", "6443")
		update.Add("ONEGATE_HAPROXY_LB0_SERVER_HOST", serverAddress)
		update.Add("ONEGATE_HAPROXY_LB0_SERVER_PORT", "6443")
		return update
	}

	slices.Sort(router.ListenerPorts)
	for idx, port := range router.ListenerPorts {
		//NOTE: Pass ports as strings, as the template make pair method doesn't support int32 values
		update.Add(goca_vm_keys.Template(fmt.Sprintf("ONEGATE_HAPROXY_LB%d_IP", idx)), "<ETH0_EP0>")
		update.Add(goca_vm_keys.Template(fmt.Sprintf("ONEGATE_HAPROXY_LB%d_PORT", idx)), strconv.Itoa(int(port)))
		update.Add(goca_vm_keys.Template(fmt.Sprintf("ONEGATE_HAPROXY_LB%d_SERVER_HOST", idx)), serverAddress)
		update.Add(goca_vm_keys.Template(fmt.Sprintf("ONEGATE_HAPROXY_LB%d_SERVER_PORT", idx)), strconv.Itoa(int(port)))
	}
	return update
}

func (m *Machine) Delete() error {
	if !m.Exists() {
		return nil
	}

	if err := m.ctrl.VM(m.ID).TerminateHard(); err != nil {
		return fmt.Errorf("Failed to delete VM: %w", err)
	}

	m.ID = -1
	return nil
}

func (m *Machine) NodeName() (string, error) {
	if !m.Exists() {
		return "", fmt.Errorf("Machine does not exist yet")
	}

	if len(m.Name) > 0 {
		return m.Name, nil
	} else {
		nodeName := fmt.Sprintf("ip-%s", strings.Replace(m.Address4, ".", "-", -1))
		return nodeName, nil
	}
}

func (m *Machine) ProviderID() *string {
	if !m.Exists() {
		return nil
	}

	providerID := fmt.Sprintf("one://%d", m.ID)
	return &providerID
}
