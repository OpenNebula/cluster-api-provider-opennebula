/*
Copyright 2025, OpenNebula Project, OpenNebula Systems.

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

// NOTE: Those helper functions are based on cluster-api e2e framework and clusterctl framework ones
// adapted to work with the RKE2 control plane provider instead of the default kubeadm one.
// references:
// - https://github.com/kubernetes-sigs/cluster-api/blob/068c0f3c8ed3a7e382952f8ae68b648aee240a60/test/e2e/quick_start.go
// - https://github.com/kubernetes-sigs/cluster-api/blob/068c0f3c8ed3a7e382952f8ae68b648aee240a60/test/framework/clusterctl/clusterctl_helpers.go
package e2e

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/OpenNebula/cluster-api-provider-opennebula/test/helpers"

	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1 "github.com/rancher/cluster-api-provider-rke2/controlplane/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	expv1 "sigs.k8s.io/cluster-api/exp/api/v1beta1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Test suite constants for e2e config variables.
const (
	KubernetesVersion = "KUBERNETES_VERSION"
)

// QuickStartSpecInput is the input for QuickStartSpec.
type QuickStartSpecInput struct {
	E2EConfig             *clusterctl.E2EConfig
	ClusterctlConfigPath  string
	BootstrapClusterProxy framework.ClusterProxy
	ArtifactFolder        string
	SkipCleanup           bool

	// Cluster name allows to specify a deterministic clusterName.
	// If not set, a random one will be generated.
	ClusterName *string

	// DeployClusterClassInSeparateNamespace defines if the ClusterClass should be deployed in a separate namespace.
	DeployClusterClassInSeparateNamespace bool

	// InfrastructureProvider allows to specify the infrastructure provider to be used when looking for
	// cluster templates.
	// If not set, clusterctl will look at the infrastructure provider installed in the management cluster;
	// if only one infrastructure provider exists, it will be used, otherwise the operation will fail if more than one exists.
	InfrastructureProvider *string

	// Flavor, if specified is the template flavor used to create the cluster for testing.
	// If not specified, the default flavor for the selected infrastructure provider is used.
	Flavor *string

	// ControlPlaneMachineCount defines the number of control plane machines to be added to the workload cluster.
	// If not specified, 1 will be used.
	ControlPlaneMachineCount *int64

	// WorkerMachineCount defines number of worker machines to be added to the workload cluster.
	// If not specified, 1 will be used.
	WorkerMachineCount *int64

	// Allows to inject functions to be run while waiting for the control plane to be initialized,
	// which unblocks CNI installation, and for the control plane machines to be ready (after CNI installation).
	ControlPlaneWaiters ControlPlaneWaiters

	// Allows to inject a function to be run after test namespace is created.
	// If not specified, this is a no-op.
	PostNamespaceCreated func(managementClusterProxy framework.ClusterProxy, workloadClusterNamespace string)

	// Allows to inject a function to be run after machines are provisioned.
	// If not specified, this is a no-op.
	PostMachinesProvisioned func(managementClusterProxy framework.ClusterProxy, workloadClusterNamespace, workloadClusterName string)

	// ClusterctlVariables allows injecting variables to the cluster template.
	// If not specified, this is a no-op.
	ClusterctlVariables map[string]string
}

// QuickStartSpec implements a spec that mimics the operation described in the Cluster API quick start, that is
// creating a workload cluster.
// This test is meant to provide a first, fast signal to detect regression; it is recommended to use it as a PR blocker test.
// NOTE: This test works with Clusters with and without ClusterClass.
func QuickStartSpec(ctx context.Context, inputGetter func() QuickStartSpecInput) {
	var (
		specName              = "quick-start"
		input                 QuickStartSpecInput
		namespace             *corev1.Namespace
		clusterClassNamespace *corev1.Namespace
		cancelWatches         context.CancelFunc
		clusterResources      *ApplyClusterTemplateAndWaitResult
	)

	BeforeEach(func() {
		Expect(ctx).NotTo(BeNil(), "ctx is required for %s spec", specName)
		input = inputGetter()
		Expect(input.E2EConfig).ToNot(BeNil(), "Invalid argument. input.E2EConfig can't be nil when calling %s spec", specName)
		Expect(input.ClusterctlConfigPath).To(BeAnExistingFile(), "Invalid argument. input.ClusterctlConfigPath must be an existing file when calling %s spec", specName)
		Expect(input.BootstrapClusterProxy).ToNot(BeNil(), "Invalid argument. input.BootstrapClusterProxy can't be nil when calling %s spec", specName)
		Expect(os.MkdirAll(input.ArtifactFolder, 0750)).To(Succeed(), "Invalid argument. input.ArtifactFolder can't be created for %s spec", specName)

		Expect(input.E2EConfig.Variables).To(HaveKey(KubernetesVersion))

		// Setup a Namespace where to host objects for this spec and create a watcher for the namespace events.
		namespace, cancelWatches = framework.SetupSpecNamespace(ctx, specName, input.BootstrapClusterProxy, input.ArtifactFolder, input.PostNamespaceCreated)

		if input.DeployClusterClassInSeparateNamespace {
			clusterClassNamespace = framework.CreateNamespace(ctx, framework.CreateNamespaceInput{Creator: input.BootstrapClusterProxy.GetClient(), Name: fmt.Sprintf("%s-clusterclass", namespace.Name)}, "40s", "10s")
			Expect(clusterClassNamespace).ToNot(BeNil(), "Failed to create namespace")
		}

		clusterResources = new(ApplyClusterTemplateAndWaitResult)
	})

	It("Should create a workload cluster", func() {
		By("Creating a workload cluster")

		infrastructureProvider := clusterctl.DefaultInfrastructureProvider
		if input.InfrastructureProvider != nil {
			infrastructureProvider = *input.InfrastructureProvider
		}

		flavor := clusterctl.DefaultFlavor
		if input.Flavor != nil {
			flavor = *input.Flavor
		}

		controlPlaneMachineCount := ptr.To[int64](1)
		if input.ControlPlaneMachineCount != nil {
			controlPlaneMachineCount = input.ControlPlaneMachineCount
		}

		workerMachineCount := ptr.To[int64](1)
		if input.WorkerMachineCount != nil {
			workerMachineCount = input.WorkerMachineCount
		}

		clusterName := fmt.Sprintf("%s-%s", specName, util.RandomString(6))
		if input.ClusterName != nil {
			clusterName = *input.ClusterName
		}

		variables := map[string]string{}
		maps.Copy(variables, input.ClusterctlVariables)

		if input.DeployClusterClassInSeparateNamespace {
			variables["CLUSTER_CLASS_NAMESPACE"] = clusterClassNamespace.Name
			By("Creating a cluster referencing a ClusterClass from another namespace")
		}

		ApplyClusterTemplateAndWait(ctx, ApplyClusterTemplateAndWaitInput{
			ClusterProxy: input.BootstrapClusterProxy,
			ConfigCluster: clusterctl.ConfigClusterInput{
				LogFolder:                filepath.Join(input.ArtifactFolder, "clusters", input.BootstrapClusterProxy.GetName()),
				ClusterctlConfigPath:     input.ClusterctlConfigPath,
				ClusterctlVariables:      variables,
				KubeconfigPath:           input.BootstrapClusterProxy.GetKubeconfigPath(),
				InfrastructureProvider:   infrastructureProvider,
				Flavor:                   flavor,
				Namespace:                namespace.Name,
				ClusterName:              clusterName,
				KubernetesVersion:        input.E2EConfig.GetVariable(KubernetesVersion),
				ControlPlaneMachineCount: controlPlaneMachineCount,
				WorkerMachineCount:       workerMachineCount,
			},
			ControlPlaneWaiters:          input.ControlPlaneWaiters,
			WaitForClusterIntervals:      input.E2EConfig.GetIntervals(specName, "wait-cluster"),
			WaitForControlPlaneIntervals: input.E2EConfig.GetIntervals(specName, "wait-control-plane"),
			WaitForMachineDeployments:    input.E2EConfig.GetIntervals(specName, "wait-worker-nodes"),
			PostMachinesProvisioned: func() {
				if input.PostMachinesProvisioned != nil {
					input.PostMachinesProvisioned(input.BootstrapClusterProxy, namespace.Name, clusterName)
				}
			},
		}, clusterResources)

		By("PASSED!")
	})

	AfterEach(func() {
		// Dumps all the resources in the spec namespace, then cleanups the cluster object and the spec namespace itself.
		framework.DumpSpecResourcesAndCleanup(ctx, specName, input.BootstrapClusterProxy, input.ArtifactFolder, namespace, cancelWatches, clusterResources.Cluster, input.E2EConfig.GetIntervals, input.SkipCleanup)
		if input.DeployClusterClassInSeparateNamespace && !input.SkipCleanup {
			framework.DeleteNamespace(ctx, framework.DeleteNamespaceInput{
				Deleter: input.BootstrapClusterProxy.GetClient(),
				Name:    clusterClassNamespace.Name,
			})
		}
	})
}

const (
	retryableOperationInterval = 3 * time.Second
	// retryableOperationTimeout requires a higher value especially for self-hosted upgrades.
	// Short unavailability of the Kube APIServer due to joining etcd members paired with unreachable conversion webhooks due to
	// failed leader election and thus controller restarts lead to longer taking retries.
	// The timeout occurs when listing machines in `GetControlPlaneMachinesByCluster`.
	retryableOperationTimeout = 3 * time.Minute
)

type ApplyClusterTemplateAndWaitInput struct {
	ClusterProxy                 framework.ClusterProxy
	ConfigCluster                clusterctl.ConfigClusterInput
	CNIManifestPath              string
	WaitForClusterIntervals      []interface{}
	WaitForControlPlaneIntervals []interface{}
	WaitForMachineDeployments    []interface{}
	WaitForMachinePools          []interface{}
	CreateOrUpdateOpts           []framework.CreateOrUpdateOption // options to be passed to CreateOrUpdate function config
	PreWaitForCluster            func()
	PostMachinesProvisioned      func()
	ControlPlaneWaiters
}

type ApplyClusterTemplateAndWaitResult struct {
	ClusterClass       *clusterv1.ClusterClass
	Cluster            *clusterv1.Cluster
	ControlPlane       *controlplanev1.RKE2ControlPlane
	MachineDeployments []*clusterv1.MachineDeployment
	MachinePools       []*expv1.MachinePool
}

// Waiter is a function that runs and waits for a long-running operation to finish and updates the result.
type Waiter func(ctx context.Context, input ApplyCustomClusterTemplateAndWaitInput, result *ApplyCustomClusterTemplateAndWaitResult)

type ControlPlaneWaiters struct {
	WaitForControlPlaneInitialized   Waiter
	WaitForControlPlaneMachinesReady Waiter
}

// Custom override of clusterctl.ApplyCustomClusterTemplateAndWait
func ApplyClusterTemplateAndWait(ctx context.Context, input ApplyClusterTemplateAndWaitInput, result *ApplyClusterTemplateAndWaitResult) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for ApplyClusterTemplateAndWait")
	Expect(input.ClusterProxy).ToNot(BeNil(), "Invalid argument. input.ClusterProxy can't be nil when calling ApplyClusterTemplateAndWait")
	Expect(result).ToNot(BeNil(), "Invalid argument. result can't be nil when calling ApplyClusterTemplateAndWait")
	Expect(input.ConfigCluster.ControlPlaneMachineCount).ToNot(BeNil())

	var workerMachinesCount string
	if input.ConfigCluster.WorkerMachineCount != nil {
		workerMachinesCount = fmt.Sprintf("%d", *input.ConfigCluster.WorkerMachineCount)
	} else {
		workerMachinesCount = "(unset)"
	}
	logInfo("Creating the workload cluster with name %q using the %q template (Kubernetes %s, %d control-plane machines, %s worker machines)",
		input.ConfigCluster.ClusterName, helpers.ValueOrDefault(input.ConfigCluster.Flavor, "(default)"), input.ConfigCluster.KubernetesVersion, *input.ConfigCluster.ControlPlaneMachineCount, workerMachinesCount)

	// Ensure we have a Cluster for dump and cleanup steps in AfterEach even if ApplyClusterTemplateAndWait fails.
	result.Cluster = &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      input.ConfigCluster.ClusterName,
			Namespace: input.ConfigCluster.Namespace,
		},
	}

	logInfo("Getting the cluster template yaml")

	workloadClusterTemplate := clusterctl.ConfigCluster(ctx, clusterctl.ConfigClusterInput{
		// pass reference to the management cluster hosting this test
		KubeconfigPath: input.ConfigCluster.KubeconfigPath,
		// pass the clusterctl config file that points to the local provider repository created for this test,
		ClusterctlConfigPath: input.ConfigCluster.ClusterctlConfigPath,
		// select template
		Flavor: input.ConfigCluster.Flavor,
		// define template variables
		Namespace:                input.ConfigCluster.Namespace,
		ClusterName:              input.ConfigCluster.ClusterName,
		KubernetesVersion:        input.ConfigCluster.KubernetesVersion,
		ControlPlaneMachineCount: input.ConfigCluster.ControlPlaneMachineCount,
		WorkerMachineCount:       input.ConfigCluster.WorkerMachineCount,
		InfrastructureProvider:   input.ConfigCluster.InfrastructureProvider,
		// setup clusterctl logs folder
		LogFolder:           input.ConfigCluster.LogFolder,
		ClusterctlVariables: input.ConfigCluster.ClusterctlVariables,
	})
	Expect(workloadClusterTemplate).ToNot(BeNil(), "Failed to get the cluster template")

	ApplyCustomClusterTemplateAndWait(ctx, ApplyCustomClusterTemplateAndWaitInput{
		ClusterProxy:                 input.ClusterProxy,
		CustomTemplateYAML:           workloadClusterTemplate,
		ClusterName:                  input.ConfigCluster.ClusterName,
		Namespace:                    input.ConfigCluster.Namespace,
		CNIManifestPath:              input.CNIManifestPath,
		Flavor:                       input.ConfigCluster.Flavor,
		WaitForClusterIntervals:      input.WaitForClusterIntervals,
		WaitForControlPlaneIntervals: input.WaitForControlPlaneIntervals,
		WaitForMachineDeployments:    input.WaitForMachineDeployments,
		WaitForMachinePools:          input.WaitForMachinePools,
		CreateOrUpdateOpts:           input.CreateOrUpdateOpts,
		PreWaitForCluster:            input.PreWaitForCluster,
		PostMachinesProvisioned:      input.PostMachinesProvisioned,
		ControlPlaneWaiters:          input.ControlPlaneWaiters,
	}, (*ApplyCustomClusterTemplateAndWaitResult)(result))
}

type ApplyCustomClusterTemplateAndWaitInput struct {
	ClusterProxy                 framework.ClusterProxy
	CustomTemplateYAML           []byte
	ClusterName                  string
	Namespace                    string
	CNIManifestPath              string
	Flavor                       string
	WaitForClusterIntervals      []interface{}
	WaitForControlPlaneIntervals []interface{}
	WaitForMachineDeployments    []interface{}
	WaitForMachinePools          []interface{}
	CreateOrUpdateOpts           []framework.CreateOrUpdateOption // options to be passed to CreateOrUpdate function config
	PreWaitForCluster            func()
	PostMachinesProvisioned      func()
	ControlPlaneWaiters
}

type ApplyCustomClusterTemplateAndWaitResult struct {
	ClusterClass       *clusterv1.ClusterClass
	Cluster            *clusterv1.Cluster
	ControlPlane       *controlplanev1.RKE2ControlPlane //TODO: abstract by interface
	MachineDeployments []*clusterv1.MachineDeployment
	MachinePools       []*expv1.MachinePool
}

func ApplyCustomClusterTemplateAndWait(ctx context.Context, input ApplyCustomClusterTemplateAndWaitInput, result *ApplyCustomClusterTemplateAndWaitResult) {
	setDefaults(&input)
	Expect(ctx).NotTo(BeNil(), "ctx is required for ApplyCustomClusterTemplateAndWait")
	Expect(input.ClusterProxy).ToNot(BeNil(), "Invalid argument. input.ClusterProxy can't be nil when calling ApplyCustomClusterTemplateAndWait")
	Expect(input.CustomTemplateYAML).NotTo(BeEmpty(), "Invalid argument. input.CustomTemplateYAML can't be empty when calling ApplyCustomClusterTemplateAndWait")
	Expect(input.ClusterName).NotTo(BeEmpty(), "Invalid argument. input.ClusterName can't be empty when calling ApplyCustomClusterTemplateAndWait")
	Expect(input.Namespace).NotTo(BeEmpty(), "Invalid argument. input.Namespace can't be empty when calling ApplyCustomClusterTemplateAndWait")
	Expect(result).ToNot(BeNil(), "Invalid argument. result can't be nil when calling ApplyClusterTemplateAndWait")

	logInfo("Creating the workload cluster with name %q from the provided yaml", input.ClusterName)

	// Ensure we have a Cluster for dump and cleanup steps in AfterEach even if ApplyClusterTemplateAndWait fails.
	result.Cluster = &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      input.ClusterName,
			Namespace: input.Namespace,
		},
	}

	logInfo("Applying the cluster template yaml of cluster %s", klog.KRef(input.Namespace, input.ClusterName))
	Eventually(func() error {
		return input.ClusterProxy.CreateOrUpdate(ctx, input.CustomTemplateYAML, input.CreateOrUpdateOpts...)
	}, 1*time.Minute).Should(Succeed(), "Failed to apply the cluster template")

	// Once we applied the cluster template we can run PreWaitForCluster.
	// Note: This can e.g. be used to verify the BeforeClusterCreate lifecycle hook is executed
	// and blocking correctly.
	if input.PreWaitForCluster != nil {
		logInfo("Calling PreWaitForCluster for cluster %s", klog.KRef(input.Namespace, input.ClusterName))
		input.PreWaitForCluster()
	}

	logInfo("Waiting for the cluster infrastructure of cluster %s to be provisioned", klog.KRef(input.Namespace, input.ClusterName))
	result.Cluster = framework.DiscoveryAndWaitForCluster(ctx, framework.DiscoveryAndWaitForClusterInput{
		Getter:    input.ClusterProxy.GetClient(),
		Namespace: input.Namespace,
		Name:      input.ClusterName,
	}, input.WaitForClusterIntervals...)

	if result.Cluster.Spec.Topology != nil {
		result.ClusterClass = framework.GetClusterClassByName(ctx, framework.GetClusterClassByNameInput{
			Getter:    input.ClusterProxy.GetClient(),
			Namespace: result.Cluster.GetClassKey().Namespace,
			Name:      result.Cluster.Spec.Topology.Class,
		})
	}

	logInfo("Waiting for control plane of cluster %s to be initialized", klog.KRef(input.Namespace, input.ClusterName))
	input.WaitForControlPlaneInitialized(ctx, input, result)

	if input.CNIManifestPath != "" {
		logInfo("Installing a CNI plugin to the workload cluster %s", klog.KRef(input.Namespace, input.ClusterName))
		workloadCluster := input.ClusterProxy.GetWorkloadCluster(ctx, result.Cluster.Namespace, result.Cluster.Name)

		cniYaml, err := os.ReadFile(input.CNIManifestPath)
		Expect(err).ShouldNot(HaveOccurred())

		Expect(workloadCluster.CreateOrUpdate(ctx, cniYaml)).ShouldNot(HaveOccurred())
	}

	logInfo("Waiting for control plane of cluster %s to be ready", klog.KRef(input.Namespace, input.ClusterName))
	input.WaitForControlPlaneMachinesReady(ctx, input, result)

	logInfo("Waiting for the machine deployments of cluster %s to be provisioned", klog.KRef(input.Namespace, input.ClusterName))
	result.MachineDeployments = framework.DiscoveryAndWaitForMachineDeployments(ctx, framework.DiscoveryAndWaitForMachineDeploymentsInput{
		Lister:  input.ClusterProxy.GetClient(),
		Cluster: result.Cluster,
	}, input.WaitForMachineDeployments...)

	logInfo("Waiting for the machine pools of cluster %s to be provisioned", klog.KRef(input.Namespace, input.ClusterName))
	result.MachinePools = framework.DiscoveryAndWaitForMachinePools(ctx, framework.DiscoveryAndWaitForMachinePoolsInput{
		Getter:  input.ClusterProxy.GetClient(),
		Lister:  input.ClusterProxy.GetClient(),
		Cluster: result.Cluster,
	}, input.WaitForMachinePools...)

	if input.PostMachinesProvisioned != nil {
		logInfo("Calling PostMachinesProvisioned for cluster %s", klog.KRef(input.Namespace, input.ClusterName))
		input.PostMachinesProvisioned()
	}
}

// setDefaults sets the default values for ApplyCustomClusterTemplateAndWaitInput if not set.
// Currently, we set the default ControlPlaneWaiters here.
func setDefaults(input *ApplyCustomClusterTemplateAndWaitInput) {
	if input.WaitForControlPlaneInitialized == nil {
		input.WaitForControlPlaneInitialized = func(ctx context.Context, input ApplyCustomClusterTemplateAndWaitInput, result *ApplyCustomClusterTemplateAndWaitResult) {
			result.ControlPlane = DiscoveryAndWaitForControlPlaneInitialized(ctx, framework.DiscoveryAndWaitForControlPlaneInitializedInput{
				Lister:  input.ClusterProxy.GetClient(),
				Cluster: result.Cluster,
			}, input.WaitForControlPlaneIntervals...)
		}
	}

	if input.WaitForControlPlaneMachinesReady == nil {
		input.WaitForControlPlaneMachinesReady = func(ctx context.Context, input ApplyCustomClusterTemplateAndWaitInput, result *ApplyCustomClusterTemplateAndWaitResult) {
			WaitForControlPlaneAndMachinesReady(ctx, WaitForControlPlaneAndMachinesReadyInput{
				GetLister:    input.ClusterProxy.GetClient(),
				Cluster:      result.Cluster,
				ControlPlane: result.ControlPlane,
			}, input.WaitForControlPlaneIntervals...)
		}
	}
}

type WaitForControlPlaneMachinesToExistInput struct {
	Lister       framework.Lister
	Cluster      *clusterv1.Cluster
	ControlPlane *controlplanev1.RKE2ControlPlane //TODO: abstract by interface
}

func DiscoveryAndWaitForControlPlaneInitialized(ctx context.Context, input framework.DiscoveryAndWaitForControlPlaneInitializedInput, intervals ...interface{}) *controlplanev1.RKE2ControlPlane { //TODO:Abstract by interface
	Expect(ctx).NotTo(BeNil(), "ctx is required for DiscoveryAndWaitForControlPlaneInitialized")
	Expect(input.Lister).ToNot(BeNil(), "Invalid argument. input.Lister can't be nil when calling DiscoveryAndWaitForControlPlaneInitialized")
	Expect(input.Cluster).ToNot(BeNil(), "Invalid argument. input.Cluster can't be nil when calling DiscoveryAndWaitForControlPlaneInitialized")

	var controlPlane *controlplanev1.RKE2ControlPlane //TODO: abstract by interface
	Eventually(func(g Gomega) {
		controlPlane = GetControlPlaneByCluster(ctx, GetControlPlaneByClusterInput{
			Lister:      input.Lister,
			ClusterName: input.Cluster.Name,
			Namespace:   input.Cluster.Namespace,
		})
		g.Expect(controlPlane).ToNot(BeNil())
	}, "10s", "1s").Should(Succeed(), "Couldn't get the control plane for the cluster %s", klog.KObj(input.Cluster))

	logInfo("Waiting for the first control plane machine managed by %s to be provisioned", klog.KObj(controlPlane))
	WaitForOneControlPlaneMachineToExist(ctx, WaitForControlPlaneMachinesToExistInput{
		Lister:       input.Lister,
		Cluster:      input.Cluster,
		ControlPlane: controlPlane,
	}, intervals...)

	return controlPlane
}

// WaitForOneControlPlaneMachineToExist will wait until all control plane machines have node refs.
func WaitForOneControlPlaneMachineToExist(ctx context.Context, input WaitForControlPlaneMachinesToExistInput, intervals ...interface{}) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for WaitForOneControlPlaneMachineToExist")
	Expect(input.Lister).ToNot(BeNil(), "Invalid argument. input.Getter can't be nil when calling WaitForOneControlPlaneMachineToExist")
	Expect(input.ControlPlane).ToNot(BeNil(), "Invalid argument. input.ControlPlane can't be nil when calling WaitForOneControlPlaneMachineToExist")

	By("Waiting for one control plane node to exist")
	inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
	// ControlPlane labels
	matchClusterListOption := client.MatchingLabels{
		clusterv1.MachineControlPlaneLabel: "",
		clusterv1.ClusterNameLabel:         input.Cluster.Name,
	}

	Eventually(func() (bool, error) {
		machineList := &clusterv1.MachineList{}
		if err := input.Lister.List(ctx, machineList, inClustersNamespaceListOption, matchClusterListOption); err != nil {
			logError(err, "Failed to list the machines")
			return false, err
		}
		count := 0
		for _, machine := range machineList.Items {
			if machine.Status.NodeRef != nil {
				count++
			}
		}
		return count > 0, nil
	}, intervals...).Should(BeTrue(), "No Control Plane machines came into existence. ")
}

type GetControlPlaneByClusterInput struct {
	Lister      framework.Lister
	ClusterName string
	Namespace   string
}

// GetControlPlaneByCluster returns the ControlPlane objects for a cluster.
// Important! this method relies on labels that are created by the CAPI controllers during the first reconciliation, so
// it is necessary to ensure this is already happened before calling it.
func GetControlPlaneByCluster(ctx context.Context, input GetControlPlaneByClusterInput) *controlplanev1.RKE2ControlPlane { //TODO: Abstract this
	controlPlaneList := &controlplanev1.RKE2ControlPlaneList{} //TODO: Abstract this
	Eventually(func() error {
		return input.Lister.List(ctx, controlPlaneList, byClusterOptions(input.ClusterName, input.Namespace)...)
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Failed to list ControlPlane object for Cluster %s", klog.KRef(input.Namespace, input.ClusterName))
	Expect(len(controlPlaneList.Items)).ToNot(BeNumerically(">", 1), "Cluster %s should not have more than 1 ControlPlane object", klog.KRef(input.Namespace, input.ClusterName))
	if len(controlPlaneList.Items) == 1 {
		return &controlPlaneList.Items[0]
	}
	return nil
}

// byClusterOptions returns a set of ListOptions that allows to identify all the objects belonging to a Cluster.
func byClusterOptions(name, namespace string) []client.ListOption {
	return []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel: name,
		},
	}
}

type WaitForControlPlaneAndMachinesReadyInput struct {
	GetLister    framework.GetLister
	Cluster      *clusterv1.Cluster
	ControlPlane *controlplanev1.RKE2ControlPlane //TODO: abstract by interface
}

// WaitForControlPlaneAndMachinesReady waits for a ControlPlane object to be ready (all the machine provisioned and one node ready).
func WaitForControlPlaneAndMachinesReady(ctx context.Context, input WaitForControlPlaneAndMachinesReadyInput, intervals ...interface{}) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for WaitForControlPlaneReady")
	Expect(input.GetLister).ToNot(BeNil(), "Invalid argument. input.GetLister can't be nil when calling WaitForControlPlaneReady")
	Expect(input.Cluster).ToNot(BeNil(), "Invalid argument. input.Cluster can't be nil when calling WaitForControlPlaneReady")
	Expect(input.ControlPlane).ToNot(BeNil(), "Invalid argument. input.ControlPlane can't be nil when calling WaitForControlPlaneReady")

	if input.ControlPlane.Spec.Replicas != nil && int(*input.ControlPlane.Spec.Replicas) > 1 {
		logInfo("Waiting for the remaining control plane machines managed by %s to be provisioned", klog.KObj(input.ControlPlane))
		WaitForControlPlaneMachinesToExist(ctx, WaitForControlPlaneMachinesToExistInput{
			Lister:       input.GetLister,
			Cluster:      input.Cluster,
			ControlPlane: input.ControlPlane,
		}, intervals...)
	}

	logInfo("Waiting for control plane %s to be ready (implies underlying nodes to be ready as well)", klog.KObj(input.ControlPlane))
	waitForControlPlaneToBeReadyInput := WaitForControlPlaneToBeReadyInput{
		Getter:       input.GetLister,
		ControlPlane: input.ControlPlane,
	}
	WaitForControlPlaneToBeReady(ctx, waitForControlPlaneToBeReadyInput, intervals...)

	framework.AssertControlPlaneFailureDomains(ctx, framework.AssertControlPlaneFailureDomainsInput{
		Lister:  input.GetLister,
		Cluster: input.Cluster,
	})
}

// WaitForControlPlaneMachinesToExist will wait until all control plane machines have node refs.
func WaitForControlPlaneMachinesToExist(ctx context.Context, input WaitForControlPlaneMachinesToExistInput, intervals ...interface{}) {
	By("Waiting for all control plane nodes to exist")
	inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
	// ControlPlane labels
	matchClusterListOption := client.MatchingLabels{
		clusterv1.MachineControlPlaneLabel: "",
		clusterv1.ClusterNameLabel:         input.Cluster.Name,
	}

	Eventually(func() (int, error) {
		machineList := &clusterv1.MachineList{}
		if err := input.Lister.List(ctx, machineList, inClustersNamespaceListOption, matchClusterListOption); err != nil {
			logError(err, "Failed to list the machines")
			return 0, err
		}
		count := 0
		for _, machine := range machineList.Items {
			if machine.Status.NodeRef != nil {
				count++
			}
		}
		return count, nil
	}, intervals...).Should(Equal(int(*input.ControlPlane.Spec.Replicas)), "Timed out waiting for %d control plane machines to exist", int(*input.ControlPlane.Spec.Replicas))
}

type WaitForControlPlaneToBeReadyInput struct {
	Getter       framework.Getter
	ControlPlane *controlplanev1.RKE2ControlPlane //TODO: abstract by interface
}

func WaitForControlPlaneToBeReady(ctx context.Context, input WaitForControlPlaneToBeReadyInput, intervals ...interface{}) {
	By("Waiting for the control plane to be ready")
	controlplane := &controlplanev1.RKE2ControlPlane{} //TODO: Abstract this
	Eventually(func() (bool, error) {
		key := client.ObjectKey{
			Namespace: input.ControlPlane.GetNamespace(),
			Name:      input.ControlPlane.GetName(),
		}
		if err := input.Getter.Get(ctx, key, controlplane); err != nil {
			return false, errors.Wrapf(err, "failed to get Control Plane") //TODO: Abstract this
		}

		desiredReplicas := controlplane.Spec.Replicas
		statusReplicas := controlplane.Status.Replicas
		updatedReplicas := controlplane.Status.UpdatedReplicas
		readyReplicas := controlplane.Status.ReadyReplicas
		unavailableReplicas := controlplane.Status.UnavailableReplicas

		// Control plane is still rolling out (and thus not ready) if:
		// * .spec.replicas, .status.replicas, .status.updatedReplicas,
		//   .status.readyReplicas are not equal and
		// * unavailableReplicas > 0
		if statusReplicas != *desiredReplicas ||
			updatedReplicas != *desiredReplicas ||
			readyReplicas != *desiredReplicas ||
			unavailableReplicas > 0 {
			return false, nil
		}

		return true, nil
	}, intervals...).Should(BeTrue(), framework.PrettyPrint(controlplane)+"\n")
}

func logInfo(format string, args ...any) {
	GinkgoLogr.Info(fmt.Sprintf(format, args...))
}

func logError(err error, format string, args ...any) {
	GinkgoLogr.Error(err, fmt.Sprintf(format, args...))
}
