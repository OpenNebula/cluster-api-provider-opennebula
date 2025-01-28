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

package e2e

import (
	"context"
	"regexp"
	"time"

	"sigs.k8s.io/cluster-api/test/framework/clusterctl"

	goca "github.com/OpenNebula/one/src/oca/go/src/goca"
)

func WaitForVRsToBeDeleted(ctx context.Context, nameRegex string, e2eConfig *clusterctl.E2EConfig, retries, seconds int) (bool, error) {
	re, err := regexp.Compile(nameRegex)
	if err != nil {
		return false, err
	}

	ctrl := goca.NewController(goca.NewDefaultClient(goca.OneConfig{
		Endpoint: e2eConfig.GetVariable("ONE_XMLRPC"),
		Token:    e2eConfig.GetVariable("ONE_AUTH"),
	}))

	for retry := 0; retry < retries; retry++ {
		pool, err := ctrl.VirtualRouters().InfoContext(ctx)
		if err != nil {
			return false, err
		}
		found := false
		for _, vr := range pool.VirtualRouters {
			if re.MatchString(vr.Name) {
				time.Sleep(time.Duration(seconds) * time.Second)
				found = true
				break
			}
		}
		if !found {
			return true, nil
		}
	}
	return false, nil
}
