/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.
Licensed under the Apache License, Version 2.0.
*/

package readiness

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/restmapper"
)

func TestEvaluateReadyCondition(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": "runai-tls", "namespace": "runai"},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), object)
	mapper := restmapper.NewDiscoveryRESTMapper([]*restmapper.APIGroupResources{{
		Group:              metav1.APIGroup{Name: "cert-manager.io", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "cert-manager.io/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "cert-manager.io/v1", Version: "v1"}},
		VersionedResources: map[string][]metav1.APIResource{"v1": {{Name: "certificates", Kind: "Certificate", Namespaced: true}}},
	}})
	ready, message := evaluate(context.Background(), client, mapper, []Check{{
		APIVersion: "cert-manager.io/v1", Kind: "Certificate", Namespace: "runai", Name: "runai-tls",
		Conditions: []Condition{{Type: "Ready", Status: "True"}},
	}})
	if !ready || message != "" {
		t.Fatalf("expected ready check, got ready=%v message=%q", ready, message)
	}
}

func TestEvaluateAbsentResource(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := restmapper.NewDiscoveryRESTMapper([]*restmapper.APIGroupResources{{
		Group:              metav1.APIGroup{Name: "batch", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "batch/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "batch/v1", Version: "v1"}},
		VersionedResources: map[string][]metav1.APIResource{"v1": {{Name: "jobs", Kind: "Job", Namespaced: true}}},
	}})
	ready, message := evaluate(context.Background(), client, mapper, []Check{{
		APIVersion: "batch/v1", Kind: "Job", Resource: "jobs", Namespace: "kube-system", Name: "dns-check",
	}})
	if ready || message != "Job dns-check is absent" {
		t.Fatalf("unexpected absent result: ready=%v message=%q", ready, message)
	}
}

func TestEvaluateDNSMatchesService(t *testing.T) {
	service := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{
			"name":      "oneks-haproxy-ingress",
			"namespace": "haproxy-controller",
		},
		"spec": map[string]any{
			"clusterIP":  "10.43.12.9",
			"clusterIPs": []any{"10.43.12.9"},
		},
	}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), service)
	mapper := restmapper.NewDiscoveryRESTMapper([]*restmapper.APIGroupResources{{
		Group: metav1.APIGroup{
			Versions: []metav1.GroupVersionForDiscovery{
				{GroupVersion: "v1", Version: "v1"},
			},
			PreferredVersion: metav1.GroupVersionForDiscovery{
				GroupVersion: "v1", Version: "v1",
			},
		},
		VersionedResources: map[string][]metav1.APIResource{
			"v1": {{Name: "services", Kind: "Service", Namespaced: true}},
		},
	}})
	previousLookup := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"10.43.12.9"}, nil
	}
	t.Cleanup(func() { lookupHost = previousLookup })

	ready, message := evaluate(context.Background(), client, mapper, []Check{{
		Type: "DNSMatchesService", Hostname: "runai.example.test",
		Service: &ObjectRef{
			APIVersion: "v1", Kind: "Service", Resource: "services",
			Namespace: "haproxy-controller", Name: "oneks-haproxy-ingress",
		},
	}})
	if !ready || message != "" {
		t.Fatalf("expected matching DNS result, got ready=%v message=%q", ready, message)
	}
}

func TestEvaluateDNSMismatch(t *testing.T) {
	service := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{
			"name": "ingress", "namespace": "ingress-system",
		},
		"spec": map[string]any{"clusterIP": "10.43.12.9"},
	}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), service)
	mapper := restmapper.NewDiscoveryRESTMapper([]*restmapper.APIGroupResources{{
		Group: metav1.APIGroup{
			Versions: []metav1.GroupVersionForDiscovery{
				{GroupVersion: "v1", Version: "v1"},
			},
			PreferredVersion: metav1.GroupVersionForDiscovery{
				GroupVersion: "v1", Version: "v1",
			},
		},
		VersionedResources: map[string][]metav1.APIResource{
			"v1": {{Name: "services", Kind: "Service", Namespaced: true}},
		},
	}})
	previousLookup := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"192.0.2.10"}, nil
	}
	t.Cleanup(func() { lookupHost = previousLookup })

	ready, message := evaluate(context.Background(), client, mapper, []Check{{
		Type: "DNSMatchesService", Hostname: "runai.example.test",
		Service: &ObjectRef{
			APIVersion: "v1", Kind: "Service", Resource: "services",
			Namespace: "ingress-system", Name: "ingress",
		},
	}})
	if ready || !strings.Contains(message, "expected Service ingress IP 10.43.12.9") {
		t.Fatalf("unexpected DNS mismatch result: ready=%v message=%q", ready, message)
	}
}
