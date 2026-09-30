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
	"testing"
	"time"
)

type componentStub struct {
	ready   bool
	started chan<- struct{}
}

func (c componentStub) Run(ctx context.Context) error {
	c.started <- struct{}{}
	<-ctx.Done()
	return nil
}

func (c componentStub) Ready() bool { return c.ready }

func TestManagerCoordinatesComponents(t *testing.T) {
	started := make(chan struct{}, 2)
	manager := NewManager(
		componentStub{ready: true, started: started},
		componentStub{ready: true, started: started},
	)
	if !manager.Ready() {
		t.Fatal("manager with ready components is not ready")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("component was not started")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("manager stopped with an error: %v", err)
	}
}

func TestManagerIsNotReadyWithoutReadyComponents(t *testing.T) {
	if NewManager().Ready() {
		t.Fatal("empty manager is ready")
	}
	started := make(chan struct{}, 1)
	if NewManager(componentStub{started: started}).Ready() {
		t.Fatal("manager with an unready component is ready")
	}
}
