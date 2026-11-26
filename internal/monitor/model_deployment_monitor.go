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
	"net/http"
	"slices"
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
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
)

// modelDeploymentGVR identifies KServe LLMInferenceService resources.
var modelDeploymentGVR = schema.GroupVersionResource{
	Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices",
}

const (
	// modelDeploymentIDLabel maps a Kubernetes resource to OneKS.
	modelDeploymentIDLabel = "oneks.opennebula.io/model-deployment-id"
	// OneKS installs this finalizer atomically when it creates the resource; this
	// monitor owns its removal after OneKS acknowledges resources_deleted. Keeping
	// the terminating object in Kubernetes makes deletion recovery survive restarts.
	modelDeploymentFinalizer         = "oneks.opennebula.io/model-deployment-monitor"
	modelDeploymentDiscoveryInterval = 30 * time.Second
)

type pendingModelDeployment struct {
	// Informer objects are read-only, so the queue stores a copy.
	resource *unstructured.Unstructured
	deleted  bool
	// releaseFinalizer removes our barrier without reporting a Kubernetes delete,
	// for example when an existing resource loses its OneKS ownership label.
	releaseFinalizer bool
}

// ModelDeploymentMonitor converts LLMInferenceService status into OneKS events.
// It coalesces updates and delivers the latest state when OneKS is unavailable.
// Deletion callbacks have at-least-once delivery: the monitor removes its
// finalizer only after a successful callback, so a retry may repeat an event.
type ModelDeploymentMonitor struct {
	resources   cache.SharedIndexInformer
	discovery   discovery.DiscoveryInterface
	deployments dynamic.NamespaceableResourceInterface
	publisher   Publisher
	queue       workqueue.TypedRateLimitingInterface[string]

	mu                sync.Mutex
	pending           map[string]pendingModelDeployment
	ready             atomic.Bool
	discoveryInterval time.Duration
}

// NewModelDeploymentMonitor constructs the KServe informer.
func NewModelDeploymentMonitor(
	dynamicClient dynamic.Interface,
	discoveryClient discovery.DiscoveryInterface,
	publisher Publisher,
) (*ModelDeploymentMonitor, error) {
	m := &ModelDeploymentMonitor{
		discovery:         discoveryClient,
		deployments:       dynamicClient.Resource(modelDeploymentGVR),
		publisher:         publisher,
		discoveryInterval: modelDeploymentDiscoveryInterval,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		pending: make(map[string]pendingModelDeployment),
	}
	// Watch the optional KServe CRD without importing its Go types. Filtering is
	// deliberately local: a server-side label selector reports label removal as
	// a synthetic Delete event even though the Kubernetes object still exists.
	m.resources = dynamicinformer.NewFilteredDynamicInformer(
		dynamicClient, modelDeploymentGVR, metav1.NamespaceAll, 0, nil,
		nil,
	).Informer()
	if _, err := m.resources.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// Add also receives resources found by the informer's initial list.
		AddFunc:    m.onAdd,
		UpdateFunc: m.onUpdate,
		DeleteFunc: m.onDelete,
	}); err != nil {
		return nil, fmt.Errorf("register LLMInferenceService handler: %w", err)
	}
	return m, nil
}

func (m *ModelDeploymentMonitor) onAdd(obj any) {
	resource, ok := modelDeploymentFromEvent(obj)
	if !ok {
		return
	}
	if modelDeploymentManaged(resource) {
		m.enqueue(resource, false)
	} else if slices.Contains(resource.GetFinalizers(), modelDeploymentFinalizer) {
		m.enqueueFinalizerRelease(resource)
	}
}

func (m *ModelDeploymentMonitor) onUpdate(oldObj, newObj any) {
	newResource, ok := modelDeploymentFromEvent(newObj)
	if !ok {
		return
	}
	if !modelDeploymentManaged(newResource) {
		// Losing the ownership label is not a Kubernetes deletion. Release our
		// barrier without emitting resources_deleted for an object that exists.
		if slices.Contains(newResource.GetFinalizers(), modelDeploymentFinalizer) {
			m.enqueueFinalizerRelease(newResource)
		}
		return
	}
	// Ignore updates that cannot alter the OneKS callback.
	if modelDeploymentEventSignature(oldObj) != modelDeploymentEventSignature(newObj) {
		m.enqueue(newResource, false)
	}
}

func (m *ModelDeploymentMonitor) onDelete(obj any) {
	resource, ok := modelDeploymentFromEvent(obj)
	if !ok || !modelDeploymentManaged(resource) {
		return
	}
	// Physical deletion may repeat a callback already sent from the terminating
	// update. Delivery is intentionally at-least-once, and this also covers
	// legacy resources that were deleted before they acquired the finalizer.
	m.enqueue(resource, true)
}

// Run synchronizes the KServe cache and retries delivery until cancellation.
func (m *ModelDeploymentMonitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	defer m.queue.ShutDown()
	log := ctrl.Log.WithName("model-deployment-monitor")
	// OneLLM is optional. Discovering the API before listing avoids treating an
	// authorization response as proof that a not-yet-installed CRD exists.
	defer m.ready.Store(false)
	interval := m.discoveryInterval
	if interval <= 0 {
		interval = modelDeploymentDiscoveryInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	waitingForAPI := false
	for {
		apiAvailable, err := m.apiAvailable()
		if err == nil && apiAvailable {
			break
		}
		if err != nil {
			m.ready.Store(false)
			log.Error(err, "unable to discover LLMInferenceService API; retrying")
		} else {
			// OneLLM is optional; a confirmed absence must not block readiness.
			m.ready.Store(true)
			if !waitingForAPI {
				log.Info("LLMInferenceService API is not installed; waiting for OneLLM")
				waitingForAPI = true
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
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
		// The caller retries other discovery failures without stopping the manager.
		if apierrors.IsNotFound(err) {
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
	if !ok || !modelDeploymentManaged(resource) {
		return
	}
	key := resource.GetNamespace() + "/" + resource.GetName()
	// Keep only the latest object for each namespace/name.
	m.mu.Lock()
	// A terminating object is the durable equivalent of an informer Delete
	// event and remains visible across restarts when OneKS created it protected.
	m.pending[key] = pendingModelDeployment{
		resource: resource.DeepCopy(), deleted: deleted || resource.GetDeletionTimestamp() != nil,
	}
	m.mu.Unlock()
	m.queue.Add(key)
}

func (m *ModelDeploymentMonitor) enqueueFinalizerRelease(resource *unstructured.Unstructured) {
	key := resource.GetNamespace() + "/" + resource.GetName()
	m.mu.Lock()
	m.pending[key] = pendingModelDeployment{
		resource: resource.DeepCopy(), releaseFinalizer: true,
	}
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
	queued := pending
	if pending.releaseFinalizer {
		if err := m.removeFinalizer(ctx, pending.resource); err != nil {
			ctrl.LoggerFrom(ctx).WithName("model-deployment-monitor").Error(
				err, "unable to release unmanaged LLMInferenceService", "resource", key,
			)
			m.queue.AddRateLimited(key)
			return true
		}
		m.forgetPending(key, queued)
		m.queue.Forget(key)
		return true
	}
	deploymentID, event, err := modelDeploymentEvent(pending.resource, pending.deleted)
	if err != nil {
		ctrl.LoggerFrom(ctx).WithName("model-deployment-monitor").Error(
			err, "invalid LLMInferenceService", "resource", key,
		)
		// Never strand an invalid object behind a finalizer that cannot produce
		// a valid OneKS callback.
		if slices.Contains(pending.resource.GetFinalizers(), modelDeploymentFinalizer) {
			if releaseErr := m.removeFinalizer(ctx, pending.resource); releaseErr != nil {
				m.queue.AddRateLimited(key)
				return true
			}
		}
		m.forgetPending(key, queued)
		m.queue.Forget(key)
		return true
	}

	if event.Event == "" {
		if failure := modelDeploymentFailureCondition(pending.resource); failure != nil {
			if delay := modelDeploymentFailureRetryAfter(
				pending.resource, failure, time.Now(),
			); delay > 0 {
				// No informer update is guaranteed when the grace period expires.
				// Keep the latest object pending and re-evaluate it at the deadline.
				m.queue.AddAfter(key, delay)
				return true
			}
		}
		m.forgetPending(key, queued)
		m.queue.Forget(key)
		return true
	}
	log := ctrl.LoggerFrom(ctx).WithName("model-deployment-monitor").WithValues(
		"resource", key, "deploymentID", deploymentID, "event", event.Event,
	)
	if err := m.publisher.PublishModelDeploymentEvent(ctx, deploymentID, event); err != nil {
		status, hasStatus := endpointStatusCode(err)
		alreadyDeleted := pending.deleted && hasStatus &&
			(status == http.StatusNotFound || status == http.StatusGone)
		if !alreadyDeleted {
			log.Error(err, "unable to send model deployment event; event remains pending")
			// Even 4xx responses can recover after credentials or OneKS are fixed.
			// An operator can explicitly remove our finalizer and recover OneKS when
			// choosing availability over delivery of resources_deleted.
			m.queue.AddRateLimited(key)
			return true
		}
		// OneKS may have removed the deployment after accepting an earlier
		// resources_deleted retry. A missing record is therefore an acknowledgement.
		log.Info("model deployment was already removed from OneKS")
	}
	if pending.deleted && slices.Contains(pending.resource.GetFinalizers(), modelDeploymentFinalizer) {
		// Releasing the finalizer lets the API server complete the already-pending
		// deletion; the monitor does not issue a second DELETE request.
		if err := m.removeFinalizer(ctx, pending.resource); err != nil {
			log.Error(err, "unable to release deleted LLMInferenceService")
			m.queue.AddRateLimited(key)
			return true
		}
	}
	log.Info("model deployment event sent")
	m.forgetPending(key, queued)
	m.queue.Forget(key)
	return true
}

func (m *ModelDeploymentMonitor) removeFinalizer(
	ctx context.Context,
	resource *unstructured.Unstructured,
) error {
	if m.deployments == nil {
		return fmt.Errorf("dynamic LLMInferenceService client is not configured")
	}
	client := m.deployments.Namespace(resource.GetNamespace())
	// Read before every update so concurrent controllers' finalizers and object
	// changes are preserved; RetryOnConflict repeats the read on stale versions.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := client.Get(ctx, resource.GetName(), metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		finalizers := current.GetFinalizers()
		if !slices.Contains(finalizers, modelDeploymentFinalizer) {
			return nil
		}
		finalizers = slices.DeleteFunc(finalizers, func(finalizer string) bool {
			return finalizer == modelDeploymentFinalizer
		})
		current.SetFinalizers(finalizers)
		_, err = client.Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}

func (m *ModelDeploymentMonitor) forgetPending(key string, processed pendingModelDeployment) {
	// Preserve an update that arrived while the processed event was in flight.
	m.mu.Lock()
	defer m.mu.Unlock()
	current, found := m.pending[key]
	if found && current.deleted == processed.deleted &&
		current.releaseFinalizer == processed.releaseFinalizer &&
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
	if !ok {
		return nil, false
	}
	return resource, true
}

func modelDeploymentManaged(resource *unstructured.Unstructured) bool {
	return resource.GetLabels()[modelDeploymentIDLabel] != ""
}
