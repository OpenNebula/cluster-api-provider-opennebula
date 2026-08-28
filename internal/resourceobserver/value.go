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
	"strings"
	"time"
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
