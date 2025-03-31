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
	"flag"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	ctrl "sigs.k8s.io/controller-runtime"

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
	ctx              = ctrl.SetupSignalHandler()
	_, cancelWatches = context.WithCancel(ctx)

	e2eConfig            *clusterctl.E2EConfig
	clusterctlConfigPath string

	bootstrapClusterProvider bootstrap.ClusterProvider
	bootstrapClusterProxy    framework.ClusterProxy
)

const (
	cniPathVarName = "CNI"
)

func init() {
	flag.StringVar(
		&configPath,
		"e2e.config",
		"./config/kubeadm.yaml",
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

	Expect(os.MkdirAll(artifactFolder, 0755)).To(Succeed(), "Failed to create artifact folder %s", artifactFolder)

	// E2E config

	configPath, _ = filepath.Abs(configPath)

	input1 := clusterctl.LoadE2EConfigInput{
		ConfigPath: configPath,
	}

	Expect(input1.ConfigPath).To(BeAnExistingFile(), "E2E config must be existing file %s", input1.ConfigPath)

	e2eConfig = clusterctl.LoadE2EConfig(ctx, input1)

	Expect(e2eConfig).ToNot(BeNil(), "Failed to load E2E config")

	// Clusterctl repository

	input2 := clusterctl.CreateRepositoryInput{
		E2EConfig:        e2eConfig,
		RepositoryFolder: filepath.Join(artifactFolder, "repository"),
	}

	Expect(os.MkdirAll(input2.RepositoryFolder, 0755)).To(Succeed(), "Failed to create repository folder %s", input2.RepositoryFolder)

	cniPath := e2eConfig.GetVariable(cniPathVarName)
	Expect(cniPath).To(BeAnExistingFile(), "\"%s\" variable must point to an existing file %s", cniPathVarName, cniPath)
	input2.RegisterClusterResourceSetConfigMapTransformation(cniPath, "CNI_RESOURCES")

	clusterctlConfigPath = clusterctl.CreateRepository(ctx, input2)

	Expect(clusterctlConfigPath).To(BeAnExistingFile(), "Clusterctl config must be existing file %s", clusterctlConfigPath)

	// Management cluster

	input3 := bootstrap.CreateKindBootstrapClusterAndLoadImagesInput{
		Name:              e2eConfig.ManagementClusterName,
		KubernetesVersion: e2eConfig.GetVariable("KUBERNETES_VERSION"),
		Images:            e2eConfig.Images,
		LogFolder:         filepath.Join(artifactFolder, "kind"),
	}

	Expect(os.MkdirAll(input3.LogFolder, 0755)).To(Succeed(), "Failed to create log folder %s", input3.LogFolder)

	bootstrapClusterProvider = bootstrap.CreateKindBootstrapClusterAndLoadImages(ctx, input3)

	Expect(bootstrapClusterProvider).ToNot(BeNil(), "Failed to create cluster provider")

	kubeconfigPath := bootstrapClusterProvider.GetKubeconfigPath()

	Expect(kubeconfigPath).To(BeAnExistingFile(), "Failed to get kubeconfig %s", kubeconfigPath)

	scheme := runtime.NewScheme()
	framework.TryAddDefaultSchemes(scheme)

	bootstrapClusterProxy = framework.NewClusterProxy("bootstrap", kubeconfigPath, scheme)

	Expect(bootstrapClusterProxy).ToNot(BeNil(), "Failed to get cluster proxy")

	// Cluster-API controllers

	input4 := clusterctl.InitManagementClusterAndWatchControllerLogsInput{
		ClusterProxy:              bootstrapClusterProxy,
		ClusterctlConfigPath:      clusterctlConfigPath,
		InfrastructureProviders:   e2eConfig.InfrastructureProviders(),
		IPAMProviders:             e2eConfig.IPAMProviders(),
		RuntimeExtensionProviders: e2eConfig.RuntimeExtensionProviders(),
		AddonProviders:            e2eConfig.AddonProviders(),
		LogFolder:                 filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName()),
	}

	Expect(os.MkdirAll(input4.LogFolder, 0755)).To(Succeed(), "Failed to create log folder %s", input4.LogFolder)

	clusterctl.InitManagementClusterAndWatchControllerLogs(
		ctx,
		input4,
		e2eConfig.GetIntervals(bootstrapClusterProxy.GetName(), "wait-controllers")...,
	)
}, func() {})

var _ = SynchronizedAfterSuite(func() {}, func() {
	if !skipCleanup {
		helpers.WaitForVRsToBeDeleted(
			ctx,
			"quick-start-[^-]+-cp",
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

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	ctrl.SetLogger(GinkgoLogr)
	RunSpecs(t, "capone-e2e")
}
