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
	goca_image "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image"
)

type Images struct {
	ctrl *goca.Controller
}

func NewImages(clients *Clients) (*Images, error) {
	if clients == nil {
		return nil, fmt.Errorf("clients reference is nil")
	}

	return &Images{ctrl: goca.NewController(clients.RPC2)}, nil
}

func (i *Images) CreateImage(imageName, imageContent string) error {
	existingImageID, err := i.ctrl.Images().ByName(imageName)
	if err != nil && err.Error() != "resource not found" {
		return err
	}

	if existingImageID < 0 {
		imageSpec := fmt.Sprintf("NAME = \"%s\"\n%s", imageName, imageContent)
		if _, err = i.ctrl.Images().Create(imageSpec, 1); err != nil {
			return fmt.Errorf("Failed to create image: %w", err)
		}
	}

	return nil
}

func (i *Images) ImageReady(imageName string) (bool, error) {
	existingImageID, err := i.ctrl.Images().ByName(imageName)
	if err != nil {
		return false, fmt.Errorf("Failed to find Image template: %s, %w", imageName, err)
	}

	image, err := i.ctrl.Image(existingImageID).Info(true)
	if err != nil {
		return false, fmt.Errorf("Failed to get Image info: %w", err)
	}

	state, err := image.State()
	if err != nil {
		return false, fmt.Errorf("Failed to get Image state: %w", err)
	}

	return state == goca_image.Ready || state == goca_image.Used, nil
}
