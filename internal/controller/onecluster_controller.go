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

package controller

import (
	"context"
	"fmt"
	"net"

	"github.com/pkg/errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"

	infrav1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/v1beta1"
	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/cloud"
)

// ONEClusterReconciler reconciles a ONECluster object
type ONEClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=oneclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=oneclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=oneclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *ONEClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	log := ctrl.LoggerFrom(ctx)

	oneCluster := &infrav1.ONECluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, oneCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	cluster, err := util.GetOwnerCluster(ctx, r.Client, oneCluster.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("Waiting for Cluster Controller to set OwnerRef on ONECluster")
		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(oneCluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		err := patchHelper.Patch(
			ctx,
			oneCluster,
			patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
				clusterv1.ReadyCondition,
			}},
		)
		if err != nil {
			log.Error(err, "Failed to patch ONECluster")
			if rerr == nil {
				rerr = err
			}
		}
	}()

	var (
		externalRouter  *cloud.Router
		externalCleanup *cloud.Cleanup
	)
	if oneCluster.Spec.VirtualRouter != nil {
		cloudClients, err := cloud.NewClients(ctx, r.Client, oneCluster)
		if err != nil {
			return ctrl.Result{}, err
		}
		vrName := fmt.Sprintf("%s-cp", oneCluster.Name)
		replicas := oneCluster.Spec.VirtualRouter.Replicas
		externalRouter = cloud.NewRouter(cloudClients, vrName, replicas)
		externalCleanup = cloud.NewCleanup(cloudClients, oneCluster.Name)
	}

	if !oneCluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, oneCluster, externalRouter, externalCleanup)
	}

	if !controllerutil.ContainsFinalizer(oneCluster, infrav1.ClusterFinalizer) {
		controllerutil.AddFinalizer(oneCluster, infrav1.ClusterFinalizer)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, r.reconcileNormal(ctx, oneCluster, externalRouter)
}

func (r *ONEClusterReconciler) reconcileNormal(ctx context.Context, oneCluster *infrav1.ONECluster, externalRouter *cloud.Router) error {
	if externalRouter != nil {
		externalRouter.ByName(externalRouter.Name)
		if !externalRouter.Exists() {
			if err := externalRouter.FromTemplate(
				oneCluster.Spec.VirtualRouter,
				oneCluster.Spec.PublicNetwork,
				oneCluster.Spec.PrivateNetwork,
			); err != nil {
				return errors.Wrap(err, "failed to create VR")
			}
		}

		if oneCluster.Spec.ControlPlaneEndpoint.Host == "" {
			if len(externalRouter.FloatingIPs) > 0 && net.ParseIP(externalRouter.FloatingIPs[0]) != nil {
				oneCluster.Spec.ControlPlaneEndpoint.Host = externalRouter.FloatingIPs[0]
			}
		}
		// TODO: use webhook?
		if oneCluster.Spec.ControlPlaneEndpoint.Port == 0 {
			oneCluster.Spec.ControlPlaneEndpoint.Port = 6443
		}

		if oneCluster.Spec.PrivateNetwork != nil {
			if oneCluster.Spec.PrivateNetwork.FloatingIP == nil {
				ipIndex := 0
				if oneCluster.Spec.PublicNetwork != nil {
					ipIndex++
				}
				oneCluster.Spec.PrivateNetwork.FloatingIP = &externalRouter.FloatingIPs[ipIndex]
			}
			if oneCluster.Spec.PrivateNetwork.Gateway == nil {
				oneCluster.Spec.PrivateNetwork.Gateway = oneCluster.Spec.PrivateNetwork.FloatingIP
			}
			if oneCluster.Spec.PrivateNetwork.DNS == nil {
				oneCluster.Spec.PrivateNetwork.DNS = oneCluster.Spec.PrivateNetwork.FloatingIP
			}
		}
	}

	oneCluster.Status.Ready = true
	return nil
}

func (r *ONEClusterReconciler) reconcileDelete(ctx context.Context, oneCluster *infrav1.ONECluster, externalRouter *cloud.Router, externalCleanup *cloud.Cleanup) error {
	if externalRouter != nil {
		externalRouter.ByName(externalRouter.Name)
		if err := externalRouter.Delete(); err != nil {
			return errors.Wrap(err, "failed to delete VR")
		}
	}
	if externalCleanup != nil {
		if err := externalCleanup.DeleteLBVirtualRouter(); err != nil {
			return errors.Wrap(err, "failed to cleanup LB virtual router")
		}
		if err := externalCleanup.DeleteVRReservation(); err != nil {
			return errors.Wrap(err, "failed to cleanup VR reservation")
		}
		if err := externalCleanup.DeleteLBReservation(); err != nil {
			return errors.Wrap(err, "failed to cleanup LB reservation")
		}
	}
	controllerutil.RemoveFinalizer(oneCluster, infrav1.ClusterFinalizer)
	return nil
}

func (r *ONEClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ONECluster{}).
		Complete(r)
}
