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
	"strings"
	"testing"
)

func TestContentHashChangesWithContent(t *testing.T) {
	a := contentHash("CPU = 1\nMEMORY = 1024\n")
	if a != contentHash("CPU = 1\nMEMORY = 1024\n") {
		t.Fatal("hash is not stable for identical content")
	}
	if a == contentHash("CPU = 2\nMEMORY = 1024\n") {
		t.Fatal("hash did not change with the content")
	}
}

func TestTemplateBody(t *testing.T) {
	content := "CPU = 1\n"
	body := templateBody("tpl-uid", content)

	for _, want := range []string{
		`CLUSTER_UID = "tpl-uid"`,
		contentHashAttribute + ` = "` + contentHash(content) + `"`,
		content,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q does not contain %q", body, want)
		}
	}
	if strings.Contains(body, "NAME") {
		t.Errorf("body must not contain NAME (not allowed on update): %q", body)
	}
}
