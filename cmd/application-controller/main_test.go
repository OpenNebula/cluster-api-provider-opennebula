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
	"reflect"
	"testing"
	"time"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/application"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestControllerConfigValidation(t *testing.T) {
	valid := controllerConfig{
		clusterID: "42", controllerVersion: "test",
		reconciliationPoll: time.Second,
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*controllerConfig)
	}{
		{"cluster ID", func(config *controllerConfig) { config.clusterID = "" }},
		{"version", func(config *controllerConfig) { config.controllerVersion = "" }},
		{"poll", func(config *controllerConfig) { config.reconciliationPoll = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if err := config.validate(); err == nil {
				t.Fatal("expected invalid configuration")
			}
		})
	}
}

func TestControllerCacheMatchesNamespaceScopedRBAC(t *testing.T) {
	options := controllerCacheOptions()
	assertCacheNamespaces(t, options, &applicationv1.OneKSApplication{}, applicationv1.ApplicationNamespace)
	assertCacheNamespaces(t, options, &corev1.ConfigMap{}, application.WorkloadNamespace)
	assertCacheNamespaces(t, options, &batchv1.Job{}, application.HelmChartNamespace)

	foundHelm := false
	for object, byObject := range options.ByObject {
		unstructuredObject, ok := object.(*unstructured.Unstructured)
		if !ok || unstructuredObject.GetKind() != "HelmChart" {
			continue
		}
		foundHelm = true
		if len(byObject.Namespaces) != 1 {
			t.Fatalf("HelmChart cache namespaces: %#v", byObject.Namespaces)
		}
		if _, exists := byObject.Namespaces[application.HelmChartNamespace]; !exists {
			t.Fatalf("HelmChart cache is not scoped to %s", application.HelmChartNamespace)
		}
	}
	if !foundHelm {
		t.Fatal("HelmChart cache scope is missing")
	}
}

func assertCacheNamespaces(t *testing.T, options cache.Options, expected client.Object, namespace string) {
	t.Helper()
	for object, byObject := range options.ByObject {
		if reflect.TypeOf(object) != reflect.TypeOf(expected) {
			continue
		}
		if len(byObject.Namespaces) != 1 {
			t.Fatalf("%T cache namespaces: %#v", expected, byObject.Namespaces)
		}
		if _, exists := byObject.Namespaces[namespace]; !exists {
			t.Fatalf("%T cache is not scoped to %s", expected, namespace)
		}
		return
	}
	t.Fatalf("cache scope for %T is missing", expected)
}
