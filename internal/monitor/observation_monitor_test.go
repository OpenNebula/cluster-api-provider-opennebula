/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestObservationPollIncludesPendingPodsAndConfiguredResources(t *testing.T) {
	createdAt := time.Date(2026, 9, 10, 11, 12, 13, 0, time.UTC)
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "capone-resource-monitor", Namespace: "kube-system"},
			Data:       map[string]string{resourceConfigDataKey: validResourceConfig},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "queued", Namespace: "payments", CreationTimestamp: metav1.NewTime(createdAt)},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "payments"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	deployment := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "api", "namespace": "payments", "creationTimestamp": createdAt.Format(time.RFC3339),
		},
		"status": map[string]any{"readyReplicas": int64(2)},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), deployment)

	var snapshot ObservationSnapshot
	monitor := newObservationMonitor(client, dynamicClient, senderFunc(
		func(_ context.Context, destination Destination, payload any) error {
			if destination.path() != "/clusters/42/observations" {
				t.Fatalf("unexpected destination: %s", destination.path())
			}
			value, ok := payload.(ObservationSnapshot)
			if !ok {
				t.Fatalf("unexpected payload type: %T", payload)
			}
			snapshot = value
			return nil
		},
	), Config{
		ClusterID:               "42",
		ResourceConfigNamespace: "kube-system",
		ResourceConfigName:      "capone-resource-monitor", ResourcePollInterval: time.Second,
	})
	if err := monitor.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !monitor.Ready() || len(snapshot) != 2 {
		t.Fatalf("ready=%t snapshot=%#v", monitor.Ready(), snapshot)
	}
	pending := snapshot[0]
	if pending.Resource != "pods" || pending.Namespace != "payments" || pending.Name != "queued" ||
		pending.Path != "status.phase" || pending.Value != "Pending" ||
		pending.CreatedAt != "2026-09-10T11:12:13Z" {
		t.Fatalf("unexpected Pending Pod observation: %#v", pending)
	}
	configured := snapshot[1]
	if configured.Resource != "deployments" || configured.Name != "api" || configured.Value != int64(2) ||
		configured.CreatedAt != "2026-09-10T11:12:13Z" {
		t.Fatalf("unexpected configured observation: %#v", configured)
	}
	encoded, err := json.Marshal(ObservationSnapshot{pending})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"resource":"pods","namespace":"payments","name":"queued","path":"status.phase","value":"Pending","createdAt":"2026-09-10T11:12:13Z"}]`
	if string(encoded) != want {
		t.Fatalf("observation payload = %s, want %s", encoded, want)
	}
}

func TestObservationPollRetriesOnlyOnNextPoll(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "queued", Namespace: "default"},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		},
	)
	attempts := 0
	monitor := newObservationMonitor(
		client,
		dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		senderFunc(func(context.Context, Destination, any) error {
			attempts++
			return errors.New("endpoint unavailable")
		}),
		Config{
			ClusterID:               "42",
			ResourceConfigNamespace: "kube-system",
			ResourceConfigName:      "missing", ResourcePollInterval: time.Second,
		},
	)
	if err := monitor.poll(context.Background()); err == nil || attempts != 1 {
		t.Fatalf("first poll: attempts=%d err=%v", attempts, err)
	}
	if err := monitor.poll(context.Background()); err == nil || attempts != 2 {
		t.Fatalf("second poll: attempts=%d err=%v", attempts, err)
	}
}

func TestObservationPollSendsEmptySnapshot(t *testing.T) {
	client := fake.NewSimpleClientset()
	attempts := 0
	monitor := newObservationMonitor(
		client,
		dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		senderFunc(func(_ context.Context, _ Destination, payload any) error {
			attempts++
			snapshot, ok := payload.(ObservationSnapshot)
			if !ok || len(snapshot) != 0 {
				t.Fatalf("unexpected empty snapshot: %#v", payload)
			}
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != `[]` {
				t.Fatalf("empty observation payload = %s, want []", encoded)
			}
			return nil
		}),
		Config{
			ClusterID:               "42",
			ResourceConfigNamespace: "kube-system",
			ResourceConfigName:      "missing", ResourcePollInterval: time.Second,
		},
	)
	if err := monitor.poll(context.Background()); err != nil || attempts != 1 {
		t.Fatalf("empty snapshot: attempts=%d err=%v", attempts, err)
	}
}

func TestObservationMonitorReloadsResourceConfig(t *testing.T) {
	ctx := context.Background()
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "capone-resource-monitor", Namespace: "kube-system"},
		Data:       map[string]string{resourceConfigDataKey: "[]"},
	}
	client := fake.NewSimpleClientset(configMap)
	monitor := newObservationMonitor(
		client,
		dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		senderFunc(func(context.Context, Destination, any) error { return nil }),
		Config{
			ClusterID:               "42",
			ResourceConfigNamespace: "kube-system",
			ResourceConfigName:      configMap.Name, ResourcePollInterval: time.Second,
		},
	)
	if err := monitor.refreshConfig(ctx); err != nil || len(monitor.active) != 0 {
		t.Fatalf("load empty configuration: active=%d err=%v", len(monitor.active), err)
	}

	configMap.Data[resourceConfigDataKey] = validResourceConfig
	if _, err := client.CoreV1().ConfigMaps(configMap.Namespace).Update(ctx, configMap, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.refreshConfig(ctx); err != nil || len(monitor.active) != 1 {
		t.Fatalf("reload configuration: active=%d err=%v", len(monitor.active), err)
	}

	configMap.Data[resourceConfigDataKey] = "invalid"
	if _, err := client.CoreV1().ConfigMaps(configMap.Namespace).Update(ctx, configMap, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.refreshConfig(ctx); err == nil || len(monitor.active) != 1 {
		t.Fatalf("invalid update replaced active configuration: active=%d err=%v", len(monitor.active), err)
	}

	if err := client.CoreV1().ConfigMaps(configMap.Namespace).Delete(ctx, configMap.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.refreshConfig(ctx); err != nil || len(monitor.active) != 0 {
		t.Fatalf("deleted configuration remained active: active=%d err=%v", len(monitor.active), err)
	}
}
