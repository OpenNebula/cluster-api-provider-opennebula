/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.
Licensed under the Apache License, Version 2.0 (the "License");
*/

package resourceobserver

import (
	"fmt"
	"math"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func ExtractValue(object *unstructured.Unstructured, path string) (any, error) {
	value, found, err := unstructured.NestedFieldNoCopy(object.Object, strings.Split(path, ".")...)
	if err != nil {
		return nil, fmt.Errorf("extract path %q: invalid object shape", path)
	}
	if !found {
		return nil, nil
	}
	return sanitizeScalar(value)
}

func sanitizeScalar(value any) (any, error) {
	switch typed := value.(type) {
	case nil, bool:
		return typed, nil
	case string:
		if len(typed) > MaxExtractedString {
			return nil, fmt.Errorf("string exceeds %d bytes", MaxExtractedString)
		}
		return typed, nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return typed, nil
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return nil, fmt.Errorf("number is not finite")
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
