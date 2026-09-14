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
	"fmt"
	"net/url"
	"regexp"
	"strings"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	"sigs.k8s.io/yaml"
)

var (
	placeholderPattern    = regexp.MustCompile(`\$\{[^}]+\}`)
	sensitiveKeyPattern   = regexp.MustCompile(`(?i)(password|secret|token|credential|api.?key|private.?key)`)
	ociChartPattern       = regexp.MustCompile(`^oci://[^[:space:]]+$`)
	valuesReferenceLeaves = map[string]bool{
		"authorizedsecretsall": true, "existingsecret": true, "existingsecretname": true, "secretname": true,
	}
	valuesReferenceContainers = map[string]bool{
		"authorizedsecrets": true, "imagepullsecrets": true, "secretkeys": true, "secretref": true, "secretkeyref": true,
	}
)

type PlanError struct {
	Reason  string
	Message string
}

func (e *PlanError) Error() string { return e.Message }

func validatePlan(app *applicationv1.OneKSApplication, clusterID string) *PlanError {
	if app.Spec.ClusterID != clusterID {
		return invalid("ClusterIDMismatch", "spec.clusterID does not match this controller")
	}
	if app.Spec.ExternalDetection != nil && len(app.Spec.ManagedResources) != 0 {
		return invalid("InvalidExternalDetection", "externalDetection must not be combined with managedResources")
	}
	if err := validateRelease(app.Spec.Release, app.Spec.CatalogueChartID, "release"); err != nil {
		return err
	}
	if app.Spec.Role == applicationv1.ApplicationRoleDependency {
		if expectedName := dependencyApplicationName(app.Spec.Release.ReleaseName); app.Name != expectedName {
			return invalid("InvalidDependencyApplicationName", "application name must be %q for releaseName %q", expectedName, app.Spec.Release.ReleaseName)
		}
	}
	if err := validateDependencyContract(app); err != nil {
		return err
	}
	if app.Spec.Uninstall != nil {
		if app.Spec.Role != applicationv1.ApplicationRoleDependency && len(app.Spec.Uninstall.PreActions) != 0 {
			return invalid("InvalidUninstallRole", "uninstall.preActions is permitted only for Dependency applications")
		}
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
	return nil
}

func validateDependencyContract(app *applicationv1.OneKSApplication) *PlanError {
	planIndex := make(map[string]*applicationv1.DependencyPlan, len(app.Spec.DependencyPlans))
	for index := range app.Spec.DependencyPlans {
		plan := &app.Spec.DependencyPlans[index]
		path := fmt.Sprintf("dependencyPlans[%d]", index)
		planIndex[plan.Name] = plan
		if err := validateDependencyPlan(*plan, path); err != nil {
			return err
		}
		if app.Spec.Role == applicationv1.ApplicationRoleRoot {
			if expectedName := dependencyApplicationName(plan.Release.ReleaseName); plan.Name != expectedName {
				return invalid("InvalidDependencyApplicationName", "%s.name must be %q for releaseName %q", path, expectedName, plan.Release.ReleaseName)
			}
		}
	}

	if app.Spec.Role == applicationv1.ApplicationRoleRoot {
		if err := validateRootDependencyGraph(app.Spec.Dependencies, app.Spec.DependencyPlans, planIndex); err != nil {
			return err
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
	var visit func(applicationv1.DependencyReference, string) *PlanError
	visit = func(reference applicationv1.DependencyReference, path string) *PlanError {
		plan, exists := planIndex[reference.Name]
		if !exists {
			return invalid("UnresolvedDependency", "%s references missing dependencyPlan %q", path, reference.Name)
		}
		if plan.CatalogueChartID != reference.CatalogueChartID {
			return invalid("UnresolvedDependency", "%s catalogueChartID does not match dependencyPlan %q", path, reference.Name)
		}
		switch states[plan.Name] {
		case visiting:
			return invalid("DependencyCycle", "dependency graph contains a cycle through %q", plan.Name)
		case visited:
			return nil
		}
		states[plan.Name] = visiting
		for index, dependency := range plan.Dependencies {
			path := fmt.Sprintf("dependencyPlan %q dependencies[%d]", plan.Name, index)
			if err := visit(dependency, path); err != nil {
				return err
			}
		}
		states[plan.Name] = visited
		return nil
	}

	for index, dependency := range rootDependencies {
		if err := visit(dependency, fmt.Sprintf("dependencies[%d]", index)); err != nil {
			return err
		}
	}
	for _, plan := range plans {
		if states[plan.Name] != visited {
			return invalid("OrphanDependencyPlan", "dependencyPlan %q is not reachable from root dependencies", plan.Name)
		}
	}
	return nil
}

func validateDependencyPlan(plan applicationv1.DependencyPlan, path string) *PlanError {
	if err := validateManagedResources(plan.ManagedResources); err != nil {
		return invalid(err.Reason, "%s: %s", path, err.Message)
	}
	if plan.ExternalDetection != nil && len(plan.ManagedResources) != 0 {
		return invalid("InvalidExternalDetection", "%s.externalDetection must not be combined with managedResources", path)
	}
	if err := validateRelease(plan.Release, plan.CatalogueChartID, path+".release"); err != nil {
		return err
	}
	if plan.Uninstall != nil {
		if err := validateUninstall(*plan.Uninstall, path+".uninstall"); err != nil {
			return err
		}
	}
	return nil
}

func validateRelease(release applicationv1.ReleaseSpec, catalogueChartID, path string) *PlanError {
	if release.ChartID != catalogueChartID {
		return invalid("ChartIDMismatch", "%s.chartID must equal catalogueChartID", path)
	}
	if err := validateReleaseSource(release, path); err != nil {
		return err
	}
	if placeholderPattern.MatchString(release.ValuesContent) {
		return invalid("UnresolvedPlaceholder", "%s.valuesContent contains an unresolved placeholder", path)
	}
	if err := validateNonSensitiveValues(release.ValuesContent); err != nil {
		return invalid(err.Reason, "%s: %s", path, err.Message)
	}
	return nil
}

func validateRepositoryURL(raw string) *PlanError {
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
			nestedSensitivity := valuesSensitivityNormal
			key = strings.ToLower(key)
			switch {
			case sensitivity == valuesSensitivityReference:
				nestedSensitivity = valuesSensitivityReference
			case key == "secrettargets", key == "includeall" && isBooleanValue(nested):
			case valuesReferenceContainers[key]:
				nestedSensitivity = valuesSensitivityReference
			case valuesReferenceLeaves[key] && isScalarValue(nested):
				nestedSensitivity = valuesSensitivityReference
			case sensitivity == valuesSensitivitySensitive || sensitiveKeyPattern.MatchString(key):
				nestedSensitivity = valuesSensitivitySensitive
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

func isScalarValue(value any) bool {
	_, mapping := value.(map[string]any)
	_, sequence := value.([]any)
	return !mapping && !sequence
}

func isBooleanValue(value any) bool { _, ok := value.(bool); return ok }

func invalid(reason, format string, args ...any) *PlanError {
	message := strings.TrimSpace(fmt.Sprintf(format, args...))
	if len(message) > 512 {
		message = message[:512]
	}
	return &PlanError{Reason: reason, Message: message}
}
