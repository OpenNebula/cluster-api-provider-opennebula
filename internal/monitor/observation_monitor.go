/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

type ResourceObservation struct {
	Resource  string `json:"resource"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Value     any    `json:"value"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

type ObservationSnapshot []ResourceObservation

type ObservationMonitor struct {
	client        kubernetes.Interface
	dynamicClient dynamic.Interface
	configMaps    corev1client.ConfigMapInterface
	configName    string
	sender        Sender
	destination   ClusterObservationsDestination
	interval      time.Duration

	active         resourceConfig
	activeDocument string
	ready          atomic.Bool
}

func NewObservationMonitor(
	client kubernetes.Interface,
	dynamicClient dynamic.Interface,
	sender Sender,
	config Config,
) *ObservationMonitor {
	return newObservationMonitor(client, dynamicClient, sender, config)
}

func newObservationMonitor(
	client kubernetes.Interface,
	dynamicClient dynamic.Interface,
	sender Sender,
	config Config,
) *ObservationMonitor {
	return &ObservationMonitor{
		client:        client,
		dynamicClient: dynamicClient,
		configMaps:    client.CoreV1().ConfigMaps(config.ResourceConfigNamespace),
		configName:    config.ResourceConfigName,
		sender:        sender,
		destination:   ClusterObservationsDestination{ClusterID: config.ClusterID},
		interval:      config.ResourcePollInterval,
	}
}

func (m *ObservationMonitor) Run(ctx context.Context) error {
	defer m.ready.Store(false)
	m.pollAndLog(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.pollAndLog(ctx)
		}
	}
}

func (m *ObservationMonitor) Ready() bool { return m.ready.Load() }

func (m *ObservationMonitor) pollAndLog(ctx context.Context) {
	if err := m.poll(ctx); err != nil {
		ctrl.LoggerFrom(ctx).WithName("observation-monitor").Error(err, "observation poll failed")
	}
}

func (m *ObservationMonitor) poll(ctx context.Context) error {
	var pollErrors []error
	if err := m.refreshConfig(ctx); err != nil {
		pollErrors = append(pollErrors, err)
	}
	pods, err := m.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=" + string(corev1.PodPending),
	})
	if err != nil {
		pollErrors = append(pollErrors, fmt.Errorf("list Pending pods: %w", err))
		return errors.Join(pollErrors...)
	}
	m.ready.Store(true)

	snapshot := make(ObservationSnapshot, 0, len(pods.Items)+len(m.active))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		snapshot = append(snapshot, newResourceObservation(ResourceSpec{
			APIVersion: "v1",
			Resource:   "pods",
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			Path:       "status.phase",
		}, string(corev1.PodPending), pod.CreationTimestamp))
	}

	observationFailed := false
	for _, query := range m.active {
		observation, err := m.observeResource(ctx, query)
		if err != nil {
			pollErrors = append(pollErrors, fmt.Errorf(
				"observe %s %s/%s path %s: %w",
				query.Resource, query.Namespace, query.Name, query.Path, err,
			))
			observationFailed = true
			continue
		}
		snapshot = append(snapshot, observation)
	}
	if observationFailed {
		return errors.Join(pollErrors...)
	}

	log := ctrl.LoggerFrom(ctx).WithName("observation-monitor")
	log.Info("sending observation snapshot", "clusterID", m.destination.ClusterID, "observations", len(snapshot))
	if err := m.sender.Send(ctx, m.destination, snapshot); err != nil {
		pollErrors = append(pollErrors, fmt.Errorf("send observation snapshot: %w", err))
	}
	return errors.Join(pollErrors...)
}

func (m *ObservationMonitor) refreshConfig(ctx context.Context) error {
	configMap, err := m.configMaps.Get(ctx, m.configName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		m.active = nil
		m.activeDocument = ""
		return nil
	}
	if err != nil {
		return fmt.Errorf("read resource configuration: %w", err)
	}
	document := cmp.Or(strings.TrimSpace(configMap.Data[resourceConfigDataKey]), "[]")
	if document == m.activeDocument {
		return nil
	}
	config, err := parseResourceConfig([]byte(document))
	if err != nil {
		return fmt.Errorf("resource configuration was rejected: %w", err)
	}
	m.active = config
	m.activeDocument = document
	return nil
}

func (m *ObservationMonitor) observeResource(
	ctx context.Context,
	query resourceQuery,
) (ResourceObservation, error) {
	object, err := m.dynamicClient.Resource(query.gvr).Namespace(query.Namespace).Get(
		ctx, query.Name, metav1.GetOptions{},
	)
	if apierrors.IsNotFound(err) {
		return newResourceObservation(query.ResourceSpec, nil, metav1.Time{}), nil
	}
	if err != nil {
		return ResourceObservation{}, fmt.Errorf("get resource: %w", err)
	}
	value, err := extractScalar(object, query.path)
	if err != nil {
		return ResourceObservation{}, err
	}
	return newResourceObservation(query.ResourceSpec, value, object.GetCreationTimestamp()), nil
}

func newResourceObservation(
	spec ResourceSpec,
	value any,
	createdAt metav1.Time,
) ResourceObservation {
	observation := ResourceObservation{
		Resource: spec.Resource, Namespace: spec.Namespace, Name: spec.Name,
		Path: spec.Path, Value: value,
	}
	if !createdAt.IsZero() {
		observation.CreatedAt = createdAt.Unix()
	}
	return observation
}

func extractScalar(object *unstructured.Unstructured, path []string) (any, error) {
	value, found, err := unstructured.NestedFieldNoCopy(object.Object, path...)
	if err != nil {
		return nil, fmt.Errorf("extract path %q: invalid object shape", strings.Join(path, "."))
	}
	if !found {
		return nil, nil
	}
	switch value.(type) {
	case nil, bool, int64, float64, string:
		return value, nil
	default:
		return nil, fmt.Errorf("path %q value is not scalar", strings.Join(path, "."))
	}
}
