/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
)

var modelDeploymentGVR = schema.GroupVersionResource{
	Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices",
}

const (
	modelDeploymentNamespace         = "onellm-system"
	modelDeploymentIDLabel           = "oneks.opennebula.io/model-deployment-id"
	modelDeploymentDiscoveryInterval = 30 * time.Second
)

type ModelDeploymentEvent struct {
	Event   string                      `json:"event"`
	Payload ModelDeploymentEventPayload `json:"payload"`
}

type ModelDeploymentEventPayload struct {
	URL             string      `json:"url,omitempty"`
	ErrorMessage    string      `json:"error_msg,omitempty"`
	Reason          string      `json:"reason,omitempty"`
	ResourceVersion json.Number `json:"resource_version"`
}

type pendingModelDeployment struct {
	resource *unstructured.Unstructured
	deleted  bool
}

// ModelDeploymentMonitor converts LLMInferenceService status into OneKS events.
type ModelDeploymentMonitor struct {
	factory   dynamicinformer.DynamicSharedInformerFactory
	resources cache.SharedIndexInformer
	discovery discovery.DiscoveryInterface
	publisher Publisher
	queue     workqueue.TypedRateLimitingInterface[string]

	mu      sync.Mutex
	pending map[string]pendingModelDeployment
	ready   atomic.Bool
}

// NewModelDeploymentMonitor constructs the filtered KServe informer.
func NewModelDeploymentMonitor(
	dynamicClient dynamic.Interface,
	discoveryClient discovery.DiscoveryInterface,
	publisher Publisher,
) (*ModelDeploymentMonitor, error) {
	m := &ModelDeploymentMonitor{
		discovery: discoveryClient,
		publisher: publisher,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		pending: make(map[string]pendingModelDeployment),
	}
	m.factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient, 0, modelDeploymentNamespace, func(options *metav1.ListOptions) {
			options.LabelSelector = modelDeploymentIDLabel
		},
	)
	m.resources = m.factory.ForResource(modelDeploymentGVR).Informer()
	if _, err := m.resources.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { m.enqueue(obj, false) },
		UpdateFunc: func(oldObj, newObj any) {
			if modelDeploymentEventSignature(oldObj) != modelDeploymentEventSignature(newObj) {
				m.enqueue(newObj, false)
			}
		},
		DeleteFunc: func(obj any) { m.enqueue(obj, true) },
	}); err != nil {
		return nil, fmt.Errorf("register LLMInferenceService handler: %w", err)
	}
	return m, nil
}

// Run synchronizes the KServe cache and retries delivery until cancellation.
func (m *ModelDeploymentMonitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	log := ctrl.Log.WithName("model-deployment-monitor")

	// OneLLM is optional. Discovering the API before listing avoids treating an
	// authorization response as proof that a not-yet-installed CRD exists.
	apiAvailable, err := m.apiAvailable()
	if err != nil {
		return fmt.Errorf("discover LLMInferenceService API: %w", err)
	}
	m.ready.Store(true)
	defer m.ready.Store(false)

	if !apiAvailable {
		log.Info("LLMInferenceService API is not installed; waiting for OneLLM")
		ticker := time.NewTicker(modelDeploymentDiscoveryInterval)
		defer ticker.Stop()

		for !apiAvailable {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				apiAvailable, err = m.apiAvailable()
				if err != nil {
					log.Error(err, "unable to discover LLMInferenceService API")
				}
			}
		}
	}

	m.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.resources.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("initial LLMInferenceService informer cache sync failed")
	}
	log.Info("LLMInferenceService cache synchronized")

	go func() {
		<-ctx.Done()
		m.queue.ShutDown()
	}()
	for m.processNext(ctx) {
	}
	return nil
}

func (m *ModelDeploymentMonitor) apiAvailable() (bool, error) {
	resources, err := m.discovery.ServerResourcesForGroupVersion(
		modelDeploymentGVR.GroupVersion().String(),
	)
	if err != nil {
		// Discovery returns a group-version error while the optional CRD is absent.
		// Other discovery failures (for example, an unreachable API server) remain fatal.
		if apierrors.IsNotFound(err) || discovery.IsGroupDiscoveryFailedError(err) {
			return false, nil
		}
		return false, err
	}
	for _, resource := range resources.APIResources {
		if resource.Name == modelDeploymentGVR.Resource {
			return true, nil
		}
	}
	return false, nil
}

// Ready reports whether initial KServe access succeeded or its optional API is absent.
func (m *ModelDeploymentMonitor) Ready() bool { return m.ready.Load() }

func (m *ModelDeploymentMonitor) enqueue(obj any, deleted bool) {
	resource, ok := modelDeploymentFromEvent(obj)
	if !ok {
		return
	}
	key := resource.GetNamespace() + "/" + resource.GetName()
	m.mu.Lock()
	m.pending[key] = pendingModelDeployment{resource: resource.DeepCopy(), deleted: deleted}
	m.mu.Unlock()
	m.queue.Add(key)
}

func (m *ModelDeploymentMonitor) processNext(ctx context.Context) bool {
	key, shutdown := m.queue.Get()
	if shutdown {
		return false
	}
	defer m.queue.Done(key)

	m.mu.Lock()
	pending, exists := m.pending[key]
	m.mu.Unlock()
	if !exists {
		m.queue.Forget(key)
		return true
	}

	deploymentID, event, err := modelDeploymentEvent(pending.resource, pending.deleted)
	if err != nil {
		ctrl.LoggerFrom(ctx).WithName("model-deployment-monitor").Error(
			err, "invalid LLMInferenceService", "resource", key,
		)
		m.forgetPending(key, pending)
		m.queue.Forget(key)
		return true
	}
	if event.Event == "" {
		m.forgetPending(key, pending)
		m.queue.Forget(key)
		return true
	}
	log := ctrl.LoggerFrom(ctx).WithName("model-deployment-monitor").WithValues(
		"resource", key, "deploymentID", deploymentID, "event", event.Event,
	)
	if err := m.publisher.PublishModelDeploymentEvent(ctx, deploymentID, event); err != nil {
		log.Error(err, "unable to send model deployment event")
		m.queue.AddRateLimited(key)
		return true
	}
	log.Info("model deployment event sent")
	m.forgetPending(key, pending)
	m.queue.Forget(key)
	return true
}

func (m *ModelDeploymentMonitor) forgetPending(key string, processed pendingModelDeployment) {
	// Preserve an update that arrived while the processed event was in flight.
	m.mu.Lock()
	defer m.mu.Unlock()
	current, found := m.pending[key]
	if found && current.deleted == processed.deleted &&
		current.resource.GetResourceVersion() == processed.resource.GetResourceVersion() {
		delete(m.pending, key)
	}
}

func modelDeploymentFromEvent(obj any) (*unstructured.Unstructured, bool) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	resource, ok := obj.(*unstructured.Unstructured)
	if !ok || resource.GetLabels()[modelDeploymentIDLabel] == "" {
		return nil, false
	}
	return resource, true
}

func modelDeploymentEvent(
	resource *unstructured.Unstructured,
	deleted bool,
) (int, ModelDeploymentEvent, error) {
	id, err := strconv.Atoi(resource.GetLabels()[modelDeploymentIDLabel])
	if err != nil || id < 0 {
		return 0, ModelDeploymentEvent{}, fmt.Errorf(
			"label %s must be a non-negative integer", modelDeploymentIDLabel,
		)
	}
	version := resource.GetResourceVersion()
	if !decimalNumber(version) {
		return 0, ModelDeploymentEvent{}, fmt.Errorf(
			"resourceVersion must be a non-negative integer: %q", version,
		)
	}
	event := ModelDeploymentEvent{
		Payload: ModelDeploymentEventPayload{ResourceVersion: json.Number(version)},
	}
	if deleted {
		event.Event = "resources_deleted"
		return id, event, nil
	}

	ready := kserveReadyCondition(resource)
	if ready == nil {
		return id, ModelDeploymentEvent{}, nil
	}
	switch stringValue(ready["status"]) {
	case "True":
		url, _, err := unstructured.NestedString(resource.Object, "status", "url")
		if err != nil || strings.TrimSpace(url) == "" {
			return id, ModelDeploymentEvent{}, err
		}
		event.Event = "inference_ready"
		event.Payload.URL = url
	case "False":
		reason := strings.TrimSpace(stringValue(ready["reason"]))
		if !strings.HasSuffix(reason, "Failed") {
			return id, ModelDeploymentEvent{}, nil
		}
		event.Event = "deployment_failed"
		event.Payload.Reason = reason
		event.Payload.ErrorMessage = strings.TrimSpace(stringValue(ready["message"]))
		if event.Payload.ErrorMessage == "" {
			event.Payload.ErrorMessage = reason
		}
	default:
		return id, ModelDeploymentEvent{}, nil
	}
	return id, event, nil
}

func kserveReadyCondition(resource *unstructured.Unstructured) map[string]any {
	conditions, _, _ := unstructured.NestedSlice(resource.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && stringValue(condition["type"]) == "Ready" {
			return condition
		}
	}
	return nil
}

func modelDeploymentEventSignature(obj any) string {
	resource, ok := modelDeploymentFromEvent(obj)
	if !ok {
		return ""
	}
	url, _, _ := unstructured.NestedString(resource.Object, "status", "url")
	ready := kserveReadyCondition(resource)
	var status, reason, message string
	if ready != nil {
		status = stringValue(ready["status"])
		reason = stringValue(ready["reason"])
		message = stringValue(ready["message"])
	}
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s", url, status, reason, message)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
