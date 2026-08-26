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
	"sort"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const applicationSelector = "app.kubernetes.io/managed-by=oneks,applications.oneks.opennebula.io/producer=oneks-server"

var applicationGVR = schema.GroupVersionResource{Group: "oneks.opennebula.io", Version: "v1alpha1", Resource: "oneksapplications"}

type Monitor struct {
	nodeFactory        informers.SharedInformerFactory
	applicationFactory dynamicinformer.DynamicSharedInformerFactory
	nodes              cache.SharedIndexInformer
	applications       cache.SharedIndexInformer
	reports            *reportQueue

	ready atomic.Bool
}

func New(config Config, client kubernetes.Interface, dynamicClient dynamic.Interface, sender Sender) (*Monitor, error) {
	m := &Monitor{reports: newReportQueue(sender)}
	// Disable periodic resync; reconciliation is driven by informer events.
	m.nodeFactory = informers.NewSharedInformerFactory(client, 0)
	m.applicationFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient, 0, config.ApplicationNamespace,
		func(options *metav1.ListOptions) { options.LabelSelector = applicationSelector },
	)
	m.nodes = m.nodeFactory.Core().V1().Nodes().Informer()
	m.applications = m.applicationFactory.ForResource(applicationGVR).Informer()

	if _, err := m.nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.onNode(obj, "Added") },
		UpdateFunc: func(_, obj any) { m.onNode(obj, "Updated") },
		DeleteFunc: func(obj any) { m.onNode(obj, "Deleted") },
	}); err != nil {
		return nil, fmt.Errorf("register node handler: %w", err)
	}
	if _, err := m.applications.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.onApplication(obj, "Added") },
		UpdateFunc: func(_, obj any) { m.onApplication(obj, "Updated") },
		DeleteFunc: func(obj any) { m.onApplication(obj, "Deleted") },
	}); err != nil {
		return nil, fmt.Errorf("register OneKSApplication handler: %w", err)
	}
	return m, nil
}

func (m *Monitor) Run(ctx context.Context) error {
	defer runtime.HandleCrash()
	m.nodeFactory.Start(ctx.Done())
	m.applicationFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.nodes.HasSynced, m.applications.HasSynced) {
		return fmt.Errorf("initial informer cache sync failed")
	}
	m.enqueueNodeSnapshot()
	m.ready.Store(true)
	defer m.ready.Store(false)
	klog.InfoS("monitor caches synchronized")

	m.reports.Run(ctx)
	return nil
}

func (m *Monitor) Ready() bool { return m.ready.Load() }

func (m *Monitor) onNode(obj any, event string) {
	node, ok := objectFromEvent[*corev1.Node](obj)
	if !ok {
		return
	}
	exclude := ""
	report := nodeReport(node, event)
	if event == "Deleted" {
		report.Status["deleted"] = true
		exclude = node.Spec.ProviderID
	}
	report.Status["readyProviderIDs"] = m.readyProviderIDs(exclude)
	m.reports.Add("Node/"+node.Name, report)
}

func (m *Monitor) readyProviderIDs(exclude string) []string {
	providerIDs := make([]string, 0)
	for _, item := range m.nodes.GetStore().List() {
		node, ok := item.(*corev1.Node)
		if !ok || node.Spec.ProviderID == "" || node.Spec.ProviderID == exclude || !nodeReady(node) {
			continue
		}
		providerIDs = append(providerIDs, node.Spec.ProviderID)
	}
	sort.Strings(providerIDs)
	return providerIDs
}

func (m *Monitor) enqueueNodeSnapshot() {
	readyProviderIDs := m.readyProviderIDs("")
	for _, item := range m.nodes.GetStore().List() {
		node, ok := item.(*corev1.Node)
		if !ok {
			continue
		}
		report := nodeReport(node, "Updated")
		report.Status["readyProviderIDs"] = readyProviderIDs
		m.reports.Add("Node/"+node.Name, report)
	}
}

func (m *Monitor) onApplication(obj any, event string) {
	app, ok := objectFromEvent[*unstructured.Unstructured](obj)
	if !ok {
		return
	}
	m.reports.Add("OneKSApplication/"+app.GetNamespace()+"/"+app.GetName(), applicationReport(app, event))
}

func objectFromEvent[T any](obj any) (T, bool) {
	if value, ok := obj.(T); ok {
		return value, true
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		var zero T
		return zero, false
	}
	value, ok := tombstone.Obj.(T)
	return value, ok
}
