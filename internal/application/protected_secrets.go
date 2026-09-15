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
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"sort"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type InputSecretValidationError struct {
	Message string
}

func (e *InputSecretValidationError) Error() string { return e.Message }

// protectedSecretAPIError deliberately hides the underlying API error text so
// an API server or transport cannot accidentally echo Secret content through
// controller logs. Unwrap preserves retry and classification behavior.
type protectedSecretAPIError struct {
	operation string
	namespace string
	name      string
	cause     error
}

func (e *protectedSecretAPIError) Error() string {
	return fmt.Sprintf("%s Secret %s/%s: Kubernetes API request failed", e.operation, e.namespace, e.name)
}

func (e *protectedSecretAPIError) Unwrap() error { return e.cause }

type protectedSecretsObservation struct {
	componentObservation
	statuses  []applicationv1.ResourceStatus
	completed int32
	current   string
}

func usesProtectedSecrets(app *applicationv1.OneKSApplication) bool {
	return app.Spec.Role == applicationv1.ApplicationRoleRoot && len(app.Spec.ProtectedSecrets) != 0
}

func validateProtectedSecretContract(spec applicationv1.OneKSApplicationSpec) *PlanError {
	if len(spec.ProtectedSecrets) == 0 || spec.SecretInputRef == nil {
		return nil
	}
	input := spec.SecretInputRef
	for index, secret := range spec.ProtectedSecrets {
		path := fmt.Sprintf("protectedSecrets[%d]", index)
		if secret.Namespace == input.Namespace && secret.Name == input.Name {
			return invalid("ProtectedSecretInputIdentityCollision", "%s must not target secretInputRef", path)
		}
		if err := validateProtectedSecretContent(secret, path); err != nil {
			return err
		}
	}
	return nil
}

func validateProtectedSecretContent(secret applicationv1.ProtectedSecretSpec, path string) *PlanError {
	var values []string
	switch secret.BuilderType {
	case applicationv1.ProtectedSecretBuilderBasicAuth:
		values = []string{secret.Username}
	case applicationv1.ProtectedSecretBuilderDockerConfigJSON:
		values = []string{secret.Registry, secret.Username, secret.Email}
	}
	for _, value := range values {
		if placeholderPattern.MatchString(value) {
			return invalid("UnresolvedPlaceholder", "%s contains an unresolved placeholder", path)
		}
	}
	return nil
}

func requiredSecretInputKeys(resources []applicationv1.ProtectedSecretSpec) []string {
	keys := make(map[string]struct{})
	for _, resource := range resources {
		switch resource.BuilderType {
		case applicationv1.ProtectedSecretBuilderBasicAuth, applicationv1.ProtectedSecretBuilderDockerConfigJSON:
			keys[resource.PasswordInputKey] = struct{}{}
		case applicationv1.ProtectedSecretBuilderOpaque:
			for _, mapping := range resource.OpaqueData {
				keys[mapping.InputKey] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func (r *Reconciler) readSecretInput(ctx context.Context, app *applicationv1.OneKSApplication) (*corev1.Secret, bool, error) {
	input := &corev1.Secret{}
	reference := app.Spec.SecretInputRef
	err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: reference.Namespace, Name: reference.Name}, input)
	if apierrors.IsNotFound(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, &protectedSecretAPIError{operation: "read input", namespace: reference.Namespace, name: reference.Name, cause: err}
	}
	expectedLabels := map[string]string{
		LabelRootManagedBy: RootManagedByValue, LabelProducer: ProducerValue,
		LabelClusterID: app.Spec.ClusterID, LabelCatalogueChartID: app.Spec.CatalogueChartID,
		LabelApplicationName: app.Name,
	}
	if !labelSubsetMatches(input.GetLabels(), expectedLabels) {
		return nil, false, &InputSecretValidationError{Message: "input Secret labels do not match the compiled application"}
	}
	if app.Status.SecretInputUID != "" && string(input.UID) != app.Status.SecretInputUID {
		return nil, false, &InputSecretValidationError{Message: "input Secret UID does not match the compiled reference"}
	}
	if input.Type != corev1.SecretTypeOpaque {
		return nil, false, &InputSecretValidationError{Message: "input Secret must have type Opaque"}
	}
	if input.Immutable == nil || !*input.Immutable {
		return nil, false, &InputSecretValidationError{Message: "input Secret must be immutable"}
	}
	required := requiredSecretInputKeys(app.Spec.ProtectedSecrets)
	if len(input.Data) != len(required) {
		return nil, false, &InputSecretValidationError{Message: "input Secret data key set does not match the protected Secret contract"}
	}
	for _, key := range required {
		if _, found := input.Data[key]; !found {
			return nil, false, &InputSecretValidationError{Message: "input Secret data key set does not match the protected Secret contract"}
		}
	}
	return input, false, nil
}

func (r *Reconciler) bindSecretInput(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	if !usesProtectedSecrets(app) || app.Status.SecretInputUID != "" {
		return false, nil
	}
	input, missing, err := r.readSecretInput(ctx, app)
	if err != nil {
		return false, err
	}
	if missing {
		return true, nil
	}
	status := baseStatus(app)
	status.SecretInputUID = string(input.UID)
	if status.Phase == "" {
		status.Phase = applicationv1.PhasePending
	}
	if err := r.updateStatus(ctx, app, status); err != nil {
		return false, err
	}
	r.event(app, corev1.EventTypeNormal, "InputSecretBound", "Input Secret UID was bound before plan execution")
	return true, nil
}

func (r *Reconciler) preflightProtectedSecretOwnership(ctx context.Context, app *applicationv1.OneKSApplication, deleting bool) error {
	if !usesProtectedSecrets(app) {
		return nil
	}
	reader := r.authoritativeReader()
	for _, resource := range app.Spec.ProtectedSecrets {
		if deleting && resource.DeletionPolicy == applicationv1.DeletionPolicyRetain {
			continue
		}
		current := &corev1.Secret{}
		err := reader.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, current)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return &protectedSecretAPIError{operation: "preflight protected", namespace: resource.Namespace, name: resource.Name, cause: err}
		}
		if !ownershipMatches(app, current) {
			return &OwnershipConflictError{Kind: "Secret", Namespace: resource.Namespace, Name: resource.Name}
		}
	}
	return nil
}

// applyProtectedSecrets returns false when inputs or concurrent changes require a retry.
func (r *Reconciler) applyProtectedSecrets(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	input, missing, err := r.readSecretInput(ctx, app)
	if err != nil || missing {
		return false, err
	}
	if err := r.preflightProtectedSecretOwnership(ctx, app, false); err != nil {
		return false, err
	}
	reader := r.authoritativeReader()
	for _, resource := range app.Spec.ProtectedSecrets {
		desired, err := desiredProtectedSecret(app, resource, input)
		if err != nil {
			return false, err
		}
		current := &corev1.Secret{}
		err = reader.Get(ctx, client.ObjectKeyFromObject(desired), current)
		if apierrors.IsNotFound(err) {
			createErr := r.Create(ctx, desired)
			if apierrors.IsAlreadyExists(createErr) {
				return false, nil
			}
			if createErr != nil {
				return false, &protectedSecretAPIError{operation: "create protected", namespace: resource.Namespace, name: resource.Name, cause: createErr}
			}
			ctrl.LoggerFrom(ctx).Info(
				"protected Secret created",
				"resourceID", resource.ID,
				"resourceNamespace", resource.Namespace, "name", resource.Name,
			)
			r.event(app, corev1.EventTypeNormal, "ProtectedSecretCreated", fmt.Sprintf("Protected Secret %s/%s created", resource.Namespace, resource.Name))
			continue
		}
		if err != nil {
			return false, &protectedSecretAPIError{operation: "read protected", namespace: resource.Namespace, name: resource.Name, cause: err}
		}
		if !ownershipMatches(app, current) {
			return false, &OwnershipConflictError{Kind: "Secret", Namespace: resource.Namespace, Name: resource.Name}
		}
		if err := r.updateProtectedSecret(ctx, current, desired, resource); err != nil {
			return false, err
		}
	}
	return true, nil
}

func desiredProtectedSecret(app *applicationv1.OneKSApplication, resource applicationv1.ProtectedSecretSpec, input *corev1.Secret) (*corev1.Secret, error) {
	data := make(map[string][]byte)
	secretType := corev1.SecretTypeOpaque
	switch resource.BuilderType {
	case applicationv1.ProtectedSecretBuilderBasicAuth:
		secretType = corev1.SecretTypeBasicAuth
		data[corev1.BasicAuthUsernameKey] = []byte(resource.Username)
		data[corev1.BasicAuthPasswordKey] = append([]byte(nil), input.Data[resource.PasswordInputKey]...)
	case applicationv1.ProtectedSecretBuilderOpaque:
		for _, mapping := range resource.OpaqueData {
			data[mapping.Key] = append([]byte(nil), input.Data[mapping.InputKey]...)
		}
	case applicationv1.ProtectedSecretBuilderDockerConfigJSON:
		secretType = corev1.SecretTypeDockerConfigJson
		password := input.Data[resource.PasswordInputKey]
		payload := map[string]any{"auths": map[string]any{
			resource.Registry: map[string]any{
				"username": resource.Username,
				"password": string(password),
				"email":    resource.Email,
				"auth":     base64.StdEncoding.EncodeToString(append([]byte(resource.Username+":"), password...)),
			},
		}}
		rendered, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("build protected Secret %s: Docker authentication JSON encoding failed", resource.ID)
		}
		data[corev1.DockerConfigJsonKey] = rendered
	default:
		return nil, fmt.Errorf("build protected Secret %s: unsupported builder", resource.ID)
	}
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: resource.Namespace,
			Name:      resource.Name,
			Labels:    ownershipLabels(app),
		},
		Type: secretType,
		Data: data,
	}, nil
}

func (r *Reconciler) updateProtectedSecret(ctx context.Context, current, desired *corev1.Secret, resource applicationv1.ProtectedSecretSpec) error {
	if current.Type == desired.Type && reflect.DeepEqual(current.Data, desired.Data) && labelSubsetMatches(current.Labels, desired.Labels) {
		return nil
	}
	updated := current.DeepCopy()
	updated.Type = desired.Type
	updated.Data = desired.DeepCopy().Data
	updated.Labels = maps.Clone(desired.Labels)
	maps.Copy(updated.Labels, current.Labels)
	maps.Copy(updated.Labels, desired.Labels)
	if err := r.Update(ctx, updated); err != nil {
		return &protectedSecretAPIError{operation: "update protected", namespace: resource.Namespace, name: resource.Name, cause: err}
	}
	ctrl.LoggerFrom(ctx).Info(
		"protected Secret updated",
		"resourceID", resource.ID,
		"resourceNamespace", resource.Namespace, "name", resource.Name,
	)
	r.event(updated, corev1.EventTypeNormal, "ProtectedSecretRepaired", fmt.Sprintf("Protected Secret %s/%s repaired", resource.Namespace, resource.Name))
	return nil
}

func expectedProtectedSecret(resource applicationv1.ProtectedSecretSpec) (corev1.SecretType, []string) {
	switch resource.BuilderType {
	case applicationv1.ProtectedSecretBuilderBasicAuth:
		return corev1.SecretTypeBasicAuth, []string{corev1.BasicAuthPasswordKey, corev1.BasicAuthUsernameKey}
	case applicationv1.ProtectedSecretBuilderOpaque:
		keys := make([]string, 0, len(resource.OpaqueData))
		for _, mapping := range resource.OpaqueData {
			keys = append(keys, mapping.Key)
		}
		sort.Strings(keys)
		return corev1.SecretTypeOpaque, keys
	case applicationv1.ProtectedSecretBuilderDockerConfigJSON:
		return corev1.SecretTypeDockerConfigJson, []string{corev1.DockerConfigJsonKey}
	default:
		return "", nil
	}
}

func unavailableProtectedSecrets(app *applicationv1.OneKSApplication, phase, reason, message, resourceMessage string, failed bool) protectedSecretsObservation {
	result := protectedSecretsObservation{componentObservation: componentObservation{failed: failed, reason: reason, message: message}}
	result.statuses = make([]applicationv1.ResourceStatus, 0, len(app.Spec.ProtectedSecrets))
	for _, resource := range app.Spec.ProtectedSecrets {
		result.statuses = append(result.statuses, applicationv1.ResourceStatus{
			ID: resource.ID, Phase: phase, Reason: reason, Message: resourceMessage,
		})
	}
	if len(app.Spec.ProtectedSecrets) != 0 {
		result.current = app.Spec.ProtectedSecrets[0].ID
	}
	return result
}

func (result *protectedSecretsObservation) addStatus(status applicationv1.ResourceStatus) {
	result.statuses = append(result.statuses, status)
	if status.Phase == "Ready" {
		result.completed++
		return
	}
	result.ready = false
	result.failed = result.failed || status.Phase == "Failed"
	if result.current == "" {
		result.current = status.ID
		result.reason = status.Reason
		result.message = status.Message
	}
}

func (r *Reconciler) observeProtectedSecrets(ctx context.Context, app *applicationv1.OneKSApplication, managedResourcesReady bool) (protectedSecretsObservation, error) {
	result := protectedSecretsObservation{componentObservation: componentObservation{ready: true}}
	if !managedResourcesReady {
		return unavailableProtectedSecrets(app, "Pending", "ManagedResourcesPending", "Protected Secrets are gated by managed resources", "Protected Secret is gated by managed resources", false), nil
	}
	_, missing, err := r.readSecretInput(ctx, app)
	if err != nil {
		if _, invalid := err.(*InputSecretValidationError); !invalid {
			return result, err
		}
		return unavailableProtectedSecrets(app, "Failed", "InputSecretInvalid", "Input Secret does not satisfy the compiled contract", "Protected Secret input is invalid", true), nil
	}
	if missing {
		return unavailableProtectedSecrets(app, "Pending", "InputSecretMissing", "Input Secret is absent", "Protected Secret is waiting for input Secret", false), nil
	}

	for _, resource := range app.Spec.ProtectedSecrets {
		status := applicationv1.ResourceStatus{
			ID: resource.ID, Phase: "Pending", Reason: "ProtectedSecretPending",
			Message: "Protected Secret is not ready",
		}
		current := &corev1.Secret{}
		err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, current)
		if apierrors.IsNotFound(err) {
			result.addStatus(status)
			continue
		}
		if err != nil {
			return result, &protectedSecretAPIError{operation: "observe protected", namespace: resource.Namespace, name: resource.Name, cause: err}
		}
		status.ResourceVersion = current.ResourceVersion
		if !ownershipMatches(app, current) {
			status.Phase, status.Reason, status.Message = "Failed", "OwnershipConflict", "Protected Secret does not have exact OneKS ownership"
			result.addStatus(status)
			continue
		}
		expectedType, expectedKeys := expectedProtectedSecret(resource)
		keysReady := len(current.Data) >= len(expectedKeys)
		for _, key := range expectedKeys {
			if _, found := current.Data[key]; !found {
				keysReady = false
			}
		}
		if current.Type == expectedType && keysReady {
			status.Phase, status.Reason, status.Message = "Ready", "ProtectedSecretReady", "Protected Secret is ready"
		}
		result.addStatus(status)
	}
	if result.ready {
		result.reason = "ProtectedSecretsReady"
		result.message = "All protected Secrets are ready"
	}
	return result, nil
}

func (r *Reconciler) reconcileDeleteProtectedSecrets(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	if err := r.preflightProtectedSecretOwnership(ctx, app, true); err != nil {
		return false, err
	}
	pending := false
	for index := len(app.Spec.ProtectedSecrets) - 1; index >= 0; index-- {
		resource := app.Spec.ProtectedSecrets[index]
		if resource.DeletionPolicy == applicationv1.DeletionPolicyRetain {
			continue
		}
		current := &corev1.Secret{}
		err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, current)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, &protectedSecretAPIError{operation: "read deleting protected", namespace: resource.Namespace, name: resource.Name, cause: err}
		}
		pending = true
		if !current.DeletionTimestamp.IsZero() {
			continue
		}
		if !ownershipMatches(app, current) {
			return false, &OwnershipConflictError{Kind: "Secret", Namespace: resource.Namespace, Name: resource.Name}
		}
		deleteErr := r.Delete(ctx, current, deletePreconditions(current)...)
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return false, &protectedSecretAPIError{operation: "delete protected", namespace: resource.Namespace, name: resource.Name, cause: deleteErr}
		}
		if deleteErr == nil {
			ctrl.LoggerFrom(ctx).Info(
				"protected Secret deletion requested",
				"resourceID", resource.ID,
				"resourceNamespace", resource.Namespace, "name", resource.Name,
			)
		}
		r.event(app, corev1.EventTypeNormal, "ProtectedSecretDeleted", fmt.Sprintf("Protected Secret %s/%s deletion requested", resource.Namespace, resource.Name))
	}
	return pending, nil
}

func (r *Reconciler) reconcileDeleteSecretInput(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	reference := app.Spec.SecretInputRef
	current := &corev1.Secret{}
	err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: reference.Namespace, Name: reference.Name}, current)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, &protectedSecretAPIError{operation: "read deleting input", namespace: reference.Namespace, name: reference.Name, cause: err}
	}
	expectedUID := app.Status.SecretInputUID
	if expectedUID == "" || string(current.UID) != expectedUID {
		r.event(app, corev1.EventTypeWarning, "InputSecretReplaced", "Input Secret replacement is not owned by this application and will be retained")
		return false, nil
	}
	if !current.DeletionTimestamp.IsZero() {
		return true, nil
	}
	deleteErr := r.Delete(ctx, current, deletePreconditions(current)...)
	if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
		return false, &protectedSecretAPIError{operation: "delete input", namespace: reference.Namespace, name: reference.Name, cause: deleteErr}
	}
	if deleteErr == nil {
		ctrl.LoggerFrom(ctx).Info(
			"input Secret deletion requested",
			"resourceNamespace", reference.Namespace, "name", reference.Name,
		)
	}
	r.event(app, corev1.EventTypeNormal, "InputSecretDeleted", "Input Secret deletion requested")
	return true, nil
}

func deletingProtectedSecretStatuses(app *applicationv1.OneKSApplication) []applicationv1.ResourceStatus {
	statuses := make([]applicationv1.ResourceStatus, 0, len(app.Spec.ProtectedSecrets))
	for _, resource := range app.Spec.ProtectedSecrets {
		statuses = append(statuses, deletingResourceStatus(resource.ID, resource.DeletionPolicy, "Protected Secret"))
	}
	return statuses
}
