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
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	maxManagedManifestBytes = 131072
	maxReadinessEntries     = 16
	maxReadinessTimeout     = 86400
)

var kubernetesKindPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

func validateManagedResources(resources []applicationv1.ManagedResourceSpec) *PlanError {
	if len(resources) > maxManagedResources {
		return invalid("TooManyManagedResources", "managedResources exceeds %d entries", maxManagedResources)
	}
	ids := make(map[string]struct{}, len(resources))
	identities := make(map[string]struct{}, len(resources))
	for index, resource := range resources {
		path := fmt.Sprintf("managedResources[%d]", index)
		if !validUTF8Bytes(resource.ID, 1, 63) || len(validation.IsValidLabelValue(resource.ID)) != 0 {
			return invalid("InvalidManagedResourceID", "%s.id must be a Kubernetes label value", path)
		}
		if _, found := ids[resource.ID]; found {
			return invalid("DuplicateManagedResourceID", "managed resource id %q is duplicated", resource.ID)
		}
		ids[resource.ID] = struct{}{}
		if resource.Scope != applicationv1.ManagedResourceScopeNamespaced && resource.Scope != applicationv1.ManagedResourceScopeCluster {
			return invalid("InvalidManagedResourceScope", "%s.scope must be namespaced or cluster", path)
		}
		if !validGroupVersionKind(resource.APIVersion, resource.Kind) {
			return invalid("InvalidManagedResourceGVK", "%s apiVersion/kind is invalid", path)
		}
		if resource.Kind == "Secret" {
			return invalid("UnsupportedManagedSecret", "%s may not manage Secret objects", path)
		}
		if resource.APIResource != "" && (!validUTF8Bytes(resource.APIResource, 1, 128) || len(validation.IsDNS1123Subdomain(resource.APIResource)) != 0) {
			return invalid("InvalidManagedAPIResource", "%s.apiResource is invalid", path)
		}
		if !validUTF8Bytes(resource.Name, 1, 253) || len(validation.IsDNS1123Subdomain(resource.Name)) != 0 {
			return invalid("InvalidManagedResourceName", "%s.name is not a DNS-1123 subdomain", path)
		}
		switch resource.Scope {
		case applicationv1.ManagedResourceScopeNamespaced:
			if !validNamespace(resource.Namespace) {
				return invalid("InvalidManagedResourceNamespace", "%s.namespace must be a DNS-1123 label", path)
			}
		case applicationv1.ManagedResourceScopeCluster:
			if resource.Namespace != "" {
				return invalid("InvalidManagedResourceNamespace", "%s.namespace must be empty for cluster scope", path)
			}
		}
		identity := resource.APIVersion + "\x00" + resource.Kind + "\x00" + resource.Namespace + "\x00" + resource.Name
		if _, found := identities[identity]; found {
			return invalid("DuplicateManagedResourceIdentity", "%s duplicates Kubernetes identity %s %s/%s", path, resource.Kind, resource.Namespace, resource.Name)
		}
		identities[identity] = struct{}{}
		if !utf8.ValidString(resource.ManifestJSON) || len([]byte(resource.ManifestJSON)) < 2 || len([]byte(resource.ManifestJSON)) > maxManagedManifestBytes {
			return invalid("InvalidManagedManifest", "%s.manifestJSON is invalid or exceeds %d bytes", path, maxManagedManifestBytes)
		}
		if placeholderPattern.MatchString(resource.ManifestJSON) {
			return invalid("UnresolvedPlaceholder", "%s.manifestJSON contains an unresolved placeholder", path)
		}
		object, err := parseManagedResource(resource)
		if err != nil {
			return invalid("InvalidManagedManifest", "%s.manifestJSON: %v", path, err)
		}
		if object.GetAPIVersion() != resource.APIVersion || object.GetKind() != resource.Kind || object.GetName() != resource.Name {
			return invalid("ManagedResourceIdentityMismatch", "%s manifest apiVersion, kind, and metadata.name must match the outer identity", path)
		}
		if object.GetNamespace() != resource.Namespace {
			return invalid("ManagedResourceIdentityMismatch", "%s manifest metadata.namespace must match the outer scope and namespace", path)
		}
		if _, found := object.Object["status"]; found {
			return invalid("UnsupportedManagedManifestField", "%s manifest must not contain top-level status", path)
		}
		metadataMap, found, metadataErr := unstructured.NestedMap(object.Object, "metadata")
		if metadataErr != nil || !found {
			return invalid("InvalidManagedManifest", "%s manifest metadata must be an object", path)
		}
		allowedMetadata := map[string]struct{}{
			"name": {}, "namespace": {}, "labels": {}, "annotations": {},
		}
		for key := range metadataMap {
			if _, allowed := allowedMetadata[key]; !allowed {
				return invalid("UnsupportedManagedManifestField", "%s manifest metadata.%s is not permitted", path, key)
			}
		}
		manifestLabels, found, labelErr := unstructured.NestedStringMap(object.Object, "metadata", "labels")
		if labelErr != nil {
			return invalid("InvalidManagedManifest", "%s manifest metadata.labels must contain string values", path)
		}
		if !found {
			manifestLabels = nil
		}
		for key := range manifestLabels {
			if reservedOwnershipLabel(key) {
				return invalid("ReservedOwnershipLabel", "%s manifest metadata.labels contains reserved ownership label %q", path, key)
			}
		}
		if _, _, annotationErr := unstructured.NestedStringMap(object.Object, "metadata", "annotations"); annotationErr != nil {
			return invalid("InvalidManagedManifest", "%s manifest metadata.annotations must contain string values", path)
		}
		if !validDeletionPolicy(resource.DeletionPolicy) {
			return invalid("InvalidDeletionPolicy", "%s.deletionPolicy must be Delete or Retain", path)
		}
		if err := validateManagedReadiness(resource.Readiness, path+".readiness"); err != nil {
			return err
		}
		seenDependencies := make(map[string]struct{}, len(resource.DependsOn))
		if len(resource.DependsOn) > maxManagedResources {
			return invalid("TooManyManagedDependencies", "%s.dependsOn exceeds %d entries", path, maxManagedResources)
		}
		for _, dependency := range resource.DependsOn {
			if !validUTF8Bytes(dependency, 1, 63) || len(validation.IsValidLabelValue(dependency)) != 0 {
				return invalid("InvalidManagedDependency", "%s.dependsOn contains an invalid resource id", path)
			}
			if dependency == resource.ID {
				return invalid("ManagedResourceSelfDependency", "%s must not depend on itself", path)
			}
			if _, duplicate := seenDependencies[dependency]; duplicate {
				return invalid("DuplicateManagedDependency", "%s.dependsOn contains duplicate id %q", path, dependency)
			}
			seenDependencies[dependency] = struct{}{}
		}
	}
	_, planError := managedResourceOrder(resources)
	return planError
}

func validateManagedReadiness(readiness applicationv1.ManagedResourceReadiness, path string) *PlanError {
	if readiness.TimeoutSeconds < 1 || readiness.TimeoutSeconds > maxReadinessTimeout {
		return invalid("InvalidReadinessTimeout", "%s.timeoutSeconds must be between 1 and %d", path, maxReadinessTimeout)
	}
	if len(readiness.Conditions) > maxReadinessEntries || len(readiness.RequiredResources) > maxReadinessEntries || len(readiness.Checks) > maxReadinessEntries {
		return invalid("TooManyReadinessRules", "%s arrays may contain at most %d entries", path, maxReadinessEntries)
	}
	conditionTypes := make(map[string]struct{}, len(readiness.Conditions))
	for index, condition := range readiness.Conditions {
		if !validUTF8Bytes(condition.Type, 1, 128) || condition.Status != "True" && condition.Status != "False" && condition.Status != "Unknown" {
			return invalid("InvalidReadinessCondition", "%s.conditions[%d] is invalid", path, index)
		}
		if _, duplicate := conditionTypes[condition.Type]; duplicate {
			return invalid("DuplicateReadinessCondition", "%s condition type %q is duplicated", path, condition.Type)
		}
		conditionTypes[condition.Type] = struct{}{}
	}
	requiredIdentities := make(map[string]struct{}, len(readiness.RequiredResources))
	for index, reference := range readiness.RequiredResources {
		itemPath := fmt.Sprintf("%s.requiredResources[%d]", path, index)
		if !validGroupVersionKind(reference.APIVersion, reference.Kind) || !validObjectReference(reference.APIResource, reference.Namespace, reference.Name) {
			return invalid("InvalidRequiredResource", "%s is invalid", itemPath)
		}
		identity := reference.APIVersion + "\x00" + reference.Kind + "\x00" + reference.Namespace + "\x00" + reference.Name
		if _, duplicate := requiredIdentities[identity]; duplicate {
			return invalid("DuplicateRequiredResource", "%s duplicates an earlier required resource", itemPath)
		}
		requiredIdentities[identity] = struct{}{}
	}
	for index, check := range readiness.Checks {
		if check.Type != applicationv1.ManagedResourceCheckDNSMatchesService || !validUTF8Bytes(check.Hostname, 1, 253) || len(validation.IsDNS1123Subdomain(check.Hostname)) != 0 || !validNamespace(check.Service.Namespace) || !validUTF8Bytes(check.Service.Name, 1, 253) || len(validation.IsDNS1123Subdomain(check.Service.Name)) != 0 {
			return invalid("InvalidReadinessCheck", "%s.checks[%d] must be a valid DNSMatchesService check", path, index)
		}
	}
	return nil
}

func validGroupVersionKind(apiVersion, kind string) bool {
	if !validUTF8Bytes(apiVersion, 1, 128) || !validUTF8Bytes(kind, 1, 128) || !kubernetesKindPattern.MatchString(kind) {
		return false
	}
	_, err := schema.ParseGroupVersion(apiVersion)
	return err == nil
}

func validObjectReference(apiResource, namespace, name string) bool {
	if apiResource != "" && (!validUTF8Bytes(apiResource, 1, 128) || len(validation.IsDNS1123Subdomain(apiResource)) != 0) {
		return false
	}
	if namespace != "" && !validNamespace(namespace) {
		return false
	}
	return validUTF8Bytes(name, 1, 253) && len(validation.IsDNS1123Subdomain(name)) == 0
}

func validNamespace(namespace string) bool {
	return validUTF8Bytes(namespace, 1, 63) && len(validation.IsDNS1123Label(namespace)) == 0
}

func parseManagedResource(resource applicationv1.ManagedResourceSpec) (*unstructured.Unstructured, error) {
	var raw any
	if err := json.Unmarshal([]byte(resource.ManifestJSON), &raw); err != nil {
		return nil, err
	}
	objectMap, ok := raw.(map[string]any)
	if !ok || objectMap == nil {
		return nil, fmt.Errorf("must contain exactly one JSON object")
	}
	return &unstructured.Unstructured{Object: objectMap}, nil
}

func reservedOwnershipLabel(key string) bool {
	switch key {
	case LabelApplicationName, LabelApplicationNamespace, LabelApplicationUID, LabelClusterID, LabelPlanDigest, LabelManagedBy:
		return true
	default:
		return false
	}
}

// managedResourceOrder returns stable declaration-order topological ordering.
func managedResourceOrder(resources []applicationv1.ManagedResourceSpec) ([]int, *PlanError) {
	indices := make(map[string]int, len(resources))
	for index, resource := range resources {
		indices[resource.ID] = index
	}
	const (
		visiting = 1
		visited  = 2
	)
	states := make([]uint8, len(resources))
	order := make([]int, 0, len(resources))
	var visit func(int) *PlanError
	visit = func(index int) *PlanError {
		if states[index] == visiting {
			return invalid("ManagedResourceCycle", "managed resource graph contains a cycle through %q", resources[index].ID)
		}
		if states[index] == visited {
			return nil
		}
		states[index] = visiting
		for _, dependency := range resources[index].DependsOn {
			dependencyIndex, found := indices[dependency]
			if !found {
				return invalid("UnknownManagedDependency", "managed resource %q depends on unknown id %q", resources[index].ID, dependency)
			}
			if err := visit(dependencyIndex); err != nil {
				return err
			}
		}
		states[index] = visited
		order = append(order, index)
		return nil
	}
	for index := range resources {
		if err := visit(index); err != nil {
			return nil, err
		}
	}
	return order, nil
}
