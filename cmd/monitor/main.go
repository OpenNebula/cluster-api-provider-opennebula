/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/monitor"
	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/readiness"
)

func main() {
	klog.InitFlags(nil)
	readinessFile := flag.String("readiness-checks-file", "", "run a finite readiness check from this JSON file")
	flag.Parse()
	if *readinessFile != "" {
		runReadiness(*readinessFile)
		return
	}

	config, err := monitor.ConfigFromEnv()
	if err != nil {
		klog.ErrorS(err, "invalid monitor configuration")
		os.Exit(1)
	}
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		klog.ErrorS(err, "unable to load in-cluster Kubernetes configuration")
		os.Exit(1)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.ErrorS(err, "unable to create Kubernetes client")
		os.Exit(1)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		klog.ErrorS(err, "unable to create dynamic Kubernetes client")
		os.Exit(1)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	metrics := monitor.NewMetrics(registry)
	sender, err := monitor.NewHTTPEncryptedSender(config)
	if err != nil {
		klog.ErrorS(err, "unable to create encrypted callback sender")
		os.Exit(1)
	}
	watcher, err := monitor.New(config, client, dynamicClient, sender, metrics)
	if err != nil {
		klog.ErrorS(err, "unable to create monitor")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	health := &http.Server{
		Addr:    config.HealthAddress,
		Handler: healthHandler(watcher),
	}
	var metricsServer *http.Server
	if config.MetricsAddress != "" {
		metricsServer = &http.Server{
			Addr:    config.MetricsAddress,
			Handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		}
		go func() {
			klog.InfoS("starting metrics server", "address", config.MetricsAddress)
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				klog.ErrorS(err, "metrics server failed")
			}
		}()
	}
	go func() {
		<-ctx.Done()
		_ = health.Close()
		if metricsServer != nil {
			_ = metricsServer.Close()
		}
	}()
	go func() {
		klog.InfoS("starting health server", "address", config.HealthAddress)
		if err := health.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.ErrorS(err, "health server failed")
		}
	}()

	if err := watcher.Run(ctx); err != nil {
		klog.ErrorS(err, "monitor stopped with an error")
		os.Exit(1)
	}
}

func runReadiness(path string) {
	config, err := readiness.Load(path)
	if err != nil {
		klog.ErrorS(err, "invalid readiness configuration")
		os.Exit(1)
	}
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		klog.ErrorS(err, "unable to load in-cluster Kubernetes configuration")
		os.Exit(1)
	}
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		klog.ErrorS(err, "unable to create dynamic Kubernetes client")
		os.Exit(1)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		klog.ErrorS(err, "unable to create Kubernetes discovery client")
		os.Exit(1)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	if err := readiness.Run(ctrl.SetupSignalHandler(), client, mapper, config); err != nil {
		klog.ErrorS(err, "readiness checks failed")
		os.Exit(1)
	}
	klog.InfoS("all readiness checks completed")
}

func healthHandler(watcher *monitor.Monitor) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !watcher.Ready() {
			http.Error(w, "informer caches are not synchronized", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})
	return mux
}
