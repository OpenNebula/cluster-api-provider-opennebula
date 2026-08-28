/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.
Licensed under the Apache License, Version 2.0 (the "License");
*/

package resourceobserver

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type Callback func(identity string, value ResourceValue) bool

type PollerOptions struct {
	ConfigNamespace string
	ConfigName      string
	PollInterval    time.Duration
}

const (
	DefaultPollInterval = 30 * time.Second
	MinPollInterval     = 5 * time.Second
	MaxPollInterval     = time.Hour
)

type ConfigStatus struct {
	ConfigMap string `json:"configMap"`
	Digest    string `json:"digest"`
	Active    bool   `json:"active"`
	LastError string `json:"lastError,omitempty"`
}

type activeConfig struct {
	config     Config
	revision   string
	generation uint64
	values     map[string]observedValue
}

type observedValue struct {
	value any
	set   bool
}

type configEvent struct {
	configMap  *corev1.ConfigMap
	deleted    bool
	generation uint64
}

type ResourceValuePoller struct {
	dynamicClient dynamic.Interface
	options       PollerOptions
	callback      Callback
	factory       informers.SharedInformerFactory
	informer      cache.SharedIndexInformer
	events        chan struct{}
	now           func() time.Time

	mu                sync.Mutex
	active            *activeConfig
	lastError         string
	desiredGeneration uint64
	desired           configEvent
}

func NewResourceValuePoller(client kubernetes.Interface, dynamicClient dynamic.Interface, options PollerOptions, callback Callback) (*ResourceValuePoller, error) {
	if callback == nil {
		return nil, fmt.Errorf("resource value callback is required")
	}
	if options.ConfigNamespace == "" {
		options.ConfigNamespace = "kube-system"
	}
	if options.ConfigName == "" {
		options.ConfigName = "capone-resource-monitor"
	}
	if options.PollInterval == 0 {
		options.PollInterval = DefaultPollInterval
	}
	if options.PollInterval < MinPollInterval || options.PollInterval > MaxPollInterval {
		return nil, fmt.Errorf("resource poll interval must be between %s and %s", MinPollInterval, MaxPollInterval)
	}
	poller := &ResourceValuePoller{
		dynamicClient: dynamicClient, options: options, callback: callback,
		events: make(chan struct{}, 1), now: time.Now,
	}
	poller.factory = informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithNamespace(options.ConfigNamespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = "metadata.name=" + options.ConfigName
		}),
	)
	poller.informer = poller.factory.Core().V1().ConfigMaps().Informer()
	if _, err := poller.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(value any) { poller.submit(value, false) },
		UpdateFunc: func(_, value any) { poller.submit(value, false) },
		DeleteFunc: func(value any) { poller.submit(value, true) },
	}); err != nil {
		return nil, fmt.Errorf("register resource value configuration handler: %w", err)
	}
	return poller, nil
}

func (poller *ResourceValuePoller) Run(ctx context.Context) {
	poller.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), poller.informer.HasSynced) {
		poller.setError("resource value ConfigMap informer did not synchronize")
		return
	}

	var timer *time.Timer
	var timerC <-chan time.Time
	schedule := func(delay time.Duration) {
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
		timerC = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			poller.disable()
			return
		case <-poller.events:
			poller.mu.Lock()
			event := poller.desired
			poller.mu.Unlock()
			if event.deleted {
				poller.disable()
				timerC = nil
				continue
			}
			if strings.TrimSpace(event.configMap.Data[ConfigDataKey]) == "" {
				poller.disable()
				timerC = nil
				continue
			}
			changed, err := poller.apply(event.configMap, event.generation)
			if err != nil {
				poller.setError(err.Error())
				klog.ErrorS(err, "resource value configuration was rejected",
					"configMap", ConfigMapIdentity(poller.options.ConfigNamespace, poller.options.ConfigName))
				continue
			}
			if changed {
				if _, active := poller.interval(); active {
					schedule(0)
				} else {
					timerC = nil
				}
			}
		case <-timerC:
			poller.poll(ctx)
			if interval, ok := poller.interval(); ok {
				schedule(interval)
			} else {
				timerC = nil
			}
		}
	}
}

func (poller *ResourceValuePoller) submit(value any, deleted bool) {
	var configMap *corev1.ConfigMap
	if item, ok := value.(*corev1.ConfigMap); ok {
		configMap = item.DeepCopy()
	} else if tombstone, ok := value.(cache.DeletedFinalStateUnknown); ok {
		configMap, _ = tombstone.Obj.(*corev1.ConfigMap)
	}
	if configMap == nil || configMap.Name != poller.options.ConfigName {
		return
	}
	poller.mu.Lock()
	poller.desiredGeneration++
	poller.desired = configEvent{configMap: configMap, deleted: deleted, generation: poller.desiredGeneration}
	poller.mu.Unlock()
	select {
	case poller.events <- struct{}{}:
	default:
	}
}

func (poller *ResourceValuePoller) apply(configMap *corev1.ConfigMap, generation uint64) (bool, error) {
	document, exists := configMap.Data[ConfigDataKey]
	if !exists {
		return false, fmt.Errorf("ConfigMap does not contain %q", ConfigDataKey)
	}
	config, revision, err := ParseConfig([]byte(document))
	if err != nil {
		return false, err
	}
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if generation != poller.desiredGeneration {
		return false, nil
	}
	if len(config) == 0 {
		changed := poller.active != nil
		poller.active = nil
		poller.lastError = ""
		return changed, nil
	}
	if poller.active != nil && poller.active.revision == revision {
		poller.active.generation = generation
		poller.lastError = ""
		return false, nil
	}
	poller.active = &activeConfig{
		config: config, revision: revision, generation: generation,
		values: make(map[string]observedValue, len(config)),
	}
	poller.lastError = ""
	return true, nil
}

func (poller *ResourceValuePoller) poll(ctx context.Context) {
	poller.mu.Lock()
	if poller.active == nil {
		poller.mu.Unlock()
		return
	}
	config := poller.active.config
	revision := poller.active.revision
	poller.mu.Unlock()

	errors := make([]string, 0)
	for _, spec := range config {
		if err := poller.pollResource(ctx, revision, spec); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", spec.ID, err))
		}
	}
	if len(errors) != 0 {
		poller.setError(strings.Join(errors, "; "))
		return
	}
	poller.mu.Lock()
	if poller.active != nil && poller.active.revision == revision {
		poller.lastError = ""
	}
	poller.mu.Unlock()
}

func (poller *ResourceValuePoller) pollResource(ctx context.Context, revision string, spec ResourceSpec) error {
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
		value, err = ExtractValue(object, spec.Path)
		if err != nil {
			return err
		}
	}

	poller.mu.Lock()
	if poller.active == nil || poller.active.revision != revision {
		poller.mu.Unlock()
		return nil
	}
	previous := poller.active.values[spec.ID]
	if previous.set && reflect.DeepEqual(previous.value, value) {
		poller.mu.Unlock()
		return nil
	}
	poller.mu.Unlock()

	report, err := NewResourceValue(spec, value, poller.now())
	if err != nil {
		return err
	}
	if !poller.callback(report.Identity(), report) {
		return fmt.Errorf("delivery queue rejected the report")
	}
	poller.mu.Lock()
	if poller.active != nil && poller.active.revision == revision {
		if poller.active.values == nil {
			poller.active.values = make(map[string]observedValue)
		}
		poller.active.values[spec.ID] = observedValue{value: value, set: true}
	}
	poller.mu.Unlock()
	return nil
}

func (poller *ResourceValuePoller) interval() (time.Duration, bool) {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.active == nil {
		return 0, false
	}
	return poller.options.PollInterval, true
}

func (poller *ResourceValuePoller) disable() {
	poller.mu.Lock()
	poller.active = nil
	poller.lastError = ""
	poller.mu.Unlock()
}

func (poller *ResourceValuePoller) Status() ConfigStatus {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	status := ConfigStatus{
		ConfigMap: ConfigMapIdentity(poller.options.ConfigNamespace, poller.options.ConfigName),
		LastError: poller.lastError,
	}
	if poller.active != nil {
		status.Digest = poller.active.revision
		status.Active = true
	}
	return status
}

func (poller *ResourceValuePoller) setError(message string) {
	poller.mu.Lock()
	poller.lastError = sanitizeError(message)
	poller.mu.Unlock()
}

func sanitizeError(message string) string {
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > 256 {
		message = message[:256]
	}
	return message
}
