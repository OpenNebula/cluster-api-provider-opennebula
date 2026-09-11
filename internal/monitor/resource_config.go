/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

const resourceConfigDataKey = "monitor.yaml"

type resourceConfig []resourceQuery

type ResourceSpec struct {
	APIVersion string `json:"apiVersion"`
	Resource   string `json:"resource"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Path       string `json:"path"`
}

type resourceQuery struct {
	ResourceSpec
	gvr  schema.GroupVersionResource
	path []string
}

func parseResourceConfig(document []byte) (resourceConfig, error) {
	var specs []ResourceSpec
	if err := yaml.Unmarshal(document, &specs); err != nil {
		return nil, fmt.Errorf("decode configuration: %w", err)
	}
	config := make(resourceConfig, len(specs))
	for i, spec := range specs {
		query, err := prepareResource(spec)
		if err != nil {
			return nil, fmt.Errorf("resource %d: %w", i, err)
		}
		config[i] = query
	}
	return config, nil
}

func prepareResource(spec ResourceSpec) (resourceQuery, error) {
	for field, value := range map[string]string{
		"apiVersion": spec.APIVersion, "resource": spec.Resource,
		"name": spec.Name, "path": spec.Path,
	} {
		if strings.TrimSpace(value) == "" {
			return resourceQuery{}, fmt.Errorf("%s is required", field)
		}
	}
	gv, err := schema.ParseGroupVersion(spec.APIVersion)
	if err != nil {
		return resourceQuery{}, fmt.Errorf("invalid apiVersion %q", spec.APIVersion)
	}
	return resourceQuery{
		ResourceSpec: spec,
		gvr:          gv.WithResource(spec.Resource),
		path:         strings.Split(spec.Path, "."),
	}, nil
}
