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
	"time"

	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	utilexp "sigs.k8s.io/cluster-api/exp/util"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/labels"
	clog "sigs.k8s.io/cluster-api/util/log"
	"sigs.k8s.io/cluster-api/util/patch"

	infrav1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/v1beta1"
	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/cloud"
)

// ONEMachineReconciler reconciles a ONEMachine object
type ONEMachineReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Tracker *remote.ClusterCacheTracker
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=onemachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=onemachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=onemachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinesets;machinesets/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *ONEMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	log := log.FromContext(ctx)

	oneMachine := &infrav1.ONEMachine{}
	if err := r.Client.Get(ctx, req.NamespacedName, oneMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	ctx, log, err := clog.AddOwners(ctx, r.Client, oneMachine)
	if err != nil {
		return ctrl.Result{}, err
	}

	machine, err := util.GetOwnerMachine(ctx, r.Client, oneMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on ONEMachine")
		return ctrl.Result{}, nil
	}

	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("ONEMachine owner Machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info(fmt.Sprintf("Please associate this machine with a cluster using the label %s: <name of cluster>", clusterv1.ClusterNameLabel))
		return ctrl.Result{}, nil
	}

	if annotations.IsPaused(cluster, oneMachine) {
		log.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	if cluster.Spec.InfrastructureRef == nil {
		log.Info("Cluster infrastructureRef is not available yet")
		return ctrl.Result{}, nil
	}

	oneCluster := &infrav1.ONECluster{}
	oneClusterName := client.ObjectKey{
		Namespace: oneMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, oneClusterName, oneCluster); err != nil {
		log.Info("ONECluster is not available yet")
		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(oneMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		err := patchHelper.Patch(
			ctx,
			oneMachine,
			patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
				clusterv1.ReadyCondition,
			}},
		)
		if err != nil {
			log.Error(err, "Failed to patch ONEMachine")
			if rerr == nil {
				rerr = err
			}
		}
	}()

	if oneMachine.ObjectMeta.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(oneMachine, infrav1.MachineFinalizer) {
		controllerutil.AddFinalizer(oneMachine, infrav1.MachineFinalizer)
		return ctrl.Result{}, nil
	}

	name := machine.Name
	if labels.IsMachinePoolOwned(oneMachine) {
		name = oneMachine.Name
	}
	cloudClients, err := cloud.NewClients(ctx, r.Client, oneCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	externalMachine := cloud.NewMachine(cloudClients, &name)

	if !oneMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, oneCluster, machine, oneMachine, externalMachine)
	}

	res, err := r.reconcileNormal(ctx, cluster, oneCluster, machine, oneMachine, externalMachine)
	if errors.Is(err, remote.ErrClusterLocked) {
		log.V(5).Info("Requeuing because another worker has the lock on the ClusterCacheTracker")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	return res, err
}

func (r *ONEMachineReconciler) reconcileNormal(ctx context.Context, cluster *clusterv1.Cluster, oneCluster *infrav1.ONECluster, machine *clusterv1.Machine, oneMachine *infrav1.ONEMachine, externalMachine *cloud.Machine) (res ctrl.Result, retErr error) {
	log := log.FromContext(ctx)

	if !cluster.Status.InfrastructureReady {
		log.Info("Waiting for Cluster Controller to create cluster infrastructure")
		return ctrl.Result{}, nil
	}

	var dataSecretName *string
	if labels.IsMachinePoolOwned(oneMachine) {
		machinePool, err := utilexp.GetMachinePoolByLabels(ctx, r.Client, oneMachine.GetNamespace(), oneMachine.Labels)
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to get machine pool for ONEMachine")
		}
		if machinePool == nil {
			log.Info("No MachinePool matching labels found, returning without error")
			return ctrl.Result{}, nil
		}

		dataSecretName = machinePool.Spec.Template.Spec.Bootstrap.DataSecretName
	} else {
		dataSecretName = machine.Spec.Bootstrap.DataSecretName
	}

	if oneMachine.Spec.ProviderID != nil {
		if err := externalMachine.ByName(oneMachine.Name); err != nil {
			return ctrl.Result{}, err
		}

		setMachineAddress(oneMachine, externalMachine.Address4)
		oneMachine.Status.Ready = true
		return ctrl.Result{}, nil
	}

	if dataSecretName == nil {
		if !util.IsControlPlaneMachine(machine) && !conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
			log.Info("Waiting for the control plane to be initialized")
			return ctrl.Result{}, nil
		}

		log.Info("Waiting for the Bootstrap provider controller to set bootstrap data")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	var dataSecret corev1.Secret
	key := client.ObjectKey{
		Namespace: oneCluster.Namespace,
		Name:      *dataSecretName,
	}
	if err := r.Client.Get(ctx, key, &dataSecret); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "Failed to get data secret")
	}

	externalMachine.ByName(oneMachine.Name)
	if !externalMachine.Exists() {
		userData := string(dataSecret.Data["value"])
		if oneMachine.Spec.UserData != nil {
			userData += "\n"
			userData += *oneMachine.Spec.UserData
		}
		if err := externalMachine.FromTemplate(oneMachine.Spec.TemplateName, &userData); err != nil {
			return ctrl.Result{}, err
		}
	}
	setMachineAddress(oneMachine, externalMachine.Address4)

	if cluster.Spec.ControlPlaneRef != nil && !conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	remoteClient, err := r.Tracker.GetClient(ctx, client.ObjectKeyFromObject(cluster))
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to generate workload cluster client")
	}
	if err := cloudProviderNodePatch(ctx, remoteClient, *externalMachine.NodeName(), *externalMachine.ProviderID()); err != nil {
		log.Error(err, "Failed to patch the Kubernetes node with the machine providerID")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	oneMachine.Spec.ProviderID = externalMachine.ProviderID()
	oneMachine.Status.Ready = true
	return ctrl.Result{}, nil
}

func cloudProviderNodePatch(ctx context.Context, c client.Client, nodeName, providerID string) error {
	log := ctrl.LoggerFrom(ctx)

	node := &corev1.Node{}
	if err := c.Get(ctx, apimachinerytypes.NamespacedName{Name: nodeName}, node); err != nil {
		return errors.Wrap(err, "unable to complete Cloud Provider tasks: failed to retrieve node")
	}

	patchHelper, err := patch.NewHelper(node, c)
	if err != nil {
		return err
	}

	log.Info("Setting Kubernetes node providerID")
	node.Spec.ProviderID = providerID
	return patchHelper.Patch(ctx, node)
}

func setMachineAddress(oneMachine *infrav1.ONEMachine, address string) {
	oneMachine.Status.Addresses = []clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalIP, Address: address},
		{Type: clusterv1.MachineInternalIP, Address: address},
	}
}

func (r *ONEMachineReconciler) reconcileDelete(ctx context.Context, oneCluster *infrav1.ONECluster, machine *clusterv1.Machine, oneMachine *infrav1.ONEMachine, externalMachine *cloud.Machine) error {
	externalMachine.ByName(oneMachine.Name)
	if err := externalMachine.Delete(); err != nil {
		return errors.Wrap(err, "failed to delete ONEMachine")
	}

	controllerutil.RemoveFinalizer(oneMachine, infrav1.MachineFinalizer)
	return nil
}

func (r *ONEMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ONEMachine{}).
		Complete(r)
}
