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
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	maxValuesContentBytes  = 65536
	maxCanonicalPlanBytes  = 131072
	maxResources           = 16
	maxConfigMapEntries    = 128
	maxConfigMapKeyBytes   = 253
	maxConfigMapValueBytes = 32768
	maxConfigMapDataBytes  = 65536
	maxDependencies        = 16
	maxDependencyPlans     = 16
	WorkloadNamespace      = "oneks-poc-workloads"
)

var (
	placeholderPattern  = regexp.MustCompile(`\$\{[^}]+\}`)
	sensitiveKeyPattern = regexp.MustCompile(`(?i)(password|secret|token|credential|api.?key|private.?key)`)
	planDigestPattern   = regexp.MustCompile(`^sha256-[A-Za-z0-9_-]{43}$`)
	ociChartPattern     = regexp.MustCompile(`^oci://[^[:space:]]+$`)
)

type ValidationConfig struct {
	ClusterID string
}

type PlanError struct {
	Reason  string
	Message string
}

func (e *PlanError) Error() string { return e.Message }

func ValidatePlan(app *applicationv1.OneKSApplication, config ValidationConfig) *PlanError {
	return validatePlan(app, config, true)
}

func ValidateDeletionPlan(app *applicationv1.OneKSApplication) *PlanError {
	return validatePlan(app, ValidationConfig{ClusterID: app.Spec.ClusterID}, false)
}

func validatePlan(app *applicationv1.OneKSApplication, config ValidationConfig, validateProducer bool) *PlanError {
	if app.Namespace != applicationv1.ApplicationNamespace {
		return invalid("InvalidNamespace", "application namespace must be %q", applicationv1.ApplicationNamespace)
	}
	if !validUTF8Bytes(app.Name, 1, 63) || len(validation.IsValidLabelValue(app.Name)) != 0 {
		return invalid("InvalidApplicationName", "application name must be a Kubernetes label value of at most 63 characters")
	}
	if !validUTF8Bytes(string(app.UID), 1, 63) || len(validation.IsValidLabelValue(string(app.UID))) != 0 {
		return invalid("InvalidApplicationUID", "application UID must be a Kubernetes label value of at most 63 characters")
	}
	if len(app.OwnerReferences) != 0 {
		return invalid("InvalidOwnerReferences", "application ownerReferences are not permitted")
	}
	if !validApplicationFinalizers(app.Finalizers) {
		return invalid("InvalidFinalizers", "application finalizers may contain only the OneKS application finalizer")
	}
	if app.Spec.ExecutionMode == applicationv1.ExecutionModeObserve && len(app.Finalizers) != 0 {
		return invalid("InvalidFinalizers", "Observe applications must not have a cleanup finalizer")
	}
	if app.Spec.ClusterID == "" || app.Spec.ClusterID != config.ClusterID {
		return invalid("ClusterIDMismatch", "spec.clusterID does not match this controller")
	}
	if !validUTF8Bytes(app.Spec.ClusterID, 1, 63) || len(validation.IsValidLabelValue(app.Spec.ClusterID)) > 0 {
		return invalid("InvalidClusterID", "spec.clusterID is not a Kubernetes label value")
	}
	switch app.Spec.PlanVersion {
	case applicationv1.PlanVersionV1Alpha1:
		if app.Spec.Role != "" || app.Spec.Dependencies != nil || app.Spec.DependencyPlans != nil {
			return invalid("InvalidPlanV1Alpha1Fields", "plan-v1alpha1 does not permit role or dependency fields")
		}
	case applicationv1.PlanVersionV1Alpha2:
		if app.Spec.Role != applicationv1.ApplicationRoleRoot && app.Spec.Role != applicationv1.ApplicationRoleDependency {
			return invalid("InvalidApplicationRole", "plan-v1alpha2 role must be Root or Dependency")
		}
	default:
		return invalid("UnsupportedPlanVersion", "unsupported planVersion %q", app.Spec.PlanVersion)
	}
	if app.Spec.ExecutionMode != applicationv1.ExecutionModeObserve && app.Spec.ExecutionMode != applicationv1.ExecutionModeExecute {
		return invalid("InvalidExecutionMode", "executionMode must be Observe or Execute")
	}
	if !validDeletionPolicy(app.Spec.DeletionPolicy) {
		return invalid("InvalidDeletionPolicy", "deletionPolicy must be Delete or Retain")
	}
	if !validUTF8Bytes(app.Spec.CatalogueChartID, 1, 63) || len(validation.IsValidLabelValue(app.Spec.CatalogueChartID)) != 0 {
		return invalid("InvalidCatalogueChartID", "catalogueChartID must be a Kubernetes label value")
	}
	if !validUTF8Bytes(app.Spec.Release.ChartID, 1, 63) || len(validation.IsValidLabelValue(app.Spec.Release.ChartID)) != 0 || app.Spec.Release.ChartID != app.Spec.CatalogueChartID {
		return invalid("ChartIDMismatch", "release.chartID must equal catalogueChartID")
	}
	if !validUTF8Bytes(app.Spec.Release.Chart, 1, 1024) {
		return invalid("InvalidChart", "release chart must be valid UTF-8 between 1 and 1024 bytes")
	}
	if !validUTF8Bytes(app.Spec.Release.Version, 1, 253) {
		return invalid("InvalidChartVersion", "release version must be valid UTF-8 between 1 and 253 bytes")
	}
	if app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha1 {
		if err := validateRepositoryURL(app.Spec.Release.RepositoryURL); err != nil {
			return err
		}
	} else if err := validateReleaseSourceV1Alpha2(app.Spec.Release, "release"); err != nil {
		return err
	}
	if !validUTF8Bytes(app.Spec.Release.ReleaseName, 1, 63) || len(validation.IsDNS1123Label(app.Spec.Release.ReleaseName)) > 0 {
		return invalid("InvalidReleaseName", "releaseName is not a DNS-1123 label of at most 63 characters")
	}
	if app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha2 && app.Spec.Role == applicationv1.ApplicationRoleDependency {
		expectedName := dependencyApplicationName(app.Spec.Release.ReleaseName)
		if app.Name != expectedName {
			return invalid("InvalidDependencyApplicationName", "application name must be %q for releaseName %q", expectedName, app.Spec.Release.ReleaseName)
		}
	}
	if !validUTF8Bytes(app.Spec.Release.TargetNamespace, 1, 63) || len(validation.IsDNS1123Label(app.Spec.Release.TargetNamespace)) > 0 {
		return invalid("InvalidTargetNamespace", "targetNamespace is not a DNS-1123 label of at most 63 characters")
	}
	if app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha1 || app.Spec.Role == applicationv1.ApplicationRoleRoot {
		if app.Spec.Release.TargetNamespace != WorkloadNamespace {
			return invalid("InvalidTargetNamespace", "targetNamespace must be %q", WorkloadNamespace)
		}
		if app.Spec.Release.CreateNamespace {
			return invalid("InvalidCreateNamespace", "createNamespace must be false")
		}
	}
	if app.Spec.Role == applicationv1.ApplicationRoleDependency && app.Spec.Release.CreateNamespace && len(app.Spec.Resources) != 0 {
		return invalid("InvalidDependencyResources", "Dependency with createNamespace=true must not contain resources")
	}
	if !utf8.ValidString(app.Spec.Release.ValuesContent) {
		return invalid("InvalidValuesContent", "valuesContent must be valid UTF-8")
	}
	if len([]byte(app.Spec.Release.ValuesContent)) > maxValuesContentBytes {
		return invalid("ValuesContentTooLarge", "valuesContent exceeds %d bytes", maxValuesContentBytes)
	}
	if placeholderPattern.MatchString(app.Spec.Release.ValuesContent) {
		return invalid("UnresolvedPlaceholder", "valuesContent contains an unresolved placeholder")
	}
	if err := validateNonSensitiveValues(app.Spec.Release.ValuesContent); err != nil {
		return err
	}
	if len(app.Spec.Resources) > maxResources {
		return invalid("TooManyResources", "resources exceeds %d entries", maxResources)
	}

	resourceIDs := make(map[string]struct{}, len(app.Spec.Resources))
	resourceNames := make(map[string]struct{}, len(app.Spec.Resources))
	for index, resource := range app.Spec.Resources {
		if !validUTF8Bytes(resource.ID, 1, 63) || len(validation.IsValidLabelValue(resource.ID)) != 0 {
			return invalid("InvalidResourceID", "resources[%d].id must be a Kubernetes label value", index)
		}
		if _, exists := resourceIDs[resource.ID]; exists {
			return invalid("DuplicateResourceID", "resource id %q is duplicated", resource.ID)
		}
		resourceIDs[resource.ID] = struct{}{}
		if resource.APIVersion != "v1" || resource.Kind != "ConfigMap" {
			return invalid("UnsupportedResource", "resources[%d] must be a v1 ConfigMap", index)
		}
		if resource.Namespace != app.Spec.Release.TargetNamespace {
			return invalid("InvalidResourceNamespace", "resources[%d] must use %q", index, app.Spec.Release.TargetNamespace)
		}
		if !validUTF8Bytes(resource.Name, 1, 253) || len(validation.IsDNS1123Subdomain(resource.Name)) > 0 {
			return invalid("InvalidResourceName", "resources[%d].name is not a DNS-1123 subdomain", index)
		}
		if _, exists := resourceNames[resource.Name]; exists {
			return invalid("DuplicateResourceIdentity", "ConfigMap %q is duplicated", resource.Name)
		}
		resourceNames[resource.Name] = struct{}{}
		if !validDeletionPolicy(resource.DeletionPolicy) {
			return invalid("InvalidDeletionPolicy", "resources[%d].deletionPolicy must be Delete or Retain", index)
		}
		if len(resource.Data) > maxConfigMapEntries {
			return invalid("ConfigMapDataTooLarge", "resources[%d] data exceeds %d entries", index, maxConfigMapEntries)
		}
		dataBytes := 0
		for key, value := range resource.Data {
			if !utf8.ValidString(key) || len([]byte(key)) > maxConfigMapKeyBytes || len(validation.IsConfigMapKey(key)) > 0 {
				return invalid("InvalidConfigMapKey", "resources[%d] contains an invalid data key", index)
			}
			if !utf8.ValidString(value) || len([]byte(value)) > maxConfigMapValueBytes {
				return invalid("InvalidConfigMapValue", "resources[%d] contains an invalid or oversized data value", index)
			}
			if placeholderPattern.MatchString(key) || placeholderPattern.MatchString(value) {
				return invalid("UnresolvedPlaceholder", "resources[%d] data contains an unresolved placeholder", index)
			}
			if value != "" && sensitiveKeyPattern.MatchString(key) {
				return invalid("SensitiveConfigMapData", "resources[%d] data contains a sensitive key", index)
			}
			dataBytes += len([]byte(key)) + len([]byte(value))
		}
		if dataBytes > maxConfigMapDataBytes {
			return invalid("ConfigMapDataTooLarge", "resources[%d] data exceeds %d aggregate bytes", index, maxConfigMapDataBytes)
		}
	}

	if app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha2 {
		if err := validateDependencyContract(app); err != nil {
			return err
		}
	}

	canonical, err := CanonicalPlan(app.Spec)
	if err != nil {
		return invalid("InvalidCanonicalPlan", "canonical plan encoding failed: %v", err)
	}
	if len(canonical) > maxCanonicalPlanBytes {
		return invalid("PlanTooLarge", "canonical plan exceeds %d bytes", maxCanonicalPlanBytes)
	}
	expected := Digest(canonical)
	if app.Spec.PlanDigest != expected {
		return invalid("PlanDigestMismatch", "planDigest does not match the canonical application plan")
	}
	if validateProducer && !producerLabelsMatch(app) {
		return invalid("InvalidProducerLabels", "application producer identity labels are missing or do not match the plan")
	}
	return nil
}

func validateDependencyContract(app *applicationv1.OneKSApplication) *PlanError {
	if len(app.Spec.Dependencies) > maxDependencies {
		return invalid("TooManyDependencies", "dependencies exceeds %d entries", maxDependencies)
	}
	dependencyNames := make(map[string]struct{}, len(app.Spec.Dependencies))
	for index, dependency := range app.Spec.Dependencies {
		if err := validateDependencyReference(dependency, fmt.Sprintf("dependencies[%d]", index)); err != nil {
			return err
		}
		if _, exists := dependencyNames[dependency.Name]; exists {
			return invalid("DuplicateDependencyName", "dependency name %q is duplicated", dependency.Name)
		}
		dependencyNames[dependency.Name] = struct{}{}
		if app.Spec.Role == applicationv1.ApplicationRoleDependency && dependency.Name == app.Name {
			return invalid("SelfDependency", "Dependency application %q must not directly reference itself", app.Name)
		}
	}

	if len(app.Spec.DependencyPlans) > maxDependencyPlans {
		return invalid("TooManyDependencyPlans", "dependencyPlans exceeds %d entries", maxDependencyPlans)
	}
	if app.Spec.Role == applicationv1.ApplicationRoleDependency && len(app.Spec.DependencyPlans) != 0 {
		return invalid("InvalidDependencyPlans", "Dependency applications must not contain dependencyPlans")
	}

	planIndex := make(map[string]*applicationv1.DependencyPlan, len(app.Spec.DependencyPlans))
	for index := range app.Spec.DependencyPlans {
		plan := &app.Spec.DependencyPlans[index]
		path := fmt.Sprintf("dependencyPlans[%d]", index)
		identity := applicationv1.DependencyReference{
			Name: plan.Name, CatalogueChartID: plan.CatalogueChartID, PlanDigest: plan.PlanDigest,
		}
		if err := validateDependencyReference(identity, path); err != nil {
			return err
		}
		if _, exists := planIndex[plan.Name]; exists {
			return invalid("DuplicateDependencyPlanName", "dependency plan name %q is duplicated", plan.Name)
		}
		planIndex[plan.Name] = plan
		if err := validateDependencyPlan(*plan, path); err != nil {
			return err
		}
		if app.Spec.Role == applicationv1.ApplicationRoleRoot {
			expectedName := dependencyApplicationName(plan.Release.ReleaseName)
			if plan.Name != expectedName {
				return invalid("InvalidDependencyApplicationName", "%s.name must be %q for releaseName %q", path, expectedName, plan.Release.ReleaseName)
			}
		}
	}

	if app.Spec.Role == applicationv1.ApplicationRoleRoot {
		if err := validateRootDependencyGraph(app.Spec.Dependencies, app.Spec.DependencyPlans, planIndex); err != nil {
			return err
		}
		for index, plan := range app.Spec.DependencyPlans {
			if err := validateDependencyPlanDigest(app.Spec.ClusterID, plan, fmt.Sprintf("dependencyPlans[%d]", index)); err != nil {
				return err
			}
		}
	}
	return nil
}

// dependencyApplicationName maps the HelmChart collision domain to one stable
// OneKSApplication metadata.name. Only releaseName participates intentionally.
func dependencyApplicationName(releaseName string) string {
	prefix := releaseName
	if len(prefix) > 32 {
		prefix = prefix[:32]
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(releaseName)))
	return "oneks-dep-" + prefix + "-" + digest[:20]
}

func validateRootDependencyGraph(rootDependencies []applicationv1.DependencyReference, plans []applicationv1.DependencyPlan, planIndex map[string]*applicationv1.DependencyPlan) *PlanError {
	const (
		visiting = 1
		visited  = 2
	)
	states := make(map[string]uint8, len(planIndex))
	reachable := make(map[string]struct{}, len(planIndex))

	var visitReference func(applicationv1.DependencyReference, string) *PlanError
	var visitPlan func(*applicationv1.DependencyPlan) *PlanError
	visitReference = func(reference applicationv1.DependencyReference, path string) *PlanError {
		plan, exists := planIndex[reference.Name]
		if !exists {
			return invalid("UnresolvedDependency", "%s references missing dependencyPlan %q", path, reference.Name)
		}
		if plan.CatalogueChartID != reference.CatalogueChartID {
			return invalid("UnresolvedDependency", "%s catalogueChartID does not match dependencyPlan %q", path, reference.Name)
		}
		if plan.PlanDigest != reference.PlanDigest {
			return invalid("UnresolvedDependency", "%s planDigest does not match dependencyPlan %q", path, reference.Name)
		}
		return visitPlan(plan)
	}
	visitPlan = func(plan *applicationv1.DependencyPlan) *PlanError {
		switch states[plan.Name] {
		case visiting:
			return invalid("DependencyCycle", "dependency graph contains a cycle through %q", plan.Name)
		case visited:
			return nil
		}
		states[plan.Name] = visiting
		reachable[plan.Name] = struct{}{}
		for index, dependency := range plan.Dependencies {
			path := fmt.Sprintf("dependencyPlan %q dependencies[%d]", plan.Name, index)
			if err := visitReference(dependency, path); err != nil {
				return err
			}
		}
		states[plan.Name] = visited
		return nil
	}

	for index, dependency := range rootDependencies {
		if err := visitReference(dependency, fmt.Sprintf("dependencies[%d]", index)); err != nil {
			return err
		}
	}
	for _, plan := range plans {
		if _, exists := reachable[plan.Name]; !exists {
			return invalid("OrphanDependencyPlan", "dependencyPlan %q is not reachable from root dependencies", plan.Name)
		}
	}
	return nil
}

func validateDependencyPlanDigest(clusterID string, plan applicationv1.DependencyPlan, path string) *PlanError {
	child := dependencyPlanChildSpec(clusterID, plan)
	canonical, err := canonicalPlanV1Alpha2(child)
	if err != nil {
		return invalid("InvalidDependencyPlanDigest", "%s child plan cannot be canonicalized: %v", path, err)
	}
	if expected := Digest(canonical); plan.PlanDigest != expected {
		return invalid("InvalidDependencyPlanDigest", "%s.planDigest does not match its canonical child OneKSApplicationSpec", path)
	}
	return nil
}

func dependencyPlanChildSpec(clusterID string, plan applicationv1.DependencyPlan) applicationv1.OneKSApplicationSpec {
	return applicationv1.OneKSApplicationSpec{
		ClusterID: clusterID, CatalogueChartID: plan.CatalogueChartID,
		PlanVersion:   applicationv1.PlanVersionV1Alpha2,
		ExecutionMode: applicationv1.ExecutionModeExecute,
		Release:       plan.Release, Resources: plan.Resources,
		Role: applicationv1.ApplicationRoleDependency, Dependencies: plan.Dependencies,
		DependencyPlans: nil, DeletionPolicy: plan.DeletionPolicy,
	}
}

func validateDependencyReference(reference applicationv1.DependencyReference, path string) *PlanError {
	if !validUTF8Bytes(reference.Name, 1, 63) || len(validation.IsDNS1123Label(reference.Name)) > 0 {
		return invalid("InvalidDependencyName", "%s.name is not a DNS-1123 label of at most 63 characters", path)
	}
	if !validUTF8Bytes(reference.CatalogueChartID, 1, 63) || len(validation.IsValidLabelValue(reference.CatalogueChartID)) != 0 {
		return invalid("InvalidDependencyCatalogueChartID", "%s.catalogueChartID must be a Kubernetes label value", path)
	}
	if !planDigestPattern.MatchString(reference.PlanDigest) {
		return invalid("InvalidDependencyPlanDigest", "%s.planDigest must use the sha256 base64url contract", path)
	}
	return nil
}

func validateDependencyPlan(plan applicationv1.DependencyPlan, path string) *PlanError {
	if plan.Release.ChartID != plan.CatalogueChartID {
		return invalid("DependencyChartIDMismatch", "%s.release.chartID must equal catalogueChartID", path)
	}
	if !validUTF8Bytes(plan.Release.Chart, 1, 1024) {
		return invalid("InvalidDependencyChart", "%s.release.chart must be valid UTF-8 between 1 and 1024 bytes", path)
	}
	if !validUTF8Bytes(plan.Release.Version, 1, 253) {
		return invalid("InvalidDependencyChartVersion", "%s.release.version must be valid UTF-8 between 1 and 253 bytes", path)
	}
	if err := validateReleaseSourceV1Alpha2(plan.Release, path+".release"); err != nil {
		return err
	}
	if !validUTF8Bytes(plan.Release.ReleaseName, 1, 63) || len(validation.IsDNS1123Label(plan.Release.ReleaseName)) > 0 {
		return invalid("InvalidDependencyReleaseName", "%s.release.releaseName is not a DNS-1123 label", path)
	}
	if !validUTF8Bytes(plan.Release.TargetNamespace, 1, 63) || len(validation.IsDNS1123Label(plan.Release.TargetNamespace)) > 0 {
		return invalid("InvalidDependencyTargetNamespace", "%s.release.targetNamespace is not a DNS-1123 label", path)
	}
	if !utf8.ValidString(plan.Release.ValuesContent) || len([]byte(plan.Release.ValuesContent)) > maxValuesContentBytes {
		return invalid("InvalidDependencyValuesContent", "%s.release.valuesContent is invalid or exceeds %d bytes", path, maxValuesContentBytes)
	}
	if placeholderPattern.MatchString(plan.Release.ValuesContent) {
		return invalid("UnresolvedPlaceholder", "%s.release.valuesContent contains an unresolved placeholder", path)
	}
	if err := validateNonSensitiveValues(plan.Release.ValuesContent); err != nil {
		return invalid(err.Reason, "%s.release: %s", path, err.Message)
	}
	if !validDeletionPolicy(plan.DeletionPolicy) {
		return invalid("InvalidDeletionPolicy", "%s.deletionPolicy must be Delete or Retain", path)
	}
	if len(plan.Resources) > maxResources {
		return invalid("TooManyResources", "%s.resources exceeds %d entries", path, maxResources)
	}
	if plan.Release.CreateNamespace && len(plan.Resources) != 0 {
		return invalid("InvalidDependencyResources", "%s with createNamespace=true must not contain resources", path)
	}
	if err := validateDependencyPlanResources(plan.Resources, plan.Release.TargetNamespace, path); err != nil {
		return err
	}
	if len(plan.Dependencies) > maxDependencies {
		return invalid("TooManyDependencies", "%s.dependencies exceeds %d entries", path, maxDependencies)
	}
	names := make(map[string]struct{}, len(plan.Dependencies))
	for index, dependency := range plan.Dependencies {
		dependencyPath := fmt.Sprintf("%s.dependencies[%d]", path, index)
		if err := validateDependencyReference(dependency, dependencyPath); err != nil {
			return err
		}
		if _, exists := names[dependency.Name]; exists {
			return invalid("DuplicateDependencyName", "%s dependency name %q is duplicated", path, dependency.Name)
		}
		names[dependency.Name] = struct{}{}
	}
	return nil
}

func validateDependencyPlanResources(resources []applicationv1.ResourceSpec, targetNamespace, path string) *PlanError {
	resourceIDs := make(map[string]struct{}, len(resources))
	resourceNames := make(map[string]struct{}, len(resources))
	for index, resource := range resources {
		resourcePath := fmt.Sprintf("%s.resources[%d]", path, index)
		if !validUTF8Bytes(resource.ID, 1, 63) || len(validation.IsValidLabelValue(resource.ID)) != 0 {
			return invalid("InvalidResourceID", "%s.id must be a Kubernetes label value", resourcePath)
		}
		if _, exists := resourceIDs[resource.ID]; exists {
			return invalid("DuplicateResourceID", "%s resource id %q is duplicated", path, resource.ID)
		}
		resourceIDs[resource.ID] = struct{}{}
		if resource.APIVersion != "v1" || resource.Kind != "ConfigMap" {
			return invalid("UnsupportedResource", "%s must be a v1 ConfigMap", resourcePath)
		}
		if resource.Namespace != targetNamespace {
			return invalid("InvalidResourceNamespace", "%s must use %q", resourcePath, targetNamespace)
		}
		if !validUTF8Bytes(resource.Name, 1, 253) || len(validation.IsDNS1123Subdomain(resource.Name)) > 0 {
			return invalid("InvalidResourceName", "%s.name is not a DNS-1123 subdomain", resourcePath)
		}
		if _, exists := resourceNames[resource.Name]; exists {
			return invalid("DuplicateResourceIdentity", "%s ConfigMap %q is duplicated", path, resource.Name)
		}
		resourceNames[resource.Name] = struct{}{}
		if !validDeletionPolicy(resource.DeletionPolicy) {
			return invalid("InvalidDeletionPolicy", "%s.deletionPolicy must be Delete or Retain", resourcePath)
		}
		if len(resource.Data) > maxConfigMapEntries {
			return invalid("ConfigMapDataTooLarge", "%s.data exceeds %d entries", resourcePath, maxConfigMapEntries)
		}
		dataBytes := 0
		for key, value := range resource.Data {
			if !utf8.ValidString(key) || len([]byte(key)) > maxConfigMapKeyBytes || len(validation.IsConfigMapKey(key)) > 0 {
				return invalid("InvalidConfigMapKey", "%s contains an invalid data key", resourcePath)
			}
			if !utf8.ValidString(value) || len([]byte(value)) > maxConfigMapValueBytes {
				return invalid("InvalidConfigMapValue", "%s contains an invalid or oversized data value", resourcePath)
			}
			if placeholderPattern.MatchString(key) || placeholderPattern.MatchString(value) {
				return invalid("UnresolvedPlaceholder", "%s.data contains an unresolved placeholder", resourcePath)
			}
			if value != "" && sensitiveKeyPattern.MatchString(key) {
				return invalid("SensitiveConfigMapData", "%s.data contains a sensitive key", resourcePath)
			}
			dataBytes += len([]byte(key)) + len([]byte(value))
		}
		if dataBytes > maxConfigMapDataBytes {
			return invalid("ConfigMapDataTooLarge", "%s.data exceeds %d aggregate bytes", resourcePath, maxConfigMapDataBytes)
		}
	}
	return nil
}

func CanonicalPlan(spec applicationv1.OneKSApplicationSpec) ([]byte, error) {
	switch spec.PlanVersion {
	case applicationv1.PlanVersionV1Alpha1:
		return canonicalPlanV1Alpha1(spec)
	case applicationv1.PlanVersionV1Alpha2:
		return canonicalPlanV1Alpha2(spec)
	default:
		return nil, fmt.Errorf("unsupported planVersion %q", spec.PlanVersion)
	}
}

func canonicalPlanV1Alpha1(spec applicationv1.OneKSApplicationSpec) ([]byte, error) {
	resources := make([]any, len(spec.Resources))
	for index, resource := range spec.Resources {
		data := make(map[string]any, len(resource.Data))
		for key, value := range resource.Data {
			data[key] = value
		}
		resources[index] = map[string]any{
			"id": resource.ID, "apiVersion": resource.APIVersion,
			"kind": resource.Kind, "namespace": resource.Namespace,
			"name": resource.Name, "data": data,
			"deletionPolicy": string(resource.DeletionPolicy),
		}
	}
	plan := map[string]any{
		"clusterID": spec.ClusterID, "catalogueChartID": spec.CatalogueChartID,
		"planVersion": spec.PlanVersion, "executionMode": string(spec.ExecutionMode),
		"release": map[string]any{
			"chartID": spec.Release.ChartID, "repositoryURL": spec.Release.RepositoryURL,
			"chart": spec.Release.Chart, "version": spec.Release.Version,
			"releaseName":     spec.Release.ReleaseName,
			"targetNamespace": spec.Release.TargetNamespace,
			"createNamespace": spec.Release.CreateNamespace,
			"valuesContent":   spec.Release.ValuesContent,
		},
		"resources": resources, "deletionPolicy": string(spec.DeletionPolicy),
	}

	var output bytes.Buffer
	if err := writeCanonicalJSON(&output, plan); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func canonicalPlanV1Alpha2(spec applicationv1.OneKSApplicationSpec) ([]byte, error) {
	dependencies := make([]any, len(spec.Dependencies))
	for index, dependency := range spec.Dependencies {
		dependencies[index] = canonicalDependencyReference(dependency)
	}
	dependencyPlans := make([]any, len(spec.DependencyPlans))
	for index, dependencyPlan := range spec.DependencyPlans {
		dependencyPlans[index] = canonicalDependencyPlan(dependencyPlan)
	}
	plan := map[string]any{
		"clusterID": spec.ClusterID, "catalogueChartID": spec.CatalogueChartID,
		"planVersion": spec.PlanVersion, "executionMode": string(spec.ExecutionMode),
		"release": canonicalRelease(spec.Release), "resources": canonicalResources(spec.Resources),
		"role": string(spec.Role), "dependencies": dependencies,
		"dependencyPlans": dependencyPlans, "deletionPolicy": string(spec.DeletionPolicy),
	}

	var output bytes.Buffer
	if err := writeCanonicalJSON(&output, plan); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func canonicalDependencyPlan(plan applicationv1.DependencyPlan) map[string]any {
	dependencies := make([]any, len(plan.Dependencies))
	for index, dependency := range plan.Dependencies {
		dependencies[index] = canonicalDependencyReference(dependency)
	}
	return map[string]any{
		"name": plan.Name, "catalogueChartID": plan.CatalogueChartID,
		"planDigest": plan.PlanDigest, "release": canonicalRelease(plan.Release),
		"resources": canonicalResources(plan.Resources), "dependencies": dependencies,
		"deletionPolicy": string(plan.DeletionPolicy),
	}
}

func canonicalDependencyReference(reference applicationv1.DependencyReference) map[string]any {
	return map[string]any{
		"name": reference.Name, "catalogueChartID": reference.CatalogueChartID,
		"planDigest": reference.PlanDigest,
	}
}

func canonicalRelease(release applicationv1.ReleaseSpec) map[string]any {
	return map[string]any{
		"chartID": release.ChartID, "repositoryURL": release.RepositoryURL,
		"chart": release.Chart, "version": release.Version,
		"releaseName": release.ReleaseName, "targetNamespace": release.TargetNamespace,
		"createNamespace": release.CreateNamespace, "valuesContent": release.ValuesContent,
	}
}

func canonicalResources(resourceSpecs []applicationv1.ResourceSpec) []any {
	resources := make([]any, len(resourceSpecs))
	for index, resource := range resourceSpecs {
		data := make(map[string]any, len(resource.Data))
		for key, value := range resource.Data {
			data[key] = value
		}
		resources[index] = map[string]any{
			"id": resource.ID, "apiVersion": resource.APIVersion,
			"kind": resource.Kind, "namespace": resource.Namespace,
			"name": resource.Name, "data": data,
			"deletionPolicy": string(resource.DeletionPolicy),
		}
	}
	return resources
}

func Digest(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return "sha256-" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func writeCanonicalJSON(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if !utf8.ValidString(key) {
				return fmt.Errorf("object key is not valid UTF-8")
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeJSONString(output, key); err != nil {
				return err
			}
			output.WriteByte(':')
			if err := writeCanonicalJSON(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	case []any:
		output.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeCanonicalJSON(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case string:
		return writeJSONString(output, typed)
	case bool:
		output.WriteString(strconv.FormatBool(typed))
	case int:
		output.WriteString(strconv.Itoa(typed))
	case int8:
		output.WriteString(strconv.FormatInt(int64(typed), 10))
	case int16:
		output.WriteString(strconv.FormatInt(int64(typed), 10))
	case int32:
		output.WriteString(strconv.FormatInt(int64(typed), 10))
	case int64:
		output.WriteString(strconv.FormatInt(typed, 10))
	case uint:
		output.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint8:
		output.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint16:
		output.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint32:
		output.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint64:
		output.WriteString(strconv.FormatUint(typed, 10))
	case json.Number:
		if strings.ContainsAny(string(typed), ".eE") {
			return fmt.Errorf("floating-point values are not canonical")
		}
		integer, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil || strconv.FormatInt(integer, 10) != string(typed) {
			return fmt.Errorf("invalid canonical integer %q", typed)
		}
		output.WriteString(string(typed))
	default:
		return fmt.Errorf("unsupported canonical value type %T", value)
	}
	return nil
}

func writeJSONString(output *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("string is not valid UTF-8")
	}
	const hex = "0123456789abcdef"
	output.WriteByte('"')
	for _, codepoint := range value {
		switch codepoint {
		case '"':
			output.WriteString(`\"`)
		case '\\':
			output.WriteString(`\\`)
		case '\b':
			output.WriteString(`\b`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\f':
			output.WriteString(`\f`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if codepoint >= 0x20 && codepoint <= 0x7e {
				output.WriteRune(codepoint)
			} else if codepoint <= 0xffff {
				writeUTF16Escape(output, uint16(codepoint), hex)
			} else {
				high, low := utf16.EncodeRune(codepoint)
				writeUTF16Escape(output, uint16(high), hex)
				writeUTF16Escape(output, uint16(low), hex)
			}
		}
	}
	output.WriteByte('"')
	return nil
}

func writeUTF16Escape(output *bytes.Buffer, value uint16, hex string) {
	output.WriteString(`\u`)
	output.WriteByte(hex[(value>>12)&0xf])
	output.WriteByte(hex[(value>>8)&0xf])
	output.WriteByte(hex[(value>>4)&0xf])
	output.WriteByte(hex[value&0xf])
}

func validateRepositoryURL(raw string) *PlanError {
	if !validUTF8Bytes(raw, 1, 2048) {
		return invalid("InvalidRepositoryURL", "repositoryURL must be valid UTF-8 between 1 and 2048 bytes")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return invalid("InvalidRepositoryURL", "repositoryURL must be an HTTPS URL without credentials or a fragment")
	}
	return nil
}

func validateReleaseSourceV1Alpha2(release applicationv1.ReleaseSpec, path string) *PlanError {
	if release.RepositoryURL == "" {
		parsed, err := url.Parse(release.Chart)
		if err != nil || !ociChartPattern.MatchString(release.Chart) || parsed.Scheme != "oci" || parsed.Host == "" {
			return invalid("InvalidReleaseSource", "%s must use a non-empty OCI chart when repositoryURL is empty", path)
		}
		return nil
	}
	if strings.HasPrefix(release.Chart, "oci://") {
		return invalid("InvalidReleaseSource", "%s must not combine an OCI chart with repositoryURL", path)
	}
	if err := validateRepositoryURL(release.RepositoryURL); err != nil {
		return invalid(err.Reason, "%s: %s", path, err.Message)
	}
	return nil
}

func validateNonSensitiveValues(content string) *PlanError {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	var values any
	if err := yaml.Unmarshal([]byte(content), &values); err != nil {
		return invalid("InvalidValuesContent", "valuesContent must be valid YAML")
	}
	mapping, ok := values.(map[string]any)
	if !ok {
		return invalid("InvalidValuesContent", "valuesContent must be a YAML mapping")
	}
	if containsSensitiveValue(mapping, false) {
		return invalid("SensitiveValuesContent", "valuesContent contains a sensitive value")
	}
	return nil
}

func containsSensitiveValue(value any, sensitivePath bool) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if containsSensitiveValue(nested, sensitivePath || sensitiveKeyPattern.MatchString(key)) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsSensitiveValue(nested, sensitivePath) {
				return true
			}
		}
	case nil:
		return false
	default:
		return sensitivePath && fmt.Sprint(typed) != ""
	}
	return false
}

func validUTF8Bytes(value string, minimum, maximum int) bool {
	length := len([]byte(value))
	return utf8.ValidString(value) && length >= minimum && length <= maximum
}

func validApplicationFinalizers(finalizers []string) bool {
	return len(finalizers) == 0 || (len(finalizers) == 1 && finalizers[0] == applicationv1.ApplicationFinalizer)
}

func validDeletionPolicy(policy applicationv1.DeletionPolicy) bool {
	return policy == applicationv1.DeletionPolicyDelete || policy == applicationv1.DeletionPolicyRetain
}

func invalid(reason, format string, args ...any) *PlanError {
	message := fmt.Sprintf(format, args...)
	message = strings.TrimSpace(message)
	if len(message) > 512 {
		message = message[:512]
	}
	return &PlanError{Reason: reason, Message: message}
}
