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
	"fmt"
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

// modelDeploymentGVR identifies KServe LLMInferenceService resources.
var modelDeploymentGVR = schema.GroupVersionResource{
	Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices",
}

const (
	// modelDeploymentIDLabel maps a Kubernetes resource to OneKS.
	modelDeploymentIDLabel           = "oneks.opennebula.io/model-deployment-id"
	modelDeploymentDiscoveryInterval = 30 * time.Second
)

type pendingModelDeployment struct {
	// Informer objects are read-only, so the queue stores a copy.
	resource *unstructured.Unstructured
	deleted  bool
}

// ModelDeploymentMonitor converts LLMInferenceService status into OneKS events.
// It coalesces updates and delivers the latest state when OneKS is unavailable.
type ModelDeploymentMonitor struct {
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
	// Watch the optional KServe CRD without importing its Go types.
	m.resources = dynamicinformer.NewFilteredDynamicInformer(
		dynamicClient, modelDeploymentGVR, metav1.NamespaceAll, 0, nil,
		func(options *metav1.ListOptions) {
			options.LabelSelector = modelDeploymentIDLabel
		},
	).Informer()
	if _, err := m.resources.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// Add also receives resources found by the informer's initial list.
		AddFunc: func(obj any) { m.enqueue(obj, false) },
		UpdateFunc: func(oldObj, newObj any) {
			// Ignore updates that cannot alter the OneKS callback.
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
	defer m.queue.ShutDown()
	log := ctrl.Log.WithName("model-deployment-monitor")
	// OneLLM is optional. Discovering the API before listing avoids treating an
	// authorization response as proof that a not-yet-installed CRD exists.
	apiAvailable, err := m.apiAvailable()
	if err != nil {
		return fmt.Errorf("discover LLMInferenceService API: %w", err)
	}
	defer m.ready.Store(false)

	if !apiAvailable {
		// OneLLM is optional; an absent CRD must not block monitor readiness.
		m.ready.Store(true)
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

	// Once the API is present, readiness requires a synchronized informer cache.
	m.ready.Store(false)
	go m.resources.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.resources.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("initial LLMInferenceService informer cache sync failed")
	}
	m.ready.Store(true)
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
	// Kubernetes discovery reports which API groups and resources are installed.
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
	// Keep only the latest object for each namespace/name.
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
	}
	if err != nil || event.Event == "" {
		m.forgetPending(key, pending)
		m.queue.Forget(key)
		return true
	}
	log := ctrl.LoggerFrom(ctx).WithName("model-deployment-monitor").WithValues(
		"resource", key, "deploymentID", deploymentID, "event", event.Event,
	)
	if err := m.publisher.PublishModelDeploymentEvent(ctx, deploymentID, event); err != nil {
		log.Error(err, "unable to send model deployment event")
		// AddRateLimited retries transient HTTP failures with client-go backoff.
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
	// A tombstone represents an object already missing from the informer cache.
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	resource, ok := obj.(*unstructured.Unstructured)
	if !ok || resource.GetLabels()[modelDeploymentIDLabel] == "" {
		return nil, false
	}
	return resource, true
}
