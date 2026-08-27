/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

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

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/application"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var version = "development"

type controllerConfig struct {
	clusterID          string
	metricsAddress     string
	healthAddress      string
	leaderElection     bool
	controllerVersion  string
	reconciliationPoll time.Duration
}

func main() {
	config := controllerConfig{}
	flag.StringVar(&config.clusterID, "cluster-id", os.Getenv("CLUSTER_ID"), "OneKS cluster identifier served by this controller")
	flag.StringVar(&config.metricsAddress, "metrics-bind-address", "0", "Metrics bind address, or 0 to disable")
	flag.StringVar(&config.healthAddress, "health-probe-bind-address", ":8081", "Health probe bind address")
	flag.BoolVar(&config.leaderElection, "leader-elect", true, "Enable leader election")
	flag.StringVar(&config.controllerVersion, "controller-version", version, "Version reported in application status")
	flag.DurationVar(&config.reconciliationPoll, "reconciliation-poll", 15*time.Second, "Periodic authoritative status reconciliation interval")
	logging := zap.Options{Development: false}
	logging.BindFlags(flag.CommandLine)
	flag.Parse()

	if err := config.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logging)))
	if err := run(config); err != nil {
		ctrl.Log.WithName("setup").Error(err, "application controller stopped")
		os.Exit(1)
	}
}

func (config controllerConfig) validate() error {
	if config.clusterID == "" {
		return fmt.Errorf("--cluster-id is required")
	}
	if config.controllerVersion == "" {
		return fmt.Errorf("--controller-version is required")
	}
	if config.reconciliationPoll <= 0 {
		return fmt.Errorf("--reconciliation-poll must be positive")
	}
	return nil
}

func run(config controllerConfig) error {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(batchv1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(applicationv1.AddToScheme(scheme))

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: config.metricsAddress},
		HealthProbeBindAddress:  config.healthAddress,
		LeaderElection:          config.leaderElection,
		LeaderElectionID:        applicationv1.LeaderElectionID,
		LeaderElectionNamespace: applicationv1.ApplicationNamespace,
		Cache:                   controllerCacheOptions(),
		Client:                  controllerClientOptions(),
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	reconciler := &application.Reconciler{
		Client: manager.GetClient(), APIReader: manager.GetAPIReader(), Scheme: manager.GetScheme(),
		Recorder:  manager.GetEventRecorderFor(applicationv1.FieldManager),
		ClusterID: config.clusterID, ControllerVersion: config.controllerVersion,
		RequeueAfter: config.reconciliationPoll,
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("register application reconciler: %w", err)
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("register health check: %w", err)
	}
	if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("register readiness check: %w", err)
	}
	return manager.Start(ctrl.SetupSignalHandler())
}

func controllerClientOptions() client.Options {
	return client.Options{Cache: &client.CacheOptions{
		DisableFor:   []client.Object{&corev1.Namespace{}, &corev1.ConfigMap{}},
		Unstructured: true,
	}}
}

func controllerCacheOptions() cache.Options {
	helmChart := &unstructured.Unstructured{}
	helmChart.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "helm.cattle.io", Version: "v1", Kind: "HelmChart",
	})
	return cache.Options{
		DefaultNamespaces: map[string]cache.Config{
			applicationv1.ApplicationNamespace: {},
		},
		ByObject: map[client.Object]cache.ByObject{
			&applicationv1.OneKSApplication{}: {
				Namespaces: namespaceCache(applicationv1.ApplicationNamespace),
			},
			&corev1.ConfigMap{}: {
				Namespaces: map[string]cache.Config{},
				Label: labels.SelectorFromSet(labels.Set{
					application.LabelManagedBy: application.ManagedByValue,
				}),
			},
			helmChart: {
				Namespaces: namespaceCache(application.HelmChartNamespace),
			},
			&batchv1.Job{}: {
				Namespaces: namespaceCache(application.HelmChartNamespace),
			},
		},
	}
}

func namespaceCache(namespace string) map[string]cache.Config {
	return map[string]cache.Config{namespace: {}}
}
