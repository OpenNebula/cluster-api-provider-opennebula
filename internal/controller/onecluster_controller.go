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
	"time"

	"github.com/pkg/errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
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

	if annotations.IsPaused(cluster, oneCluster) {
		log.Info("Reconciliation is paused for this object")
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
		externalImages    *cloud.Images
		externalTemplates *cloud.Templates
		externalRouter    *cloud.Router
		externalCleanup   *cloud.Cleanup
	)
	if len(oneCluster.Spec.Images) > 0 || len(oneCluster.Spec.Templates) > 0 || oneCluster.Spec.VirtualRouter != nil {
		cloudClients, err := cloud.NewClients(ctx, r.Client, oneCluster)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(oneCluster.Spec.Images) > 0 {
			externalImages, err = cloud.NewImages(cloudClients)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to initialize cloud images")
			}
		}
		if len(oneCluster.Spec.Templates) > 0 {
			externalTemplates, err = cloud.NewTemplates(cloudClients, string(oneCluster.UID))
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to initialize cloud templates")
			}
		}
		if oneCluster.Spec.VirtualRouter != nil {
			routerOpts := []cloud.RouterOption{
				cloud.WithRouterName(fmt.Sprintf("%s-cp", oneCluster.Name)),
			}
			if oneCluster.Spec.VirtualRouter.Replicas != nil {
				routerOpts = append(routerOpts,
					cloud.WithRouterReplicas(int(*oneCluster.Spec.VirtualRouter.Replicas)),
				)
			}
			externalRouter, err = cloud.NewRouter(cloudClients, routerOpts...)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to initialize cloud router")
			}
			externalCleanup, err = cloud.NewCleanup(cloudClients, oneCluster.Name)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to initialize cloud cleanup")
			}
		}
	}

	if !oneCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, oneCluster, externalRouter, externalCleanup)
	}

	if !controllerutil.ContainsFinalizer(oneCluster, infrav1.ClusterFinalizer) {
		controllerutil.AddFinalizer(oneCluster, infrav1.ClusterFinalizer)
		return ctrl.Result{}, nil
	}

	return r.reconcileNormal(ctx, oneCluster, externalImages, externalTemplates, externalRouter)
}

func (r *ONEClusterReconciler) reconcileNormal(
	ctx context.Context,
	oneCluster *infrav1.ONECluster,
	externalImages *cloud.Images, externalTemplates *cloud.Templates, externalRouter *cloud.Router) (ctrl.Result, error) {

	if externalImages != nil {
		imagesReady := true
		for _, image := range oneCluster.Spec.Images {
			if image.ImageName != "" && image.ImageContent != "" {
				if err := externalImages.CreateImage(
					image.ImageName,
					image.ImageContent,
				); err != nil {
					return ctrl.Result{}, errors.Wrap(err, "failed to create images")
				}
				imageReady, _ := externalImages.ImageReady(image.ImageName)
				imagesReady = imagesReady && imageReady
			}
		}
		if !imagesReady {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	if externalTemplates != nil {
		for _, template := range oneCluster.Spec.Templates {
			if template.TemplateName != "" && template.TemplateContent != "" {
				if err := externalTemplates.CreateTemplate(
					template.TemplateName,
					template.TemplateContent,
				); err != nil {
					return ctrl.Result{}, errors.Wrap(err, "failed to create templates")
				}
			}
		}
	}

	if externalRouter != nil {
		externalRouter.ByName(externalRouter.Name)
		if !externalRouter.Exists() {
			if err := externalRouter.FromTemplate(
				oneCluster.Spec.VirtualRouter,
				oneCluster.Spec.PublicNetwork,
				oneCluster.Spec.PrivateNetwork,
			); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to create VR")
			}

			if oneCluster.Spec.ControlPlaneEndpoint.Host == "" {
				if len(externalRouter.FloatingIPs) > 0 && net.ParseIP(externalRouter.FloatingIPs[0]) != nil {
					oneCluster.Spec.ControlPlaneEndpoint.Host = externalRouter.FloatingIPs[0]
				}
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
	}

	if oneCluster.Spec.ControlPlaneEndpoint.Host == "" {
		return ctrl.Result{}, fmt.Errorf("Spec.ControlPlaneEndpoint.Host must not be empty")
	}

	// TODO: use webhook?
	if oneCluster.Spec.ControlPlaneEndpoint.Port == 0 {
		oneCluster.Spec.ControlPlaneEndpoint.Port = 6443
	}

	oneCluster.Status.Ready = true
	return ctrl.Result{}, nil
}

func (r *ONEClusterReconciler) reconcileDelete(
	ctx context.Context,
	oneCluster *infrav1.ONECluster,
	externalRouter *cloud.Router, externalCleanup *cloud.Cleanup) (ctrl.Result, error) {

	if externalRouter != nil {
		externalRouter.ByName(externalRouter.Name)
		if err := externalRouter.Delete(); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to delete VR")
		}
	}

	if externalCleanup != nil {
		if err := externalCleanup.DeleteLBVirtualRouter(); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to cleanup LB virtual router")
		}
		if err := externalCleanup.DeleteVRReservation(); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to cleanup VR reservation")
		}
		if err := externalCleanup.DeleteLBReservation(); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to cleanup LB reservation")
		}
	}

	controllerutil.RemoveFinalizer(oneCluster, infrav1.ClusterFinalizer)
	return ctrl.Result{}, nil
}

func (r *ONEClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ONECluster{}).
		Complete(r)
}
