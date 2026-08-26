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

	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const maxPendingReports = 8192

type reportQueue struct {
	sender Sender
	queue  workqueue.TypedRateLimitingInterface[string]

	mu      sync.Mutex
	pending map[string]*Report
}

func newReportQueue(sender Sender) *reportQueue {
	return &reportQueue{
		sender: sender,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		pending: map[string]*Report{},
	}
}

func (q *reportQueue) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		q.queue.ShutDown()
	}()

	for q.processNext(ctx) {
	}
}

func (q *reportQueue) Add(key string, report Report) bool {
	q.mu.Lock()
	if _, exists := q.pending[key]; !exists && len(q.pending) >= maxPendingReports {
		q.mu.Unlock()
		klog.ErrorS(
			fmt.Errorf("pending report limit %d reached", maxPendingReports),
			"report was not queued", "key", key,
		)
		return false
	}
	q.pending[key] = &report
	q.mu.Unlock()

	q.queue.Add(key)
	return true
}

func (q *reportQueue) processNext(ctx context.Context) bool {
	key, shutdown := q.queue.Get()
	if shutdown {
		return false
	}
	defer q.queue.Done(key)

	q.mu.Lock()
	report := q.pending[key]
	q.mu.Unlock()
	if report == nil {
		q.queue.Forget(key)
		return true
	}

	if err := q.sender.Send(ctx, *report); err != nil {
		klog.ErrorS(err, "unable to report resource status", "key", key)
		q.queue.AddRateLimited(key)
		return true
	}

	q.queue.Forget(key)
	q.mu.Lock()
	// A different pointer means a newer report arrived while this one was sent.
	if q.pending[key] == report {
		delete(q.pending, key)
	} else {
		q.queue.Add(key)
	}
	q.mu.Unlock()
	return true
}
