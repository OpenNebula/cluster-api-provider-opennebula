/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import "context"

// Component is an independently running part of the monitor. Ready reports
// whether its Kubernetes cache or initial Kubernetes polling access has been
// established. It does not assert current connectivity to OneKS or OpenNebula.
type Component interface {
	// Run blocks until the context is cancelled or the component encounters a
	// fatal startup/runtime error. Transient delivery errors are handled by the
	// component and must not normally terminate Run.
	Run(context.Context) error
	// Ready reports whether the component completed its initial Kubernetes
	// synchronization or successful Kubernetes list operation.
	Ready() bool
}

// Manager gives all monitor components one lifetime and one readiness result.
// A fatal error from any component cancels the shared lifetime.
type Manager struct {
	components []Component
}

// NewManager returns a manager for the supplied monitor components.
func NewManager(components ...Component) *Manager {
	return &Manager{components: components}
}

// Run starts every component and blocks until all components stop or the first
// component returns an error.
func (m *Manager) Run(ctx context.Context) error {
	// A component error ends the manager, the deferred cancellation then stops
	// the remaining components through the shared context
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errors := make(chan error, len(m.components))
	for _, component := range m.components {
		go func() {
			errors <- component.Run(ctx)
		}()
	}

	for range m.components {
		if err := <-errors; err != nil {
			return err
		}
	}
	return nil
}

// Ready reports true only when the manager has at least one component and all
// components report ready.
func (m *Manager) Ready() bool {
	if len(m.components) == 0 {
		return false
	}
	for _, component := range m.components {
		if !component.Ready() {
			return false
		}
	}
	return true
}
