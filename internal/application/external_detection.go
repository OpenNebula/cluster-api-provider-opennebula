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
	"fmt"
	"strings"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	certManagerNamespace         = "cert-manager"
	certManagerComponentSelector = "app.kubernetes.io/component in (controller,webhook)"
	certManagerWebhookName       = "cert-manager-webhook"
)

var certManagerCRDNames = []string{
	"certificates.cert-manager.io",
	"certificaterequests.cert-manager.io",
	"issuers.cert-manager.io",
	"clusterissuers.cert-manager.io",
}

type externalDetectionState string

const (
	externalDetectionUsable   externalDetectionState = "usable"
	externalDetectionAbsent   externalDetectionState = "absent"
	externalDetectionUnusable externalDetectionState = "present-but-unusable"
)

type externalDetectionResult struct {
	state   externalDetectionState
	message string
}

func usesExternalDetection(app *applicationv1.OneKSApplication) bool {
	return app.Spec.Role == applicationv1.ApplicationRoleDependency &&
		app.Spec.ExternalDetection != nil
}

func externalSelection(app *applicationv1.OneKSApplication) (string, error) {
	if !usesExternalDetection(app) {
		return "", nil
	}
	selection := app.GetAnnotations()[ExternalSelectionAnnotation]
	switch selection {
	case "", ExternalSelectionExternal, ExternalSelectionManaged:
		return selection, nil
	default:
		return "", fmt.Errorf("invalid persisted external dependency selection %q", selection)
	}
}

func ownsHelmLifecycle(app *applicationv1.OneKSApplication) bool {
	if !usesExternalDetection(app) {
		return true
	}
	selection, err := externalSelection(app)
	return err == nil && selection == ExternalSelectionManaged
}

func (r *Reconciler) patchExternalSelection(ctx context.Context, app *applicationv1.OneKSApplication, selection string) (*applicationv1.OneKSApplication, error) {
	updated := app.DeepCopy()
	annotations := make(map[string]string, len(app.Annotations)+1)
	for key, value := range app.Annotations {
		annotations[key] = value
	}
	annotations[ExternalSelectionAnnotation] = selection
	updated.Annotations = annotations
	patch := client.MergeFromWithOptions(app, client.MergeFromWithOptimisticLock{})
	if err := r.Patch(ctx, updated, patch); err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *Reconciler) detectExternalDependency(ctx context.Context, app *applicationv1.OneKSApplication) (externalDetectionResult, error) {
	switch app.Spec.ExternalDetection.Detector {
	case applicationv1.ExternalDetectorCertManager:
		return r.detectCertManager(ctx)
	default:
		return externalDetectionResult{}, fmt.Errorf("unsupported external detector %q", app.Spec.ExternalDetection.Detector)
	}
}

func (r *Reconciler) detectCertManager(ctx context.Context) (externalDetectionResult, error) {
	reader := r.authoritativeReader()
	evidence := false
	issues := make([]string, 0)

	for _, name := range certManagerCRDNames {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := reader.Get(ctx, types.NamespacedName{Name: name}, crd)
		if apierrors.IsNotFound(err) {
			issues = append(issues, "CRD "+name+" is absent")
			continue
		}
		if err != nil {
			return externalDetectionResult{}, fmt.Errorf("detect cert-manager CRD %s: %w", name, err)
		}
		evidence = true
		if !crdConditionTrue(crd, apiextensionsv1.Established) {
			issues = append(issues, "CRD "+name+" is not Established")
		}
	}

	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: certManagerNamespace}, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			issues = append(issues, "namespace cert-manager is absent")
		} else {
			return externalDetectionResult{}, fmt.Errorf("detect cert-manager namespace: %w", err)
		}
	}

	deployments := &appsv1.DeploymentList{}
	if err := reader.List(ctx, deployments, client.InNamespace(certManagerNamespace), client.MatchingLabelsSelector{Selector: mustParseSelector(certManagerComponentSelector)}); err != nil {
		return externalDetectionResult{}, fmt.Errorf("detect cert-manager Deployments: %w", err)
	}
	if len(deployments.Items) != 0 {
		evidence = true
	}
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(certManagerNamespace), client.MatchingLabelsSelector{Selector: mustParseSelector(certManagerComponentSelector)}); err != nil {
		return externalDetectionResult{}, fmt.Errorf("detect cert-manager Pods: %w", err)
	}
	if len(pods.Items) != 0 {
		evidence = true
	}
	for _, component := range []string{"controller", "webhook"} {
		componentDeployments := deploymentsForComponent(deployments.Items, component)
		if len(componentDeployments) == 0 {
			issues = append(issues, component+" Deployment is absent")
		} else {
			for i := range componentDeployments {
				desired := int32(1)
				if componentDeployments[i].Spec.Replicas != nil {
					desired = *componentDeployments[i].Spec.Replicas
				}
				if desired < 1 || componentDeployments[i].Status.AvailableReplicas < desired {
					issues = append(issues, component+" Deployment is not ready")
					break
				}
			}
		}
		componentPods := podsForComponent(pods.Items, component)
		if len(componentPods) == 0 {
			issues = append(issues, component+" Pod is absent")
		} else {
			for i := range componentPods {
				if componentPods[i].Status.Phase != corev1.PodRunning || !podConditionTrue(&componentPods[i], corev1.PodReady) {
					issues = append(issues, component+" Pod is not ready")
					break
				}
			}
		}
	}

	service := &corev1.Service{}
	serviceKey := types.NamespacedName{Namespace: certManagerNamespace, Name: certManagerWebhookName}
	if err := reader.Get(ctx, serviceKey, service); err != nil {
		if apierrors.IsNotFound(err) {
			issues = append(issues, "webhook Service is absent")
		} else {
			return externalDetectionResult{}, fmt.Errorf("detect cert-manager webhook Service: %w", err)
		}
	} else {
		evidence = true
	}
	endpoints := &corev1.Endpoints{}
	if err := reader.Get(ctx, serviceKey, endpoints); err != nil {
		if apierrors.IsNotFound(err) {
			issues = append(issues, "webhook Endpoints are absent")
		} else {
			return externalDetectionResult{}, fmt.Errorf("detect cert-manager webhook Endpoints: %w", err)
		}
	} else {
		evidence = true
		if !hasReadyEndpoint(endpoints) {
			issues = append(issues, "webhook Endpoints have no ready addresses")
		}
	}

	if len(issues) == 0 {
		return externalDetectionResult{state: externalDetectionUsable, message: "External cert-manager is usable"}, nil
	}
	if !evidence {
		return externalDetectionResult{state: externalDetectionAbsent, message: "External cert-manager is absent"}, nil
	}
	return externalDetectionResult{state: externalDetectionUnusable, message: strings.Join(issues, "; ")}, nil
}

func crdConditionTrue(crd *apiextensionsv1.CustomResourceDefinition, conditionType apiextensionsv1.CustomResourceDefinitionConditionType) bool {
	for _, condition := range crd.Status.Conditions {
		if condition.Type == conditionType && condition.Status == apiextensionsv1.ConditionTrue {
			return true
		}
	}
	return false
}

func deploymentsForComponent(items []appsv1.Deployment, component string) []appsv1.Deployment {
	result := make([]appsv1.Deployment, 0)
	for i := range items {
		if items[i].Labels["app.kubernetes.io/component"] == component {
			result = append(result, items[i])
		}
	}
	return result
}

func podsForComponent(items []corev1.Pod, component string) []corev1.Pod {
	result := make([]corev1.Pod, 0)
	for i := range items {
		if items[i].Labels["app.kubernetes.io/component"] == component {
			result = append(result, items[i])
		}
	}
	return result
}

func podConditionTrue(pod *corev1.Pod, conditionType corev1.PodConditionType) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func hasReadyEndpoint(endpoints *corev1.Endpoints) bool {
	for _, subset := range endpoints.Subsets {
		if len(subset.Addresses) != 0 {
			return true
		}
	}
	return false
}

func mustParseSelector(value string) labels.Selector {
	selector, err := labels.Parse(value)
	if err != nil {
		panic(err)
	}
	return selector
}
