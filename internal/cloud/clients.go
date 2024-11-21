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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/v1beta1"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
)

type Clients struct {
	RPC2 *goca.Client
}

func NewClients(ctx context.Context, c client.Client, oneCluster *infrav1.ONECluster) (*Clients, error) {
	rpc2, err := newRPC2(ctx, c, oneCluster)
	if err != nil {
		return nil, err
	}

	return &Clients{RPC2: rpc2}, nil
}

func newRPC2(ctx context.Context, c client.Client, oneCluster *infrav1.ONECluster) (*goca.Client, error) {
	var secret corev1.Secret
	key := client.ObjectKey{
		Namespace: oneCluster.Namespace,
		Name:      oneCluster.Spec.SecretName,
	}
	if err := c.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("Failed to get secret: %w", err)
	}

	secret.SetOwnerReferences(util.EnsureOwnerRef(secret.OwnerReferences, metav1.OwnerReference{
		APIVersion: infrav1.GroupVersion.String(),
		Kind:       "ONECluster",
		Name:       oneCluster.Name,
		UID:        oneCluster.UID,
	}))
	if err := c.Update(ctx, &secret); err != nil {
		return nil, fmt.Errorf("Failed to set ownerReference to secret: %w", err)
	}

	return goca.NewDefaultClient(goca.OneConfig{
		Endpoint: string(secret.Data["ONE_XMLRPC"]),
		Token:    string(secret.Data["ONE_AUTH"]),
	}), nil
}
