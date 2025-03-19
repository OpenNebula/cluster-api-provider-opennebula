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

	goca "github.com/OpenNebula/one/src/oca/go/src/goca"
)

type Templates struct {
	ctrl       *goca.Controller
	clusterUID string
}

func NewTemplates(cc *Clients, clusterUID string) *Templates {
	return &Templates{
		ctrl:       goca.NewController(cc.RPC2),
		clusterUID: clusterUID,
	}
}

func (m *Templates) CreateTemplate(templateName, templateContent string) error {
	existingID, err := m.ctrl.Templates().ByName(templateName)
	if err != nil && err.Error() != "resource not found" {
		return err
	}

	createNew := existingID < 0
	templateClusterUID := templateName + "-" + m.clusterUID
	if existingID >= 0 {
		vmTemplate, err := m.ctrl.Template(existingID).Info(false, true)
		if err != nil {
			return fmt.Errorf("Failed to obtain existing VM template: %w", err)
		}

		existingClusterUID, err := vmTemplate.Template.Get("CLUSTER_UID")
		if err != nil || existingClusterUID != templateClusterUID {
			if err = m.ctrl.Template(existingID).Delete(); err != nil {
				return fmt.Errorf("Failed to delete existing VM template: %w", err)
			}
			createNew = true
		}
	}
	if createNew {
		templateSpec := fmt.Sprintf(
			"NAME = \"%s\"\nCLUSTER_UID = \"%s\"\n%s",
			templateName, templateClusterUID, templateContent)
		if _, err = m.ctrl.Templates().Create(templateSpec); err != nil {
			return fmt.Errorf("Failed to create VM template: %w", err)
		}
	}

	return nil
}
