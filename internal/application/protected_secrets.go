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
	"reflect"
	"sort"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	maxProtectedOpaqueData = 16
	maxProtectedUsername   = 1024
	maxProtectedRegistry   = 2048
	maxProtectedEmail      = 320
	maxSecretInputUID      = 128
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
	statuses  []applicationv1.ResourceStatus
	ready     bool
	failed    bool
	completed int32
	current   string
	reason    string
	message   string
}

func usesProtectedSecrets(app *applicationv1.OneKSApplication) bool {
	return (app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha4 || app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha5) && app.Spec.Role == applicationv1.ApplicationRoleRoot
}

func validateProtectedSecretContract(spec applicationv1.OneKSApplicationSpec) *PlanError {
	if spec.SecretInputRef == nil {
		return invalid("MissingSecretInputRef", "%s requires secretInputRef", spec.PlanVersion)
	}
	input := spec.SecretInputRef
	if input.Namespace != applicationv1.ApplicationNamespace {
		return invalid("InvalidSecretInputNamespace", "secretInputRef.namespace must be %q", applicationv1.ApplicationNamespace)
	}
	if !validUTF8Bytes(input.Name, 1, 253) || len(validation.IsDNS1123Subdomain(input.Name)) != 0 {
		return invalid("InvalidSecretInputName", "secretInputRef.name must be a DNS-1123 subdomain")
	}
	if spec.PlanVersion == applicationv1.PlanVersionV1Alpha4 && !validUTF8Bytes(input.UID, 1, maxSecretInputUID) {
		return invalid("InvalidSecretInputUID", "plan-v1alpha4 secretInputRef.uid must be valid UTF-8 between 1 and %d bytes", maxSecretInputUID)
	}
	if spec.PlanVersion == applicationv1.PlanVersionV1Alpha5 && input.UID != "" {
		return invalid("InvalidSecretInputUID", "plan-v1alpha5 binds the input Secret UID through status")
	}
	if len(spec.ProtectedSecrets) == 0 {
		return invalid("MissingProtectedSecrets", "plan-v1alpha4 requires at least one protected Secret")
	}
	if len(spec.ProtectedSecrets) > maxProtectedSecrets {
		return invalid("TooManyProtectedSecrets", "protectedSecrets exceeds %d entries", maxProtectedSecrets)
	}
	if len(spec.ManagedResources)+len(spec.ProtectedSecrets) > maxStatusResources {
		return invalid("TooManyCombinedResources", "managedResources and protectedSecrets exceed %d combined entries", maxStatusResources)
	}

	ids := make(map[string]struct{}, len(spec.ProtectedSecrets))
	managedIDs := make(map[string]struct{}, len(spec.ManagedResources))
	for _, resource := range spec.ManagedResources {
		managedIDs[resource.ID] = struct{}{}
	}
	identities := make(map[string]struct{}, len(spec.ProtectedSecrets))
	for index, secret := range spec.ProtectedSecrets {
		path := fmt.Sprintf("protectedSecrets[%d]", index)
		if !validUTF8Bytes(secret.ID, 1, 63) || len(validation.IsValidLabelValue(secret.ID)) != 0 {
			return invalid("InvalidProtectedSecretID", "%s.id must be a Kubernetes label value", path)
		}
		if _, duplicate := ids[secret.ID]; duplicate {
			return invalid("DuplicateProtectedSecretID", "protected Secret id %q is duplicated", secret.ID)
		}
		if _, collision := managedIDs[secret.ID]; collision {
			return invalid("ProtectedSecretManagedResourceIDCollision", "protected Secret id %q collides with a managed resource id", secret.ID)
		}
		ids[secret.ID] = struct{}{}
		if !validNamespace(secret.Namespace) {
			return invalid("InvalidProtectedSecretNamespace", "%s.namespace must be a DNS-1123 label", path)
		}
		if !validUTF8Bytes(secret.Name, 1, 253) || len(validation.IsDNS1123Subdomain(secret.Name)) != 0 {
			return invalid("InvalidProtectedSecretName", "%s.name must be a DNS-1123 subdomain", path)
		}
		identity := secret.Namespace + "\x00" + secret.Name
		if _, duplicate := identities[identity]; duplicate {
			return invalid("DuplicateProtectedSecretIdentity", "%s duplicates protected Secret identity %s/%s", path, secret.Namespace, secret.Name)
		}
		identities[identity] = struct{}{}
		if secret.Namespace == input.Namespace && secret.Name == input.Name {
			return invalid("ProtectedSecretInputIdentityCollision", "%s must not target secretInputRef", path)
		}
		if !validDeletionPolicy(secret.DeletionPolicy) {
			return invalid("InvalidDeletionPolicy", "%s.deletionPolicy must be Delete or Retain", path)
		}
		if err := validateProtectedSecretBuilder(secret, path); err != nil {
			return err
		}
	}
	return nil
}

func validateProtectedSecretBuilder(secret applicationv1.ProtectedSecretSpec, path string) *PlanError {
	validInputKey := func(value string) bool {
		return validUTF8Bytes(value, 1, 253) && len(validation.IsConfigMapKey(value)) == 0
	}
	validText := func(value string, maximum int) bool {
		return validUTF8Bytes(value, 1, maximum) && !placeholderPattern.MatchString(value)
	}
	switch secret.BuilderType {
	case applicationv1.ProtectedSecretBuilderBasicAuth:
		if !validText(secret.Username, maxProtectedUsername) || !validInputKey(secret.PasswordInputKey) || secret.OpaqueData != nil || secret.Registry != "" || secret.Email != "" {
			return invalid("InvalidBasicAuthSecret", "%s basicAuthSecret requires only bounded username and passwordInputKey fields", path)
		}
	case applicationv1.ProtectedSecretBuilderOpaque:
		if secret.Username != "" || secret.PasswordInputKey != "" || secret.Registry != "" || secret.Email != "" || len(secret.OpaqueData) == 0 || len(secret.OpaqueData) > maxProtectedOpaqueData {
			return invalid("InvalidOpaqueSecret", "%s opaqueSecret requires between 1 and %d opaqueData entries only", path, maxProtectedOpaqueData)
		}
		keys := make(map[string]struct{}, len(secret.OpaqueData))
		for index, mapping := range secret.OpaqueData {
			if !validInputKey(mapping.Key) || !validInputKey(mapping.InputKey) {
				return invalid("InvalidOpaqueSecretData", "%s.opaqueData[%d] contains an invalid Secret data key", path, index)
			}
			if _, duplicate := keys[mapping.Key]; duplicate {
				return invalid("DuplicateOpaqueSecretDataKey", "%s.opaqueData target key %q is duplicated", path, mapping.Key)
			}
			keys[mapping.Key] = struct{}{}
		}
	case applicationv1.ProtectedSecretBuilderDockerConfigJSON:
		if !validText(secret.Registry, maxProtectedRegistry) || !validText(secret.Username, maxProtectedUsername) || !validInputKey(secret.PasswordInputKey) || !validText(secret.Email, maxProtectedEmail) || secret.OpaqueData != nil {
			return invalid("InvalidDockerConfigJsonSecret", "%s dockerConfigJsonSecret requires only bounded registry, username, passwordInputKey, and email fields", path)
		}
	default:
		return invalid("UnsupportedProtectedSecretBuilder", "%s.builderType is not supported", path)
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
	expectedUID := reference.UID
	if app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha5 {
		expectedUID = app.Status.SecretInputUID
		labels := input.GetLabels()
		expectedLabels := map[string]string{
			LabelRootManagedBy: RootManagedByValue, LabelProducer: ProducerValue,
			LabelClusterID: app.Spec.ClusterID, LabelCatalogueChartID: app.Spec.CatalogueChartID,
			LabelPlanDigest: app.Spec.PlanDigest, LabelApplicationName: app.Name,
		}
		for key, expected := range expectedLabels {
			if labels[key] != expected {
				return nil, false, &InputSecretValidationError{Message: "input Secret labels do not match the compiled application"}
			}
		}
	}
	if expectedUID != "" && string(input.UID) != expectedUID {
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

func (r *Reconciler) bindV1Alpha5SecretInput(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
	if app.Spec.PlanVersion != applicationv1.PlanVersionV1Alpha5 || app.Status.SecretInputUID != "" {
		return false, nil
	}
	input, missing, err := r.readSecretInput(ctx, app)
	if err != nil {
		return false, err
	}
	if missing {
		return true, nil
	}
	status := baseStatus(app, r.ControllerVersion)
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

func (r *Reconciler) reconcileProtectedSecrets(ctx context.Context, app *applicationv1.OneKSApplication) (bool, error) {
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
			ctrl.LoggerFrom(ctx).V(1).Info(
				"reconciling protected Secret",
				"action", "create", "resourceID", resource.ID,
				"resourceNamespace", resource.Namespace, "name", resource.Name,
			)
			if createErr := r.Create(ctx, desired); createErr != nil {
				if !apierrors.IsAlreadyExists(createErr) {
					return false, &protectedSecretAPIError{operation: "create protected", namespace: resource.Namespace, name: resource.Name, cause: createErr}
				}
				current = &corev1.Secret{}
				if rereadErr := reader.Get(ctx, client.ObjectKeyFromObject(desired), current); rereadErr != nil {
					if apierrors.IsNotFound(rereadErr) {
						return false, nil
					}
					return false, &protectedSecretAPIError{operation: "re-read protected", namespace: resource.Namespace, name: resource.Name, cause: rereadErr}
				}
				if !ownershipMatches(app, current) {
					return false, &OwnershipConflictError{Kind: "Secret", Namespace: resource.Namespace, Name: resource.Name}
				}
				if err := r.updateProtectedSecret(ctx, current, desired, resource); err != nil {
					return false, err
				}
				continue
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
	observed, err := r.observeProtectedSecrets(ctx, app, true)
	return err == nil && observed.ready, err
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
	if !protectedSecretNeedsUpdate(current, desired) {
		return nil
	}
	updated := current.DeepCopy()
	updated.Type = desired.Type
	updated.Data = copySecretData(desired.Data)
	labels := updated.Labels
	if labels == nil {
		labels = make(map[string]string)
	}
	for key, value := range desired.Labels {
		labels[key] = value
	}
	updated.Labels = labels
	ctrl.LoggerFrom(ctx).V(1).Info(
		"reconciling protected Secret",
		"action", "update", "resourceID", resource.ID,
		"resourceNamespace", resource.Namespace, "name", resource.Name,
	)
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

func protectedSecretNeedsUpdate(current, desired *corev1.Secret) bool {
	return current.Type != desired.Type || !reflect.DeepEqual(current.Data, desired.Data) || !ownedLabelsEqual(current.Labels, desired.Labels)
}

func copySecretData(source map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(source))
	for key, value := range source {
		result[key] = append([]byte(nil), value...)
	}
	return result
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

func (r *Reconciler) observeProtectedSecrets(ctx context.Context, app *applicationv1.OneKSApplication, managedResourcesReady bool) (protectedSecretsObservation, error) {
	result := protectedSecretsObservation{ready: true}
	if !managedResourcesReady {
		result.ready = false
		result.reason = "ManagedResourcesPending"
		result.message = "Protected Secrets are gated by managed resources"
		for _, resource := range app.Spec.ProtectedSecrets {
			result.statuses = append(result.statuses, applicationv1.ResourceStatus{
				ID: resource.ID, Phase: "Pending", Reason: result.reason,
				Message: "Protected Secret is gated by managed resources",
			})
		}
		if len(app.Spec.ProtectedSecrets) != 0 {
			result.current = app.Spec.ProtectedSecrets[0].ID
		}
		return result, nil
	}
	_, missing, err := r.readSecretInput(ctx, app)
	if err != nil {
		if _, invalid := err.(*InputSecretValidationError); !invalid {
			return result, err
		}
		result.ready = false
		result.failed = true
		result.reason = "InputSecretInvalid"
		result.message = "Input Secret does not satisfy the compiled contract"
		for _, resource := range app.Spec.ProtectedSecrets {
			result.statuses = append(result.statuses, applicationv1.ResourceStatus{
				ID: resource.ID, Phase: "Failed", Reason: result.reason,
				Message: "Protected Secret input is invalid",
			})
		}
		if len(app.Spec.ProtectedSecrets) != 0 {
			result.current = app.Spec.ProtectedSecrets[0].ID
		}
		return result, nil
	}
	if missing {
		result.ready = false
		result.reason = "InputSecretMissing"
		result.message = "Input Secret is absent"
		for _, resource := range app.Spec.ProtectedSecrets {
			result.statuses = append(result.statuses, applicationv1.ResourceStatus{
				ID: resource.ID, Phase: "Pending", Reason: result.reason,
				Message: "Protected Secret is waiting for input Secret",
			})
		}
		if len(app.Spec.ProtectedSecrets) != 0 {
			result.current = app.Spec.ProtectedSecrets[0].ID
		}
		return result, nil
	}

	for _, resource := range app.Spec.ProtectedSecrets {
		status := applicationv1.ResourceStatus{
			ID: resource.ID, Phase: "Pending", Reason: "ProtectedSecretPending",
			Message: "Protected Secret is not ready",
		}
		current := &corev1.Secret{}
		err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, current)
		if apierrors.IsNotFound(err) {
			result.ready = false
			if result.current == "" {
				result.current = resource.ID
				result.reason = status.Reason
				result.message = status.Message
			}
			result.statuses = append(result.statuses, status)
			continue
		}
		if err != nil {
			return result, &protectedSecretAPIError{operation: "observe protected", namespace: resource.Namespace, name: resource.Name, cause: err}
		}
		status.ResourceVersion = current.ResourceVersion
		if !ownershipMatches(app, current) {
			status.Phase = "Failed"
			status.Reason = "OwnershipConflict"
			status.Message = "Protected Secret does not have exact OneKS ownership"
			result.ready = false
			result.failed = true
			if result.current == "" {
				result.current = resource.ID
				result.reason = status.Reason
				result.message = status.Message
			}
			result.statuses = append(result.statuses, status)
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
			status.Phase = "Ready"
			status.Reason = "ProtectedSecretReady"
			status.Message = "Protected Secret is ready"
			result.completed++
		} else {
			result.ready = false
			if result.current == "" {
				result.current = resource.ID
				result.reason = status.Reason
				result.message = status.Message
			}
		}
		result.statuses = append(result.statuses, status)
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
		ctrl.LoggerFrom(ctx).V(1).Info(
			"deleting protected Secret",
			"resourceID", resource.ID,
			"resourceNamespace", resource.Namespace, "name", resource.Name,
		)
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
	expectedUID := reference.UID
	if app.Spec.PlanVersion == applicationv1.PlanVersionV1Alpha5 {
		expectedUID = app.Status.SecretInputUID
	}
	if expectedUID == "" || string(current.UID) != expectedUID {
		r.event(app, corev1.EventTypeWarning, "InputSecretReplaced", "Input Secret replacement is not owned by this application and will be retained")
		return false, nil
	}
	if !current.DeletionTimestamp.IsZero() {
		return true, nil
	}
	ctrl.LoggerFrom(ctx).V(1).Info(
		"deleting input Secret",
		"resourceNamespace", reference.Namespace, "name", reference.Name,
	)
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
		status := applicationv1.ResourceStatus{
			ID: resource.ID, Phase: "Deleting", Reason: "DeletionInProgress",
			Message: "Protected Secret deletion is in progress",
		}
		if resource.DeletionPolicy == applicationv1.DeletionPolicyRetain {
			status.Phase = "Retained"
			status.Reason = "RetainPolicy"
			status.Message = "Protected Secret is retained by policy"
		}
		statuses = append(statuses, status)
	}
	return statuses
}
