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
	"strings"

	"encoding/base64"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	vmkeys "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm/keys"
)

type Machine struct {
	ctrl     *goca.Controller
	ID       int
	Name     *string
	Address4 string
}

func NewMachine(cc *Clients, maybeName *string) *Machine {
	return &Machine{ctrl: goca.NewController(cc.RPC2), ID: -1, Name: maybeName}
}

func (m *Machine) Exists() bool {
	return m.ID >= 0
}

func (m *Machine) ByID(vmID int) error {
	vm, err := m.ctrl.VM(vmID).Info(true)
	if err != nil {
		return fmt.Errorf("Failed to fetch VM: %w", err)
	}

	address4, err := vm.Template.GetStrFromVec(vmkeys.ContextVec, "ETH0_IP")
	if err != nil {
		return fmt.Errorf("Failed to fetch VM: %w", err)
	}

	m.ID = vm.ID
	m.Name = &vm.Name
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

func (m *Machine) FromTemplate(templateName string, maybeUserData *string) error {
	if m.Exists() {
		return nil
	}

	templateID, err := m.ctrl.Templates().ByName(templateName)
	if err != nil {
		return fmt.Errorf("Failed to find VM template: %w", err)
	}

	template, err := m.ctrl.Template(templateID).Info(false, true)
	if err != nil {
		return fmt.Errorf("Failed to fetch VM template: %w", err)
	}

	if m.Name != nil {
		template.Template.AddPair(string(vmkeys.Name), *m.Name)
	}
	if maybeUserData != nil {
		contextVec, err := template.Template.GetVector(string(vmkeys.ContextVec))
		if err != nil {
			return fmt.Errorf("Failed to get context vector: %w", err)
		}

		contextVec.Del("USER_DATA_ENCODING")
		contextVec.AddPair("USER_DATA_ENCODING", "base64")

		contextVec.Del("USER_DATA")
		contextVec.AddPair("USER_DATA", base64.StdEncoding.EncodeToString([]byte(*maybeUserData)))
	}

	vmID, err := m.ctrl.VMs().Create(template.Template.String(), false)
	if err != nil {
		return fmt.Errorf("Failed to create VM: %w", err)
	}

	return m.ByID(vmID)
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

func (m *Machine) NodeName() *string {
	if !m.Exists() {
		return nil
	}

	nodeName := fmt.Sprintf("ip-%s", strings.Replace(m.Address4, ".", "-", -1))
	return &nodeName
}

func (m *Machine) ProviderID() *string {
	if !m.Exists() {
		return nil
	}

	providerID := fmt.Sprintf("one://%d", m.ID)
	return &providerID
}
