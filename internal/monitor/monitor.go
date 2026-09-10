/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import "context"

type Component interface {
	Run(context.Context) error
	Ready() bool
}

type Manager struct {
	components []Component
}

func NewManager(components ...Component) *Manager {
	return &Manager{components: components}
}

func (m *Manager) Run(ctx context.Context) error {
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
