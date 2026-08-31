/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package resourceobserver

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const ConfigDataKey = "monitor.yaml"

// Config is the direct list stored in monitor.yaml.
type Config []ResourceSpec

type ResourceSpec struct {
	ID         string `yaml:"id" json:"id"`
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Resource   string `yaml:"resource" json:"resource"`
	Namespace  string `yaml:"namespace" json:"namespace"`
	Name       string `yaml:"name" json:"name"`
	Path       string `yaml:"path" json:"path"`
}

func ParseConfig(document []byte) (Config, error) {
	var config Config
	if err := yaml.Unmarshal(document, &config); err != nil {
		return nil, fmt.Errorf("decode configuration: %w", err)
	}
	for index, spec := range config {
		if err := spec.Validate(); err != nil {
			return nil, fmt.Errorf("resource %d: %w", index, err)
		}
	}
	return config, nil
}

func (spec ResourceSpec) Validate() error {
	for name, value := range map[string]string{
		"id": spec.ID, "apiVersion": spec.APIVersion, "resource": spec.Resource,
		"name": spec.Name, "path": spec.Path,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	_, err := spec.GVR()
	return err
}

func (spec ResourceSpec) GVR() (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(spec.APIVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("invalid apiVersion")
	}
	return gv.WithResource(spec.Resource), nil
}

func ConfigMapIdentity(namespace, name string) string {
	return namespace + "/" + name
}
