/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package readiness

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

type Condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type Check struct {
	Type       string      `json:"type,omitempty"`
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Resource   string      `json:"resource,omitempty"`
	Namespace  string      `json:"namespace,omitempty"`
	Name       string      `json:"name"`
	Conditions []Condition `json:"conditions,omitempty"`
	Hostname   string      `json:"hostname,omitempty"`
	Service    *ObjectRef  `json:"service,omitempty"`
}

type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Resource   string `json:"resource,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

type Config struct {
	TimeoutSeconds      int     `json:"timeoutSeconds"`
	PollIntervalSeconds int     `json:"pollIntervalSeconds,omitempty"`
	Checks              []Check `json:"checks"`
}

func Load(path string) (Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read readiness configuration: %w", err)
	}
	var config Config
	if err := json.Unmarshal(body, &config); err != nil {
		return Config{}, fmt.Errorf("decode readiness configuration: %w", err)
	}
	if config.TimeoutSeconds <= 0 {
		return Config{}, fmt.Errorf("timeoutSeconds must be positive")
	}
	if config.PollIntervalSeconds == 0 {
		config.PollIntervalSeconds = 2
	}
	if config.PollIntervalSeconds < 0 {
		return Config{}, fmt.Errorf("pollIntervalSeconds must be positive")
	}
	if len(config.Checks) == 0 {
		return Config{}, fmt.Errorf("at least one readiness check is required")
	}
	for index, check := range config.Checks {
		switch check.Type {
		case "", "KubernetesObject":
			if err := validateObjectRef(ObjectRef{
				APIVersion: check.APIVersion, Kind: check.Kind,
				Resource: check.Resource, Namespace: check.Namespace,
				Name: check.Name,
			}); err != nil {
				return Config{}, fmt.Errorf("check %d: %w", index, err)
			}
		case "DNSMatchesService":
			if strings.TrimSpace(check.Hostname) == "" {
				return Config{}, fmt.Errorf("check %d requires hostname", index)
			}
			if check.Service == nil {
				return Config{}, fmt.Errorf("check %d requires service", index)
			}
			if err := validateObjectRef(*check.Service); err != nil {
				return Config{}, fmt.Errorf("check %d service: %w", index, err)
			}
			if check.Service.Kind != "Service" {
				return Config{}, fmt.Errorf("check %d service kind must be Service", index)
			}
		default:
			return Config{}, fmt.Errorf("check %d has unsupported type %q", index, check.Type)
		}
	}
	return config, nil
}

func validateObjectRef(ref ObjectRef) error {
	if strings.TrimSpace(ref.APIVersion) == "" ||
		strings.TrimSpace(ref.Kind) == "" ||
		strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("requires apiVersion, kind and name")
	}
	return nil
}

var lookupHost = net.DefaultResolver.LookupHost

func evaluate(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, checks []Check) (bool, string) {
	for _, check := range checks {
		switch check.Type {
		case "", "KubernetesObject":
			ready, message := evaluateObject(ctx, client, mapper, check)
			if !ready {
				return false, message
			}
		case "DNSMatchesService":
			ready, message := evaluateDNS(ctx, client, mapper, check)
			if !ready {
				return false, message
			}
		default:
			return false, fmt.Sprintf("unsupported check type %q", check.Type)
		}
	}
	return true, ""
}

func Run(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, config Config) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Duration(config.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()

	var lastPending string
	for {
		ready, pending := evaluate(ctx, client, mapper, config.Checks)
		if ready {
			return nil
		}
		lastPending = pending
		select {
		case <-ctx.Done():
			if lastPending == "" {
				lastPending = "readiness checks did not complete"
			}
			return fmt.Errorf("readiness timeout: %s", lastPending)
		case <-ticker.C:
		}
	}
}

func evaluateObject(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, check Check) (bool, string) {
	gvr, err := resourceFor(mapper, ObjectRef{
		APIVersion: check.APIVersion, Kind: check.Kind,
		Resource: check.Resource, Namespace: check.Namespace, Name: check.Name,
	})
	if err != nil {
		return false, err.Error()
	}
	object, message := getObject(ctx, client, gvr, check.Namespace, check.Name, check.Kind)
	if object == nil {
		return false, message
	}
	if ready, message := conditionsReady(object, check.Conditions); !ready {
		return false, fmt.Sprintf("%s %s: %s", check.Kind, check.Name, message)
	}
	return true, ""
}

func evaluateDNS(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, check Check) (bool, string) {
	service := *check.Service
	gvr, err := resourceFor(mapper, service)
	if err != nil {
		return false, err.Error()
	}
	object, message := getObject(ctx, client, gvr, service.Namespace, service.Name, service.Kind)
	if object == nil {
		return false, message
	}
	clusterIPs, _, _ := unstructured.NestedStringSlice(object.Object, "spec", "clusterIPs")
	if len(clusterIPs) == 0 {
		if clusterIP, _, _ := unstructured.NestedString(object.Object, "spec", "clusterIP"); clusterIP != "" {
			clusterIPs = []string{clusterIP}
		}
	}
	clusterIPs = slices.DeleteFunc(clusterIPs, func(ip string) bool {
		return ip == "" || strings.EqualFold(ip, "None")
	})
	if len(clusterIPs) == 0 {
		return false, fmt.Sprintf("Service %s has no cluster IP", service.Name)
	}
	resolved, err := lookupHost(ctx, check.Hostname)
	if err != nil {
		return false, fmt.Sprintf("resolve %s: %v", check.Hostname, err)
	}
	for _, address := range resolved {
		if slices.Contains(clusterIPs, address) {
			return true, ""
		}
	}
	return false, fmt.Sprintf(
		"%s resolved to %s; expected Service %s IP %s",
		check.Hostname, strings.Join(resolved, ", "), service.Name,
		strings.Join(clusterIPs, ", "),
	)
}

func getObject(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, namespace, name, kind string) (*unstructured.Unstructured, string) {
	resource := client.Resource(gvr)
	var object *unstructured.Unstructured
	var err error
	if namespace == "" {
		object, err = resource.Get(ctx, name, metav1.GetOptions{})
	} else {
		object, err = resource.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err) {
		return nil, fmt.Sprintf("%s %s is absent", kind, name)
	}
	if err != nil {
		return nil, fmt.Sprintf("get %s %s: %v", kind, name, err)
	}
	return object, ""
}

func resourceFor(mapper meta.RESTMapper, ref ObjectRef) (schema.GroupVersionResource, error) {
	groupVersion, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("invalid apiVersion %q: %w", ref.APIVersion, err)
	}
	if ref.Resource != "" {
		return groupVersion.WithResource(ref.Resource), nil
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: groupVersion.Group, Kind: ref.Kind}, groupVersion.Version)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("resolve %s %s: %w", ref.APIVersion, ref.Kind, err)
	}
	return mapping.Resource, nil
}

func conditionsReady(object *unstructured.Unstructured, required []Condition) (bool, string) {
	if len(required) == 0 {
		return true, ""
	}
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, expected := range required {
		matched := false
		for _, raw := range conditions {
			condition, ok := raw.(map[string]any)
			if !ok || condition["type"] != expected.Type {
				continue
			}
			matched = fmt.Sprint(condition["status"]) == expected.Status
			if !matched {
				message := strings.TrimSpace(fmt.Sprint(condition["message"]))
				if message != "" && message != "<nil>" {
					return false, message
				}
			}
			break
		}
		if !matched {
			return false, fmt.Sprintf("condition %s is not %s", expected.Type, expected.Status)
		}
	}
	return true, ""
}
