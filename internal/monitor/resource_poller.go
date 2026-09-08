/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.
Licensed under the Apache License, Version 2.0 (the "License");
*/

package monitor

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

type resourcePoller struct {
	dynamicClient  dynamic.Interface
	configMaps     corev1client.ConfigMapInterface
	config         Config
	reports        *reportQueue
	now            func() time.Time
	active         resourceConfig
	activeDocument string
}

func (poller *resourcePoller) Run(ctx context.Context) {
	log := ctrl.LoggerFrom(ctx).WithName("resource-observer")
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		poller.refreshConfig(ctx)
		if len(poller.active) == 0 {
			return
		}
		for _, spec := range poller.active {
			if err := poller.pollResource(ctx, spec); err != nil {
				log.Error(err, "resource value poll failed", "resource", spec.ID)
			}
		}
	}, poller.config.ResourcePollInterval)
}

func (poller *resourcePoller) refreshConfig(ctx context.Context) {
	log := ctrl.LoggerFrom(ctx).WithName("resource-observer")
	configMap, err := poller.configMaps.Get(ctx, poller.config.ResourceConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		poller.active = nil
		poller.activeDocument = ""
		return
	}
	if err != nil {
		log.Error(err, "read resource value configuration",
			"configMap", poller.config.ResourceConfigNamespace+"/"+poller.config.ResourceConfigName)
		return
	}
	document := cmp.Or(strings.TrimSpace(configMap.Data[ConfigDataKey]), "[]")
	if document == poller.activeDocument {
		return
	}
	config, err := parseResourceConfig([]byte(document))
	if err != nil {
		log.Error(err, "resource value configuration was rejected",
			"configMap", poller.config.ResourceConfigNamespace+"/"+poller.config.ResourceConfigName)
		return
	}
	poller.active = config
	poller.activeDocument = document
}

func (poller *resourcePoller) pollResource(ctx context.Context, spec resourceQuery) error {
	object, err := poller.dynamicClient.Resource(spec.gvr).Namespace(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	var value any
	if apierrors.IsNotFound(err) {
		value = nil
	} else if err != nil {
		return fmt.Errorf("get resource: %w", err)
	} else {
		value, err = extractScalar(object, spec.path)
		if err != nil {
			return err
		}
	}

	report, err := NewResourceValue(spec.ResourceSpec, value, poller.now())
	if err != nil {
		return err
	}
	if !poller.reports.Add(spec.identity, report) {
		return fmt.Errorf("delivery queue rejected the report")
	}
	return nil
}
