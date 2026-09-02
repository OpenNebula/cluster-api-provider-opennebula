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

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/monitor"
	"github.com/OpenNebula/cluster-api-provider-opennebula/internal/resourceobserver"
)

func main() {
	logging := zap.Options{Development: false}
	logging.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logging)))
	log := ctrl.Log.WithName("setup")

	config, err := monitor.ConfigFromEnv()
	if err != nil {
		log.Error(err, "invalid monitor configuration")
		os.Exit(1)
	}
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Error(err, "unable to load in-cluster Kubernetes configuration")
		os.Exit(1)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		log.Error(err, "unable to create dynamic Kubernetes client")
		os.Exit(1)
	}
	sender, err := monitor.NewHTTPEncryptedSender(config)
	if err != nil {
		log.Error(err, "unable to create encrypted callback sender")
		os.Exit(1)
	}
	watcher, err := monitor.New(config, client, dynamicClient, sender)
	if err != nil {
		log.Error(err, "unable to create monitor")
		os.Exit(1)
	}
	resourcePoller, err := resourceobserver.NewPoller(
		client, dynamicClient,
		resourceobserver.Options{
			ConfigNamespace: config.ResourceConfigNamespace,
			ConfigName:      config.ResourceConfigName,
			PollInterval:    config.ResourcePollInterval,
		},
		func(identity string, value resourceobserver.ResourceValue) bool {
			return watcher.EnqueueCallback(identity, value)
		},
	)
	if err != nil {
		log.Error(err, "unable to create resource value poller")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	health := &http.Server{
		Addr:    config.HealthAddress,
		Handler: healthHandler(watcher),
	}
	go func() {
		<-ctx.Done()
		_ = health.Close()
	}()
	go resourcePoller.Run(ctx)
	go func() {
		log.Info("starting health server", "address", config.HealthAddress)
		if err := health.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error(err, "health server failed")
		}
	}()

	if err := watcher.Run(ctx); err != nil {
		log.Error(err, "monitor stopped with an error")
		os.Exit(1)
	}
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
