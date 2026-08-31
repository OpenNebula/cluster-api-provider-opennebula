/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.
Licensed under the Apache License, Version 2.0 (the "License");
*/

package resourceobserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	MaxExtractedString   = 512
	MaxResourceValueSize = 32 * 1024
)

type ResourceValue struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	APIVersion string `json:"apiVersion"`
	Resource   string `json:"resource"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Value      any    `json:"value"`
	ObservedAt string `json:"observedAt"`
}

func (value ResourceValue) CallbackKind() string { return value.Kind }

func NewResourceValue(spec ResourceSpec, value any, now time.Time) (ResourceValue, error) {
	report := ResourceValue{
		Kind: "ResourceValue", ID: spec.ID, APIVersion: spec.APIVersion,
		Resource: spec.Resource, Namespace: spec.Namespace, Name: spec.Name,
		Path: spec.Path, Value: value, ObservedAt: now.UTC().Format(time.RFC3339),
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return ResourceValue{}, fmt.Errorf("encode resource value: %w", err)
	}
	if len(encoded) > MaxResourceValueSize {
		return ResourceValue{}, fmt.Errorf("resource value exceeds %d bytes", MaxResourceValueSize)
	}
	return report, nil
}

func (value ResourceValue) Identity() string {
	parts := []string{value.ID, value.APIVersion, value.Resource, value.Namespace, value.Name, value.Path}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "ResourceValue/" + hex.EncodeToString(digest[:])
}

func ExtractScalar(object *unstructured.Unstructured, path string) (any, error) {
	value, found, err := unstructured.NestedFieldNoCopy(object.Object, strings.Split(path, ".")...)
	if err != nil {
		return nil, fmt.Errorf("extract path %q: invalid object shape", path)
	}
	if !found {
		return nil, nil
	}
	switch typed := value.(type) {
	case nil, bool, int64:
		return typed, nil
	case string:
		if len(typed) > MaxExtractedString {
			return nil, fmt.Errorf("string exceeds %d bytes", MaxExtractedString)
		}
		return typed, nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nil, fmt.Errorf("number is not finite")
		}
		return typed, nil
	default:
		return nil, fmt.Errorf("value is not scalar")
	}
}
