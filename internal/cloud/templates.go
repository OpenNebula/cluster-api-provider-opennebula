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
	ctrl *goca.Controller
}

func NewTemplates(cc *Clients) *Templates {
	return &Templates{ctrl: goca.NewController(cc.RPC2)}
}

func (m *Templates) CreateTemplate(templateName, templateContent string) error {
	existingID, err := m.ctrl.Templates().ByName(templateName)
	if err != nil && err.Error() != "resource not found" {
		return err
	}
	if existingID < 0 {
		templateSpec := addTemplateName(templateName, templateContent)
		if _, err = m.ctrl.Templates().Create(templateSpec); err != nil {
			return fmt.Errorf("Failed to create VM template: %w", err)
		}
	}

	return nil
}
