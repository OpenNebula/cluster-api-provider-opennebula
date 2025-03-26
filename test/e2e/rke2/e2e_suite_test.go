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

package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	bootstrapv1 "github.com/rancher/cluster-api-provider-rke2/bootstrap/api/v1beta1"
	controlplanev1alpha1 "github.com/rancher/cluster-api-provider-rke2/controlplane/api/v1alpha1"
	controlplanev1 "github.com/rancher/cluster-api-provider-rke2/controlplane/api/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterv1exp "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"

	"github.com/OpenNebula/cluster-api-provider-opennebula/test/helpers"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	configPath     string
	artifactFolder string
	skipCleanup    bool
)

var (
	ctx = ctrl.SetupSignalHandler()

	_, cancelWatches = context.WithCancel(ctx)

	e2eConfig *clusterctl.E2EConfig

	clusterctlConfigPath string

	bootstrapClusterProvider bootstrap.ClusterProvider

	bootstrapClusterProxy framework.ClusterProxy
)

func init() {
	flag.StringVar(
		&configPath,
		"e2e.config",
		"./config/rke2.yaml",
		"Path to the e2e config file",
	)

	flag.StringVar(
		&artifactFolder,
		"e2e.artifacts-folder",
		"../../../_artifacts",
		"Folder where e2e test artifact should be stored",
	)

	flag.BoolVar(
		&skipCleanup,
		"e2e.skip-resource-cleanup",
		false,
		"If true, the resource cleanup after tests will be skipped",
	)
}

var _ = SynchronizedBeforeSuite(func() {
	artifactFolder, _ = filepath.Abs(artifactFolder)

	By(fmt.Sprintf("Creating artifact folder in %s ...", artifactFolder))
	Expect(os.MkdirAll(artifactFolder, 0755)).To(Succeed(), "Failed to create artifact folder %s", artifactFolder)

	By(fmt.Sprintf("Loading e2e config from %s", configPath))
	loadE2EConfig()

	repoLocalPath := filepath.Join(artifactFolder, "repository")
	By(fmt.Sprintf("Creating clusterctl local repository into %s", repoLocalPath))
	createClusterctlRepository(repoLocalPath)

	By("Creating bootstrap cluster")
	createBootstrapCluster()

	By("Initializing the bootstrap cluster")
	initBootstrapCluster()

}, func() {})

var _ = SynchronizedAfterSuite(func() {}, func() {
	if !skipCleanup {
		By("Deleting VRs created for the test...")
		helpers.WaitForVRsToBeDeleted(
			ctx,
			"quick-start-[^-]+-cp", //TODO: check name
			e2eConfig,
			24, // retries
			5,  // seconds
		)

		cancelWatches()

		if bootstrapClusterProxy != nil {
			bootstrapClusterProxy.Dispose(ctx)
		}

		if bootstrapClusterProvider != nil {
			bootstrapClusterProvider.Dispose(ctx)
		}
	}
})

func loadE2EConfig() {
	configPath, _ = filepath.Abs(configPath)

	// TODO: This is commented out as it assumes kubeadm and errors if its not there
	// Remove localLoadE2EConfig and use the line below when this issue is resolved:
	// https://github.com/kubernetes-sigs/cluster-api/issues/3983
	// Reference from the RKE2 repo: https://github.com/rancher/cluster-api-provider-rke2/blob/b4a8a0f37182082a70ad99cbd53fa283b63a2b54/test/e2e/e2e_suite_test.go#L185-L194
	/*e2eConfigInput := clusterctl.LoadE2EConfigInput{
		ConfigPath: configPath,
	}
	Expect(e2eConfigInput.ConfigPath).To(BeAnExistingFile(), "E2E config must be existing file %s", e2eConfigInput.ConfigPath)

	e2eConfig = clusterctl.LoadE2EConfig(ctx, e2eConfigInput)
	*/

	e2eConfig = overrideLoadE2EConfig(configPath)
	Expect(e2eConfig).ToNot(BeNil(), "Failed to load E2E config")
}

// reference: https://github.com/kubernetes-sigs/cluster-api/blob/a052e08156159db33bdb312298fbe897771e6cc8/test/framework/clusterctl/e2e_config.go#L56-L72
func overrideLoadE2EConfig(configPath string) *clusterctl.E2EConfig {
	configData, err := os.ReadFile(configPath)
	Expect(err).ToNot(HaveOccurred(), "Failed to read the e2e test config file")
	Expect(configData).ToNot(BeEmpty(), "The e2e test config file should not be empty")

	config := &clusterctl.E2EConfig{}
	Expect(yaml.Unmarshal(configData, config)).To(Succeed(), "Failed to convert the e2e test config file to yaml")

	Expect(config.ResolveReleases(ctx)).To(Succeed(), "Failed to resolve release markers in e2e test config file")
	config.Defaults()
	config.AbsPaths(filepath.Dir(configPath))

	// TODO: Commented validation for avoiding "invalid argument: invalid config: bootstrap-provider should be named kubeadm" error
	//Expect(config.Validate()).To(Succeed(), "The e2e test config file is not valid")

	return config
}

func createClusterctlRepository(repoLocalPath string) {
	clusterctlRepositoryInput := clusterctl.CreateRepositoryInput{
		E2EConfig:        e2eConfig,
		RepositoryFolder: repoLocalPath,
	}
	Expect(os.MkdirAll(clusterctlRepositoryInput.RepositoryFolder, 0755)).To(Succeed(), "Failed to create repository folder %s", clusterctlRepositoryInput.RepositoryFolder)
	clusterctlConfigPath = clusterctl.CreateRepository(ctx, clusterctlRepositoryInput)

	Expect(clusterctlConfigPath).To(BeAnExistingFile(), "Clusterctl config must be existing file %s", clusterctlConfigPath)
}

func createBootstrapCluster() {
	scheme := initScheme()
	createBootstrapClusterProvider()
	kubeconfigPath := getManagementClusterKubeconfigPath()
	createBootstrapClusterProxy(kubeconfigPath, scheme)
}

func createBootstrapClusterProvider() {
	clusterProviderInput := bootstrap.CreateKindBootstrapClusterAndLoadImagesInput{
		Name:              e2eConfig.ManagementClusterName,
		KubernetesVersion: e2eConfig.GetVariable("KUBERNETES_VERSION"),
		Images:            e2eConfig.Images,
		LogFolder:         filepath.Join(artifactFolder, "kind"),
	}

	//TODO: This is probably not needed
	//By(fmt.Sprintf("Creating log folder for the Kind management cluster in %s", clusterProviderInput.LogFolder))
	//Expect(os.MkdirAll(clusterProviderInput.LogFolder, 0755)).To(Succeed(), "Failed to create log folder %s", clusterProviderInput.LogFolder)

	bootstrapClusterProvider = bootstrap.CreateKindBootstrapClusterAndLoadImages(ctx, clusterProviderInput)
	Expect(bootstrapClusterProvider).ToNot(BeNil(), "Failed to create cluster provider")
}

func getManagementClusterKubeconfigPath() string {
	kubeconfigPath := bootstrapClusterProvider.GetKubeconfigPath()
	Expect(kubeconfigPath).To(BeAnExistingFile(), "Failed to get kubeconfig %s", kubeconfigPath)
	return kubeconfigPath
}

func createBootstrapClusterProxy(kubeconfigPath string, scheme *runtime.Scheme) {
	bootstrapClusterProxy = framework.NewClusterProxy("bootstrap", kubeconfigPath, scheme)
	Expect(bootstrapClusterProxy).ToNot(BeNil(), "Failed to get cluster proxy")
}

func initScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	framework.TryAddDefaultSchemes(scheme)
	Expect(controlplanev1.AddToScheme(scheme)).To(Succeed())
	Expect(controlplanev1alpha1.AddToScheme(scheme)).To(Succeed())
	Expect(bootstrapv1.AddToScheme(scheme)).To(Succeed())
	Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	Expect(clusterv1exp.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func initBootstrapCluster() {

	bootstrapClusterInitInput := clusterctl.InitManagementClusterAndWatchControllerLogsInput{
		ClusterProxy:              bootstrapClusterProxy,
		ClusterctlConfigPath:      clusterctlConfigPath,
		InfrastructureProviders:   e2eConfig.InfrastructureProviders(),
		IPAMProviders:             e2eConfig.IPAMProviders(),
		RuntimeExtensionProviders: e2eConfig.RuntimeExtensionProviders(),
		BootstrapProviders:        []string{"rke2-bootstrap:v0.12.0"},
		ControlPlaneProviders:     []string{"rke2-control-plane:v0.12.0"},
		AddonProviders:            e2eConfig.AddonProviders(),
		LogFolder:                 filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName()),
	}

	//TODO: not sure if needed
	//Expect(os.MkdirAll(input4.LogFolder, 0755)).To(Succeed(), "Failed to create log folder %s", input4.LogFolder)

	clusterctl.InitManagementClusterAndWatchControllerLogs(
		ctx,
		bootstrapClusterInitInput,
		e2eConfig.GetIntervals(bootstrapClusterProxy.GetName(), "wait-controllers")...,
	)
}

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	ctrl.SetLogger(GinkgoLogr)
	RunSpecs(t, "capone-rke2-e2e")
}
