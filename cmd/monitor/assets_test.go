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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const monitorAuthMountPath = "/var/run/secrets/oneks-monitor"

func TestMonitorDeploymentProjectsOnlyAuthenticationSecretKey(t *testing.T) {
	var deployment appsv1.Deployment
	readMonitorYAML(t, "../../kustomize/v1beta1/monitor/deployment.yaml", &deployment)
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("monitor Deployment has %d containers", len(deployment.Spec.Template.Spec.Containers))
	}
	assertMonitorAuthProjection(t, deployment.Spec.Template.Spec.Containers[0],
		deployment.Spec.Template.Spec.Volumes, "cloud-config", "ONE_AUTH")
}

func TestMonitorHelmValuesAndOverlayConfigureAuthenticationProjection(t *testing.T) {
	values, err := os.ReadFile("../../helm/v1beta1/capone-monitor/values.yaml")
	if err != nil {
		t.Fatalf("read monitor Helm values: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(values, &parsed); err != nil {
		t.Fatalf("parse monitor Helm values: %v", err)
	}
	monitorValues, ok := parsed["monitor"].(map[string]any)
	if !ok {
		t.Fatalf("monitor Helm values are malformed: %#v", parsed["monitor"])
	}
	if monitorValues["authSecretName"] != "" || monitorValues["authSecretKey"] != "ONE_AUTH" {
		t.Fatalf("unexpected authentication Helm values: %#v", monitorValues)
	}

	overlay, err := os.ReadFile("../../kustomize/v1beta1/monitor-helm/kustomization.yaml")
	if err != nil {
		t.Fatalf("read monitor Helm overlay: %v", err)
	}
	text := string(overlay)
	for _, required := range []string{
		`required "monitor.authSecretName is required" .Values.monitor.authSecretName`,
		`required "monitor.authSecretKey is required" .Values.monitor.authSecretKey`,
		`/spec/template/spec/volumes/0/secret/secretName`,
		`/spec/template/spec/volumes/0/secret/items/0/key`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("monitor Helm overlay does not contain %q", required)
		}
	}
	if strings.Contains(text, "secretKeyRef") || strings.Contains(text, "name: MONITOR_AUTH\n") {
		t.Fatal("monitor Helm overlay injects authentication through an environment Secret reference")
	}
}

func TestGeneratedMonitorChartRendersSecretFileAuthentication(t *testing.T) {
	repositoryRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	helm := repositoryTool(t, repositoryRoot, "helm")
	kustomize := repositoryTool(t, repositoryRoot, "kustomize")
	chartsDirectory := t.TempDir()
	version := "9.8.7"

	generate := exec.Command(
		"make", "monitor-chart", "MONITOR_VERSION="+version,
		"CHARTS_DIR="+chartsDirectory, "HELM="+helm, "KUSTOMIZE="+kustomize,
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
	rbacObjects := 0
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
	if rbacObjects == 0 {
		t.Fatal("generated monitor chart did not render RBAC objects")
	}
}

func TestMonitorOwnedSecretContainsOnlyEncryptionKey(t *testing.T) {
	var secret corev1.Secret
	readMonitorYAML(t, "../../kustomize/v1beta1/monitor/secret.yaml", &secret)
	if len(secret.Data) != 0 || len(secret.StringData) != 1 || secret.StringData["MONITOR_KEY"] == "" {
		t.Fatalf("monitor-owned Secret must contain only MONITOR_KEY: %#v", secret.StringData)
	}
}

func TestMonitorRBACDoesNotGrantSecretAPIAccess(t *testing.T) {
	paths, err := filepath.Glob("../../kustomize/v1beta1/monitor/*role*.yaml")
	if err != nil {
		t.Fatalf("list monitor RBAC sources: %v", err)
	}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(contents), `"secrets"`) || strings.Contains(string(contents), "- secrets") {
			t.Fatalf("monitor RBAC source %s grants Secret API access", path)
		}
	}
}

func readMonitorYAML(t *testing.T, path string, object any) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(contents, object); err != nil {
		t.Fatalf("parse %s: %v", path, err)
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
			if resource == "secrets" || resource == "*" {
				t.Fatalf("rendered RBAC %s grants Secret API access: %#v", name, rule)
			}
		}
	}
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
