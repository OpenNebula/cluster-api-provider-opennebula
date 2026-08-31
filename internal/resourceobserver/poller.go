/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.
Licensed under the Apache License, Version 2.0 (the "License");
*/

package resourceobserver

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
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
)

type Callback func(identity string, value ResourceValue) bool

type Options struct {
	ConfigNamespace string
	ConfigName      string
	PollInterval    time.Duration
}

const (
	DefaultPollInterval = 10 * time.Second
	MinPollInterval     = 5 * time.Second
	MaxPollInterval     = time.Hour
)

type Poller struct {
	dynamicClient dynamic.Interface
	configMaps    corev1client.ConfigMapInterface
	options       Options
	callback      Callback
	now           func() time.Time
	active        Config
}

func NewPoller(
	client kubernetes.Interface,
	dynamicClient dynamic.Interface,
	options Options,
	callback Callback,
) (*Poller, error) {
	if callback == nil {
		return nil, fmt.Errorf("resource value callback is required")
	}
	options.ConfigNamespace = cmp.Or(options.ConfigNamespace, "kube-system")
	options.ConfigName = cmp.Or(options.ConfigName, "capone-resource-monitor")
	options.PollInterval = cmp.Or(options.PollInterval, DefaultPollInterval)
	if options.PollInterval < MinPollInterval || options.PollInterval > MaxPollInterval {
		return nil, fmt.Errorf(
			"resource poll interval must be between %s and %s",
			MinPollInterval,
			MaxPollInterval,
		)
	}
	return &Poller{
		dynamicClient: dynamicClient, options: options, callback: callback,
		configMaps: client.CoreV1().ConfigMaps(options.ConfigNamespace), now: time.Now,
	}, nil
}

func (poller *Poller) Run(ctx context.Context) {
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		poller.refreshConfig(ctx)
		if len(poller.active) == 0 {
			return
		}
		for _, spec := range poller.active {
			if err := poller.pollResource(ctx, spec); err != nil {
				klog.ErrorS(err, "resource value poll failed", "resource", spec.ID)
			}
		}
	}, poller.options.PollInterval)
}

func (poller *Poller) refreshConfig(ctx context.Context) {
	configMap, err := poller.configMaps.Get(ctx, poller.options.ConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		poller.active = nil
		return
	}
	if err != nil {
		klog.ErrorS(err, "read resource value configuration",
			"configMap", ConfigMapIdentity(poller.options.ConfigNamespace, poller.options.ConfigName))
		return
	}
	document := cmp.Or(strings.TrimSpace(configMap.Data[ConfigDataKey]), "[]")
	config, err := ParseConfig([]byte(document))
	if err != nil {
		klog.ErrorS(err, "resource value configuration was rejected",
			"configMap", ConfigMapIdentity(poller.options.ConfigNamespace, poller.options.ConfigName))
		return
	}
	poller.active = config
}

func (poller *Poller) pollResource(ctx context.Context, spec ResourceSpec) error {
	gvr, err := spec.GVR()
	if err != nil {
		return err
	}
	object, err := poller.dynamicClient.Resource(gvr).Namespace(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	var value any
	if apierrors.IsNotFound(err) {
		value = nil
	} else if err != nil {
		return fmt.Errorf("get resource: %w", err)
	} else {
		value, err = ExtractScalar(object, spec.Path)
		if err != nil {
			return err
		}
	}

	report, err := NewResourceValue(spec, value, poller.now())
	if err != nil {
		return err
	}
	if !poller.callback(report.Identity(), report) {
		return fmt.Errorf("delivery queue rejected the report")
	}
	return nil
}
