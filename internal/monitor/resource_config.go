/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitor

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const ConfigDataKey = "monitor.yaml"

// resourceConfig is the direct list stored in monitor.yaml.
type resourceConfig []resourceQuery

// resourceQuery contains the immutable settings prepared when configuration changes.
type resourceQuery struct {
	ResourceSpec
	gvr      schema.GroupVersionResource
	path     []string
	identity string
}

type ResourceSpec struct {
	ID         string `yaml:"id" json:"id"`
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Resource   string `yaml:"resource" json:"resource"`
	Namespace  string `yaml:"namespace" json:"namespace"`
	Name       string `yaml:"name" json:"name"`
	Path       string `yaml:"path" json:"path"`
}

func parseResourceConfig(document []byte) (resourceConfig, error) {
	var specs []ResourceSpec
	if err := yaml.Unmarshal(document, &specs); err != nil {
		return nil, fmt.Errorf("decode configuration: %w", err)
	}
	config := make(resourceConfig, len(specs))
	for index, spec := range specs {
		query, err := prepareResource(spec)
		if err != nil {
			return nil, fmt.Errorf("resource %d: %w", index, err)
		}
		config[index] = query
	}
	return config, nil
}

func prepareResource(spec ResourceSpec) (resourceQuery, error) {
	for name, value := range map[string]string{
		"id": spec.ID, "apiVersion": spec.APIVersion, "resource": spec.Resource,
		"name": spec.Name, "path": spec.Path,
	} {
		if strings.TrimSpace(value) == "" {
			return resourceQuery{}, fmt.Errorf("%s is required", name)
		}
	}
	gv, err := schema.ParseGroupVersion(spec.APIVersion)
	if err != nil {
		return resourceQuery{}, fmt.Errorf("invalid apiVersion")
	}
	identity := ResourceValue{
		ID: spec.ID, APIVersion: spec.APIVersion, Resource: spec.Resource,
		Namespace: spec.Namespace, Name: spec.Name, Path: spec.Path,
	}
	return resourceQuery{
		ResourceSpec: spec, gvr: gv.WithResource(spec.Resource),
		path: strings.Split(spec.Path, "."), identity: identity.Identity(),
	}, nil
}
