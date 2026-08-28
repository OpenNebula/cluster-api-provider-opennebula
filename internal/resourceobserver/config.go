/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package resourceobserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	ConfigDataKey        = "monitor.yaml"
	MaxConfigBytes       = 64 * 1024
	MaxResources         = 128
	MaxFieldPathDepth    = 16
	MaxValueKeyBytes     = 64
	MaxExtractedString   = 512
	MaxResourceValueSize = 32 * 1024
)

var safeKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

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

func ParseConfig(document []byte) (Config, string, error) {
	if len(document) > MaxConfigBytes {
		return nil, "", fmt.Errorf("configuration exceeds %d bytes", MaxConfigBytes)
	}
	var root yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(document))
	decoder.KnownFields(true)
	if err := decoder.Decode(&root); err != nil {
		return nil, "", fmt.Errorf("decode configuration: %w", err)
	}
	if len(root.Content) == 0 {
		return nil, "", fmt.Errorf("configuration is empty")
	}
	if root.Content[0].Kind != yaml.SequenceNode {
		return nil, "", fmt.Errorf("configuration must be a YAML resource list")
	}
	if err := validateYAMLTree(root.Content[0]); err != nil {
		return nil, "", err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, "", fmt.Errorf("multiple YAML documents are not allowed")
		}
		return nil, "", fmt.Errorf("decode trailing YAML: %w", err)
	}
	var config Config
	strictDecoder := yaml.NewDecoder(bytes.NewReader(document))
	strictDecoder.KnownFields(true)
	if err := strictDecoder.Decode(&config); err != nil {
		return nil, "", fmt.Errorf("decode configuration: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize configuration: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return config, "sha256-" + hex.EncodeToString(sum[:]), nil
}

func validateYAMLTree(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" {
		return fmt.Errorf("YAML aliases and anchors are not allowed")
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]struct{}{}
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if _, exists := seen[key.Value]; exists {
				return fmt.Errorf("duplicate YAML field %q", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := validateYAMLTree(child); err != nil {
			return err
		}
	}
	return nil
}

func (config Config) Validate() error {
	if len(config) > MaxResources {
		return fmt.Errorf("resource list exceeds %d entries", MaxResources)
	}
	ids := make(map[string]struct{}, len(config))
	for index, spec := range config {
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("resource %d: %w", index, err)
		}
		if _, exists := ids[spec.ID]; exists {
			return fmt.Errorf("resource %d: duplicate id %q", index, spec.ID)
		}
		ids[spec.ID] = struct{}{}
	}
	return nil
}

func (spec ResourceSpec) Validate() error {
	if !safeKey.MatchString(spec.ID) || len(spec.ID) > MaxValueKeyBytes {
		return fmt.Errorf("invalid id")
	}
	if _, err := spec.GVR(); err != nil {
		return err
	}
	if spec.Namespace != "" {
		if errs := validation.IsDNS1123Label(spec.Namespace); len(errs) != 0 {
			return fmt.Errorf("invalid namespace")
		}
	}
	if errs := validation.IsDNS1123Subdomain(spec.Name); len(errs) != 0 {
		return fmt.Errorf("invalid name")
	}
	parts := strings.Split(spec.Path, ".")
	if len(parts) == 0 || len(parts) > MaxFieldPathDepth {
		return fmt.Errorf("path must contain between 1 and %d segments", MaxFieldPathDepth)
	}
	for _, part := range parts {
		if !safeKey.MatchString(part) {
			return fmt.Errorf("path contains an invalid segment")
		}
	}
	return nil
}

func (spec ResourceSpec) GVR() (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(spec.APIVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("invalid apiVersion")
	}
	gvr := gv.WithResource(spec.Resource)
	if gvr.Group != "" {
		if errs := validation.IsDNS1123Subdomain(gvr.Group); len(errs) != 0 {
			return schema.GroupVersionResource{}, fmt.Errorf("invalid API group")
		}
	}
	if errs := validation.IsDNS1035Label(gvr.Version); len(errs) != 0 {
		return schema.GroupVersionResource{}, fmt.Errorf("invalid API version")
	}
	if errs := validation.IsDNS1035Label(gvr.Resource); len(errs) != 0 {
		return schema.GroupVersionResource{}, fmt.Errorf("invalid resource")
	}
	return gvr, nil
}

func ConfigMapIdentity(namespace, name string) string {
	return namespace + "/" + name
}
