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
	"unsafe"

	goca_dyn "github.com/OpenNebula/one/src/oca/go/src/goca/dynamic"
	goca_vr "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/virtualrouter"
	goca_vm "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

func getNICs(maybeTemplate interface{}) (nics []*goca_dyn.Vector) {
	nics = make([]*goca_dyn.Vector, 0, 1)

	var template *goca_dyn.Template
	switch v := maybeTemplate.(type) {
	case *goca_vm.Template:
		template = (*goca_dyn.Template)(unsafe.Pointer(v))
	case *goca_vr.Template:
		template = (*goca_dyn.Template)(unsafe.Pointer(v))
	default:
		return
	}

	for _, maybeNIC := range template.Elements {
		// NOTE: do NOT use AddNIC()
		if v, ok := maybeNIC.(*goca_dyn.Vector); ok {
			if v.XMLName.Local == "NIC" {
				nics = append(nics, v)
			}
		}
	}

	return
}

func ensureNIC(maybeTemplate interface{}, index int) *goca_dyn.Vector {
	if index < 0 {
		return nil
	}

	nics := make([]*goca_dyn.Vector, 0, 1)

	var template *goca_dyn.Template
	switch v := maybeTemplate.(type) {
	case *goca_vm.Template:
		template = (*goca_dyn.Template)(unsafe.Pointer(v))
	case *goca_vr.Template:
		template = (*goca_dyn.Template)(unsafe.Pointer(v))
	default:
		return nil
	}

	for _, maybeNIC := range template.Elements {
		// NOTE: do NOT use AddNIC()
		if v, ok := maybeNIC.(*goca_dyn.Vector); ok {
			if v.XMLName.Local == "NIC" {
				nics = append(nics, v)
			}
		}
	}

	if index < len(nics) {
		return nics[index]
	} else {
		var nicVec *goca_dyn.Vector
		for k := len(nics); k <= index; k++ {
			nicVec = &goca_dyn.Vector{XMLName: xml.Name{Local: "NIC"}}
			template.Elements = append(template.Elements, nicVec)
		}
		return nicVec
	}
}

func updateContext(contextVec *goca_dyn.Vector, contextMap *map[string]string) *goca_dyn.Vector {
	if contextVec != nil && contextMap != nil {
		for k, v := range *contextMap {
			contextVec.Del(k)
			contextVec.AddPair(k, v)
		}
	}
	return contextVec
}
