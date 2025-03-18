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

type Images struct {
	ctrl *goca.Controller
}

func NewImages(cc *Clients) *Images {
	return &Images{ctrl: goca.NewController(cc.RPC2)}
}

func (t *Images) CreateImage(imageName, imageContent string) error {
	existingImageID, err := t.ctrl.Images().ByName(imageName)
	if err != nil && err.Error() != "resource not found" {
		return err
	}

	if existingImageID < 0 {
		imageSpec := fmt.Sprintf("NAME = \"%s\"\n%s", imageName, imageContent)
		if _, err = t.ctrl.Images().Create(imageSpec, 1); err != nil {
			return fmt.Errorf("Failed to create image: %w", err)
		}
	}

	return nil
}
