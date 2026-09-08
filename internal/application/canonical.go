/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package application

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
)

func CanonicalPlan(spec applicationv1.OneKSApplicationSpec) ([]byte, error) {
	if spec.PlanVersion != applicationv1.PlanVersion {
		return nil, fmt.Errorf("unsupported planVersion %q", spec.PlanVersion)
	}
	return canonicalPlan(spec)
}

// The beta digest follows the API JSON fields. Absent and empty object fields
// are equivalent; array order, false and zero remain significant. Embedded
// manifests and Helm values are opaque strings, never parsed or normalized.
func canonicalPlan(spec applicationv1.OneKSApplicationSpec) ([]byte, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var plan map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&plan); err != nil {
		return nil, err
	}
	delete(plan, "planDigest")
	normalizeCanonicalFields(plan)
	var output bytes.Buffer
	if err := writeCanonicalJSON(&output, plan); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func normalizeCanonicalFields(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			normalizeCanonicalFields(item)
			switch field := item.(type) {
			case nil:
				delete(typed, key)
			case string:
				if field == "" {
					delete(typed, key)
				}
			case []any:
				if len(field) == 0 {
					delete(typed, key)
				}
			case map[string]any:
				if len(field) == 0 {
					delete(typed, key)
				}
			}
		}
	case []any:
		for _, item := range typed {
			normalizeCanonicalFields(item)
		}
	}
}

func Digest(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return "sha256-" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// encoding/json sorts map keys and preserves list order. The plan contract
// additionally requires ASCII strings, valid UTF-8 and integer-only numbers.
func writeCanonicalJSON(output *bytes.Buffer, value any) error {
	if err := validateCanonicalValue(value); err != nil {
		return err
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	for _, codepoint := range strings.TrimSuffix(encoded.String(), "\n") {
		if codepoint < 0x7f {
			output.WriteRune(codepoint)
		} else if codepoint <= 0xffff {
			fmt.Fprintf(output, `\u%04x`, codepoint)
		} else {
			high, low := utf16.EncodeRune(codepoint)
			fmt.Fprintf(output, `\u%04x\u%04x`, high, low)
		}
	}
	return nil
}

func validateCanonicalValue(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if !utf8.ValidString(key) {
				return fmt.Errorf("object key is not valid UTF-8")
			}
			if err := validateCanonicalValue(item); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := validateCanonicalValue(item); err != nil {
				return err
			}
		}
	case string:
		if !utf8.ValidString(typed) {
			return fmt.Errorf("string is not valid UTF-8")
		}
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
	case json.Number:
		integer, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil || strconv.FormatInt(integer, 10) != string(typed) {
			return fmt.Errorf("invalid canonical integer %q", typed)
		}
	default:
		return fmt.Errorf("unsupported canonical value type %T", value)
	}
	return nil
}
