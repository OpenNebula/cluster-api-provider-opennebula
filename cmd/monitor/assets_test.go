/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

const monitorAuthMountPath = "/var/run/secrets/oneks-monitor"

func TestGeneratedMonitorChartContract(t *testing.T) {
	repositoryRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	helm := repositoryTool(t, repositoryRoot, "helm")
	chartsDirectory := t.TempDir()
	version := "9.8.7"

	generate := exec.Command(
		"make", "monitor-chart", "MONITOR_VERSION="+version,
		"CHARTS_DIR="+chartsDirectory, "HELM="+helm,
	)
	generate.Dir = repositoryRoot
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate monitor chart: %v\n%s", err, output)
	}
	chart := filepath.Join(chartsDirectory, "monitor-v"+version, "capone-monitor-"+version+".tgz")
	render := exec.Command(
		helm, "template", "capone-monitor", chart, "--namespace", "kube-system",
		"--set", "monitor.endpoint=http://oneks.example/api/v1",
		"--set-string", "monitor.clusterID=42",
		"--set-string", "monitor.key=non-sensitive-test-key",
		"--set", "monitor.authSecretName=cloud-config",
	)
	rendered, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render generated monitor chart: %v\n%s", err, rendered)
	}

	var deployment *appsv1.Deployment
	var monitorSecret *corev1.Secret
	var runtimeConfig *corev1.ConfigMap
	resourceConfigPresent := false
	rbacObjects := 0
	observedResources := map[string]bool{}
	viewBinding := false
	for _, object := range decodeManifestObjects(t, rendered) {
		switch object.GetKind() {
		case "Deployment":
			candidate := &appsv1.Deployment{}
			convertManifestObject(t, object, candidate)
			if candidate.Name == "capone-cluster-monitor" {
				deployment = candidate
			}
		case "Secret":
			candidate := &corev1.Secret{}
			convertManifestObject(t, object, candidate)
			assertNoRenderedAuthKeys(t, candidate.Name, candidate.Data, candidate.StringData)
			if candidate.Name == "capone-cluster-monitor" {
				monitorSecret = candidate
			}
		case "ConfigMap":
			candidate := &corev1.ConfigMap{}
			convertManifestObject(t, object, candidate)
			if candidate.Name == "capone-resource-monitor" {
				_, resourceConfigPresent = candidate.Data["monitor.yaml"]
			}
			if candidate.Name == "capone-cluster-monitor" {
				runtimeConfig = candidate
			}
			for key := range candidate.Data {
				if key == "MONITOR_AUTH" || key == "ONE_AUTH" {
					t.Fatalf("rendered ConfigMap %s contains authentication key %s", candidate.Name, key)
				}
			}
			for key := range candidate.BinaryData {
				if key == "MONITOR_AUTH" || key == "ONE_AUTH" {
					t.Fatalf("rendered ConfigMap %s contains authentication key %s", candidate.Name, key)
				}
			}
		case "Role":
			candidate := &rbacv1.Role{}
			convertManifestObject(t, object, candidate)
			assertNoSecretRules(t, candidate.Name, candidate.Rules)
			rbacObjects++
		case "ClusterRole":
			candidate := &rbacv1.ClusterRole{}
			convertManifestObject(t, object, candidate)
			assertNoSecretRules(t, candidate.Name, candidate.Rules)
			if candidate.Name == "capone-cluster-monitor" {
				for _, rule := range candidate.Rules {
					for _, resource := range rule.Resources {
						observedResources[resource] = true
					}
				}
			}
			rbacObjects++
		case "ClusterRoleBinding":
			candidate := &rbacv1.ClusterRoleBinding{}
			convertManifestObject(t, object, candidate)
			if candidate.Name == "capone-cluster-monitor-view" && candidate.RoleRef.Name == "view" {
				viewBinding = true
			}
			rbacObjects++
		}
	}
	if deployment == nil || len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("rendered monitor Deployment is missing or malformed: %#v", deployment)
	}
	assertMonitorAuthProjection(t, deployment.Spec.Template.Spec.Containers[0],
		deployment.Spec.Template.Spec.Volumes, "cloud-config", "ONE_AUTH")
	if monitorSecret == nil || len(monitorSecret.Data) != 0 || len(monitorSecret.StringData) != 1 ||
		monitorSecret.StringData["MONITOR_KEY"] == "" {
		t.Fatalf("rendered monitor-owned Secret must contain only MONITOR_KEY: %#v", monitorSecret)
	}
	if runtimeConfig == nil {
		t.Fatal("generated monitor chart did not render its runtime ConfigMap")
	}
	expectedConfig := map[string]string{
		"MONITOR_ENDPOINT":                  "http://oneks.example/api/v1",
		"MONITOR_CLUSTER_ID":                "42",
		"MONITOR_APPLICATION_NAMESPACE":     "oneks-system",
		"MONITOR_HTTP_TIMEOUT":              "10s",
		"MONITOR_HEALTH_ADDRESS":            ":8081",
		"MONITOR_RESOURCE_CONFIG_NAMESPACE": "kube-system",
		"MONITOR_RESOURCE_CONFIG_NAME":      "capone-resource-monitor",
		"MONITOR_RESOURCE_POLL_INTERVAL":    "10s",
	}
	if len(runtimeConfig.Data) != len(expectedConfig) {
		t.Fatalf("unexpected runtime configuration: %#v", runtimeConfig.Data)
	}
	for key, expected := range expectedConfig {
		if actual := runtimeConfig.Data[key]; actual != expected {
			t.Fatalf("runtime configuration %s = %q, want %q", key, actual, expected)
		}
	}
	if !resourceConfigPresent {
		t.Fatal("generated monitor chart did not render the administrator-editable resource ConfigMap")
	}
	if rbacObjects == 0 {
		t.Fatal("generated monitor chart did not render RBAC objects")
	}
	if !observedResources["nodes"] || !observedResources["pods"] || !viewBinding {
		t.Fatalf("generated monitor RBAC is missing view, Nodes or Pods access: resources=%#v view=%t", observedResources, viewBinding)
	}
}

func assertMonitorAuthProjection(
	t *testing.T,
	container corev1.Container,
	volumes []corev1.Volume,
	secretName, secretKey string,
) {
	t.Helper()
	var authEnv *corev1.EnvVar
	for index := range container.Env {
		env := &container.Env[index]
		if env.Name == "MONITOR_AUTH" {
			t.Fatal("legacy MONITOR_AUTH environment injection remains")
		}
		if env.Name == "MONITOR_AUTH_FILE" {
			authEnv = env
		}
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil &&
			(env.ValueFrom.SecretKeyRef.Name == secretName || env.ValueFrom.SecretKeyRef.Key == secretKey) {
			t.Fatalf("authentication credential is injected through environment variable %q", env.Name)
		}
	}
	for _, source := range container.EnvFrom {
		if source.SecretRef != nil && source.SecretRef.Name == secretName {
			t.Fatalf("authentication Secret %q is injected through envFrom", secretName)
		}
	}
	if authEnv == nil || authEnv.Value != monitorAuthMountPath+"/ONE_AUTH" || authEnv.ValueFrom != nil {
		t.Fatalf("unexpected monitor authentication file environment: %#v", authEnv)
	}

	var authMount *corev1.VolumeMount
	for index := range container.VolumeMounts {
		if container.VolumeMounts[index].Name == "monitor-auth" {
			authMount = &container.VolumeMounts[index]
			break
		}
	}
	if authMount == nil || authMount.MountPath != monitorAuthMountPath || !authMount.ReadOnly {
		t.Fatalf("unexpected monitor authentication mount: %#v", authMount)
	}

	var authVolume *corev1.Volume
	for index := range volumes {
		if volumes[index].Name == "monitor-auth" {
			authVolume = &volumes[index]
			break
		}
	}
	if authVolume == nil || authVolume.Secret == nil || authVolume.Secret.SecretName != secretName ||
		len(authVolume.Secret.Items) != 1 || authVolume.Secret.Items[0].Key != secretKey ||
		authVolume.Secret.Items[0].Path != "ONE_AUTH" {
		t.Fatalf("authentication volume must project only %s/%s: %#v", secretName, secretKey, authVolume)
	}
}

func repositoryTool(t *testing.T, repositoryRoot, name string) string {
	t.Helper()
	path := filepath.Join(repositoryRoot, "bin", name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		if os.Getenv("CI") != "" {
			t.Fatalf("repository %s tool is unavailable in CI; install it with make %s", name, name)
		}
		t.Skipf("repository %s tool is unavailable; install it with make %s", name, name)
	}
	return path
}

func decodeManifestObjects(t *testing.T, payload []byte) []unstructured.Unstructured {
	t.Helper()
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(payload), 4096)
	objects := make([]unstructured.Unstructured, 0)
	for {
		object := unstructured.Unstructured{}
		err := decoder.Decode(&object)
		if err == io.EOF {
			return objects
		}
		if err != nil {
			t.Fatalf("decode rendered monitor manifest: %v", err)
		}
		if len(object.Object) != 0 {
			objects = append(objects, object)
		}
	}
}

func convertManifestObject(t *testing.T, object unstructured.Unstructured, target any) {
	t.Helper()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, target); err != nil {
		t.Fatalf("convert rendered %s %s: %v", object.GetKind(), object.GetName(), err)
	}
}

func assertNoSecretRules(t *testing.T, name string, rules []rbacv1.PolicyRule) {
	t.Helper()
	for _, rule := range rules {
		for _, resource := range rule.Resources {
			if resource == "secrets" || resource == "*" && grantsCoreAPI(rule.APIGroups) {
				t.Fatalf("rendered RBAC %s grants Secret API access: %#v", name, rule)
			}
		}
	}
}

func grantsCoreAPI(groups []string) bool {
	for _, group := range groups {
		if group == "" || group == "*" {
			return true
		}
	}
	return false
}

func assertNoRenderedAuthKeys(t *testing.T, name string, data map[string][]byte, stringData map[string]string) {
	t.Helper()
	for key := range data {
		if key == "MONITOR_AUTH" || key == "ONE_AUTH" {
			t.Fatalf("rendered Secret %s contains authentication key %s", name, key)
		}
	}
	for key := range stringData {
		if key == "MONITOR_AUTH" || key == "ONE_AUTH" {
			t.Fatalf("rendered Secret %s contains authentication key %s", name, key)
		}
	}
}
