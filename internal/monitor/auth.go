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
	"os"
	"strings"
)

type AuthProvider interface {
	Auth(context.Context) (string, error)
}

type fileAuthProvider struct {
	path string
}

func newFileAuthProvider(path string) (*fileAuthProvider, error) {
	provider := &fileAuthProvider{path: path}
	if _, err := provider.Auth(context.Background()); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *fileAuthProvider) Auth(_ context.Context) (string, error) {
	contents, err := os.ReadFile(p.path)
	if err != nil {
		return "", fmt.Errorf("read monitor authentication file: %w", err)
	}
	credential := strings.TrimSpace(string(contents))
	if credential == "" {
		return "", fmt.Errorf("monitor authentication file is empty")
	}
	return credential, nil
}
