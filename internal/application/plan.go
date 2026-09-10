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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	maxValuesContentBytes  = 65536
	maxCanonicalPlanBytes  = 131072
	maxDependencies        = 16
	maxDependencyPlans     = 16
	maxManagedResources    = 16
	maxProtectedSecrets    = 16
	maxUninstallPreActions = 8
	maxMergePatchBytes     = 16384
)

var (
	placeholderPattern  = regexp.MustCompile(`\$\{[^}]+\}`)
	sensitiveKeyPattern = regexp.MustCompile(`(?i)(password|secret|token|credential|api.?key|private.?key)`)
	planDigestPattern   = regexp.MustCompile(`^sha256-[A-Za-z0-9_-]{43}$`)
	ociChartPattern     = regexp.MustCompile(`^oci://[^[:space:]]+$`)
	kindPattern         = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)
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
	if len(app.Finalizers) > 1 || (len(app.Finalizers) == 1 && app.Finalizers[0] != applicationv1.ApplicationFinalizer) {
		return invalid("InvalidFinalizers", "application finalizers may contain only the OneKS application finalizer")
	}
	if app.Spec.ClusterID == "" || app.Spec.ClusterID != config.ClusterID {
		return invalid("ClusterIDMismatch", "spec.clusterID does not match this controller")
	}
	if !validUTF8Bytes(app.Spec.ClusterID, 1, 63) || len(validation.IsValidLabelValue(app.Spec.ClusterID)) > 0 {
		return invalid("InvalidClusterID", "spec.clusterID is not a Kubernetes label value")
	}
	if app.Spec.PlanVersion != applicationv1.PlanVersion {
		return invalid("UnsupportedPlanVersion", "unsupported planVersion %q", app.Spec.PlanVersion)
	}
	if app.Spec.Role != applicationv1.ApplicationRoleRoot && app.Spec.Role != applicationv1.ApplicationRoleDependency {
		return invalid("InvalidApplicationRole", "plan-v1beta1 role must be Root or Dependency")
	}
	if app.Spec.Role == applicationv1.ApplicationRoleDependency {
		if len(app.Spec.ManagedResources) != 0 {
			return invalid("InvalidDependencyManagedResources", "Dependency applications must not contain managedResources")
		}
		if app.Spec.SecretInputRef != nil || len(app.Spec.ProtectedSecrets) != 0 || app.Spec.Release.AuthSecret != nil {
			return invalid("InvalidDependencyProtectedSecrets", "Dependency applications must not contain protected Secret fields")
		}
	}
	if app.Spec.Uninstall != nil && app.Spec.Role != applicationv1.ApplicationRoleDependency {
		return invalid("InvalidUninstall", "top-level uninstall is permitted only for Dependency applications")
	}
	if app.Spec.ExternalDetection != nil && app.Spec.Role != applicationv1.ApplicationRoleDependency {
		return invalid("InvalidExternalDetection", "top-level externalDetection is permitted only for Dependency applications")
	}
	if app.Spec.ExternalDetection != nil {
		if err := validateExternalDetection(*app.Spec.ExternalDetection, "externalDetection"); err != nil {
			return err
		}
	}
	if !validDeletionPolicy(app.Spec.DeletionPolicy) {
		return invalid("InvalidDeletionPolicy", "deletionPolicy must be Delete or Retain")
	}
	if !validUTF8Bytes(app.Spec.CatalogueChartID, 1, 63) || len(validation.IsValidLabelValue(app.Spec.CatalogueChartID)) != 0 {
		return invalid("InvalidCatalogueChartID", "catalogueChartID must be a Kubernetes label value")
	}
	if err := validateRelease(app.Spec.Release, app.Spec.CatalogueChartID, "release"); err != nil {
		return err
	}
	if app.Spec.Role == applicationv1.ApplicationRoleDependency {
		expectedName := dependencyApplicationName(app.Spec.Release.ReleaseName)
		if app.Name != expectedName {
			return invalid("InvalidDependencyApplicationName", "application name must be %q for releaseName %q", expectedName, app.Spec.Release.ReleaseName)
		}
	}
	if err := validateDependencyContract(app); err != nil {
		return err
	}
	if app.Spec.Uninstall != nil {
		if err := validateUninstall(*app.Spec.Uninstall, "uninstall"); err != nil {
			return err
		}
	}
	if err := validateManagedResources(app.Spec.ManagedResources); err != nil {
		return err
	}
	if err := validateProtectedSecretContract(app.Spec); err != nil {
		return err
	}
	if err := validateReleaseAuthSecret(app.Spec); err != nil {
		return err
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
	canonical, err := canonicalPlan(child)
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
		PlanVersion: applicationv1.PlanVersion, PlanDigest: plan.PlanDigest,
		Release: plan.Release,
		Role:    applicationv1.ApplicationRoleDependency, Dependencies: plan.Dependencies,
		DependencyPlans: nil, Uninstall: plan.Uninstall, ExternalDetection: plan.ExternalDetection,
		DeletionPolicy: plan.DeletionPolicy,
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
	if plan.Release.AuthSecret != nil {
		return invalid("InvalidDependencyAuthSecret", "%s.release does not permit authSecret", path)
	}
	if err := validateRelease(plan.Release, plan.CatalogueChartID, path+".release"); err != nil {
		return err
	}
	if !validDeletionPolicy(plan.DeletionPolicy) {
		return invalid("InvalidDeletionPolicy", "%s.deletionPolicy must be Delete or Retain", path)
	}
	if plan.Uninstall != nil {
		if err := validateUninstall(*plan.Uninstall, path+".uninstall"); err != nil {
			return err
		}
	}
	if plan.ExternalDetection != nil {
		if err := validateExternalDetection(*plan.ExternalDetection, path+".externalDetection"); err != nil {
			return err
		}
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

func validateRelease(release applicationv1.ReleaseSpec, catalogueChartID, path string) *PlanError {
	if !validUTF8Bytes(release.ChartID, 1, 63) || len(validation.IsValidLabelValue(release.ChartID)) != 0 || release.ChartID != catalogueChartID {
		return invalid("ChartIDMismatch", "%s.chartID must equal catalogueChartID", path)
	}
	if !validUTF8Bytes(release.Chart, 1, 1024) {
		return invalid("InvalidChart", "%s.chart must be valid UTF-8 between 1 and 1024 bytes", path)
	}
	if !validUTF8Bytes(release.Version, 1, 253) {
		return invalid("InvalidChartVersion", "%s.version must be valid UTF-8 between 1 and 253 bytes", path)
	}
	if err := validateReleaseSource(release, path); err != nil {
		return err
	}
	if !validUTF8Bytes(release.ReleaseName, 1, 63) || len(validation.IsDNS1123Label(release.ReleaseName)) > 0 {
		return invalid("InvalidReleaseName", "%s.releaseName is not a DNS-1123 label of at most 63 characters", path)
	}
	if !validUTF8Bytes(release.TargetNamespace, 1, 63) || len(validation.IsDNS1123Label(release.TargetNamespace)) > 0 {
		return invalid("InvalidTargetNamespace", "%s.targetNamespace is not a DNS-1123 label of at most 63 characters", path)
	}
	if !utf8.ValidString(release.ValuesContent) {
		return invalid("InvalidValuesContent", "%s.valuesContent must be valid UTF-8", path)
	}
	if len(release.ValuesContent) > maxValuesContentBytes {
		return invalid("ValuesContentTooLarge", "%s.valuesContent exceeds %d bytes", path, maxValuesContentBytes)
	}
	if placeholderPattern.MatchString(release.ValuesContent) {
		return invalid("UnresolvedPlaceholder", "%s.valuesContent contains an unresolved placeholder", path)
	}
	if err := validateNonSensitiveValues(release.ValuesContent); err != nil {
		return invalid(err.Reason, "%s: %s", path, err.Message)
	}
	return nil
}

func validateExternalDetection(detection applicationv1.ExternalDetectionSpec, path string) *PlanError {
	if detection.Detector != applicationv1.ExternalDetectorCertManager {
		return invalid("InvalidExternalDetector", "%s.detector must be cert-manager", path)
	}
	return nil
}

func validateReleaseAuthSecret(spec applicationv1.OneKSApplicationSpec) *PlanError {
	authSecret := spec.Release.AuthSecret
	if authSecret == nil {
		return nil
	}
	if !validUTF8Bytes(authSecret.Name, 1, 253) || len(validation.IsDNS1123Subdomain(authSecret.Name)) > 0 {
		return invalid("InvalidAuthSecretName", "release.authSecret.name is not a DNS-1123 subdomain")
	}
	if err := validateRepositoryURL(spec.Release.RepositoryURL); err != nil {
		return invalid("InvalidAuthSecretRepository", "release.authSecret requires an HTTPS repositoryURL")
	}
	matches := 0
	for _, protected := range spec.ProtectedSecrets {
		if protected.BuilderType == applicationv1.ProtectedSecretBuilderBasicAuth &&
			protected.Namespace == HelmChartNamespace && protected.Name == authSecret.Name {
			matches++
		}
	}
	if matches != 1 {
		return invalid("InvalidAuthSecretProtectedSecret", "release.authSecret must match exactly one protected basicAuthSecret in kube-system")
	}
	return nil
}

func validateUninstall(uninstall applicationv1.UninstallSpec, path string) *PlanError {
	if len(uninstall.PreActions) == 0 || len(uninstall.PreActions) > maxUninstallPreActions {
		return invalid("InvalidUninstallActions", "%s.preActions must contain between 1 and %d entries", path, maxUninstallPreActions)
	}
	for index, action := range uninstall.PreActions {
		actionPath := fmt.Sprintf("%s.preActions[%d]", path, index)
		if action.Type != applicationv1.UninstallPreActionKubernetesPatch {
			return invalid("UnsupportedUninstallAction", "%s.type must be kubernetesPatch", actionPath)
		}
		if action.PatchType != applicationv1.KubernetesPatchTypeMerge {
			return invalid("UnsupportedUninstallPatchType", "%s.patchType must be merge", actionPath)
		}
		resource := action.Resource
		parts := strings.Split(resource.APIVersion, "/")
		validAPIVersion := len(parts) == 1 && len(validation.IsDNS1123Label(parts[0])) == 0 ||
			len(parts) == 2 && len(validation.IsDNS1123Subdomain(parts[0])) == 0 && len(validation.IsDNS1123Label(parts[1])) == 0
		if !validAPIVersion || !kindPattern.MatchString(resource.Kind) {
			return invalid("InvalidUninstallResourceIdentity", "%s.resource must contain a valid apiVersion and kind", actionPath)
		}
		if resource.Namespace != "" && len(validation.IsDNS1123Label(resource.Namespace)) != 0 {
			return invalid("InvalidUninstallResourceIdentity", "%s.resource.namespace must be a DNS-1123 label", actionPath)
		}
		if len(validation.IsDNS1123Subdomain(resource.Name)) != 0 {
			return invalid("InvalidUninstallResourceIdentity", "%s.resource.name must be a DNS-1123 subdomain", actionPath)
		}
		if placeholderPattern.MatchString(resource.APIVersion) || placeholderPattern.MatchString(resource.Kind) ||
			placeholderPattern.MatchString(resource.Namespace) || placeholderPattern.MatchString(resource.Name) ||
			placeholderPattern.MatchString(action.PatchJSON) {
			return invalid("UnresolvedPlaceholder", "%s contains an unresolved placeholder", actionPath)
		}
		if !utf8.ValidString(action.PatchJSON) || len(action.PatchJSON) == 0 || len(action.PatchJSON) > maxMergePatchBytes {
			return invalid("InvalidUninstallPatch", "%s.patchJSON must be valid UTF-8 between 1 and %d bytes", actionPath, maxMergePatchBytes)
		}
		patch := map[string]any{}
		if err := json.Unmarshal([]byte(action.PatchJSON), &patch); err != nil || patch == nil || len(patch) == 0 {
			return invalid("InvalidUninstallPatch", "%s.patchJSON must be a non-empty JSON object", actionPath)
		}
	}
	return nil
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

func validateReleaseSource(release applicationv1.ReleaseSpec, path string) *PlanError {
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
	if containsSensitiveValue(mapping, valuesSensitivityNormal) {
		return invalid("SensitiveValuesContent", "valuesContent contains a sensitive value")
	}
	return nil
}

type valuesSensitivity uint8

const (
	valuesSensitivityNormal valuesSensitivity = iota
	valuesSensitivitySensitive
	valuesSensitivityReference
)

func containsSensitiveValue(value any, sensitivity valuesSensitivity) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			nestedSensitivity := sensitivity
			switch {
			case sensitivity == valuesSensitivityReference:
				nestedSensitivity = valuesSensitivityReference
			case strings.EqualFold(key, "secretTargets"):
				nestedSensitivity = valuesSensitivityNormal
			case strings.EqualFold(key, "includeAll") && isBooleanValue(nested):
				nestedSensitivity = valuesSensitivityNormal
			case isValuesReferenceContainer(key):
				nestedSensitivity = valuesSensitivityReference
			case isValuesReferenceLeaf(key) && isScalarValue(nested):
				nestedSensitivity = valuesSensitivityReference
			case sensitivity == valuesSensitivitySensitive || sensitiveKeyPattern.MatchString(key):
				nestedSensitivity = valuesSensitivitySensitive
			default:
				nestedSensitivity = valuesSensitivityNormal
			}
			if containsSensitiveValue(nested, nestedSensitivity) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsSensitiveValue(nested, sensitivity) {
				return true
			}
		}
	case nil:
		return false
	default:
		return sensitivity == valuesSensitivitySensitive && fmt.Sprint(typed) != ""
	}
	return false
}

func isValuesReferenceLeaf(key string) bool {
	switch strings.ToLower(key) {
	case "authorizedsecretsall", "existingsecret", "existingsecretname", "secretname":
		return true
	default:
		return false
	}
}

func isValuesReferenceContainer(key string) bool {
	switch strings.ToLower(key) {
	case "authorizedsecrets", "imagepullsecrets", "secretkeys", "secretref", "secretkeyref":
		return true
	default:
		return false
	}
}

func isScalarValue(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

func isBooleanValue(value any) bool {
	_, ok := value.(bool)
	return ok
}

func validUTF8Bytes(value string, minimum, maximum int) bool {
	length := len(value)
	return utf8.ValidString(value) && length >= minimum && length <= maximum
}

func validDeletionPolicy(policy applicationv1.DeletionPolicy) bool {
	return policy == applicationv1.DeletionPolicyDelete || policy == applicationv1.DeletionPolicyRetain
}

func invalid(reason, format string, args ...any) *PlanError {
	message := strings.TrimSpace(fmt.Sprintf(format, args...))
	if len(message) > 512 {
		message = message[:512]
	}
	return &PlanError{Reason: reason, Message: message}
}
