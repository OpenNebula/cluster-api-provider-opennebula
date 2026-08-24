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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileAuthProviderReadsCurrentTrimmedCredential(t *testing.T) {
	path := writeMonitorAuthFile(t, " \toneadmin:first-token\n")
	provider, err := newFileAuthProvider(path)
	if err != nil {
		t.Fatalf("create file authentication provider: %v", err)
	}

	credential, err := provider.Auth(context.Background())
	if err != nil {
		t.Fatalf("read initial credential: %v", err)
	}
	if credential != "oneadmin:first-token" {
		t.Fatalf("unexpected initial credential: %q", credential)
	}

	if err := os.WriteFile(path, []byte("rotated-user:second-token\n"), 0o600); err != nil {
		t.Fatalf("rotate authentication file: %v", err)
	}
	credential, err = provider.Auth(context.Background())
	if err != nil {
		t.Fatalf("read rotated credential: %v", err)
	}
	if credential != "rotated-user:second-token" {
		t.Fatalf("provider did not observe rotated credential: %q", credential)
	}
}

func TestFileAuthProviderFollowsAtomicProjectedSecretUpdate(t *testing.T) {
	root := t.TempDir()
	writeVersion := func(directory, credential string) {
		t.Helper()
		path := filepath.Join(root, directory)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("create projected Secret version: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "ONE_AUTH"), []byte(credential), 0o600); err != nil {
			t.Fatalf("write projected Secret version: %v", err)
		}
	}
	writeVersion("..data-old", "first-user:first-token\n")
	if err := os.Symlink("..data-old", filepath.Join(root, "..data")); err != nil {
		t.Skipf("filesystem does not support projected-Secret symlinks: %v", err)
	}
	if err := os.Symlink(filepath.Join("..data", "ONE_AUTH"), filepath.Join(root, "ONE_AUTH")); err != nil {
		t.Fatalf("create projected key symlink: %v", err)
	}

	provider, err := newFileAuthProvider(filepath.Join(root, "ONE_AUTH"))
	if err != nil {
		t.Fatalf("create file authentication provider: %v", err)
	}
	credential, err := provider.Auth(context.Background())
	if err != nil || credential != "first-user:first-token" {
		t.Fatalf("initial projected credential = %q, err %v", credential, err)
	}

	writeVersion("..data-new", "second-user:second-token\n")
	temporaryLink := filepath.Join(root, "..data-tmp")
	if err := os.Symlink("..data-new", temporaryLink); err != nil {
		t.Fatalf("create replacement data symlink: %v", err)
	}
	if err := os.Rename(temporaryLink, filepath.Join(root, "..data")); err != nil {
		t.Skipf("filesystem does not support atomic symlink replacement: %v", err)
	}

	credential, err = provider.Auth(context.Background())
	if err != nil || credential != "second-user:second-token" {
		t.Fatalf("updated projected credential = %q, err %v", credential, err)
	}
}

func TestFileAuthProviderRejectsEmptyCredentialWithoutExposingContents(t *testing.T) {
	for name, contents := range map[string]string{
		"empty":      "",
		"whitespace": " \t\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeMonitorAuthFile(t, contents)
			_, err := newFileAuthProvider(path)
			if err == nil || !strings.Contains(err.Error(), "authentication file is empty") {
				t.Fatalf("expected empty credential rejection, got %v", err)
			}
			if contents != "" && strings.Contains(err.Error(), contents) {
				t.Fatalf("authentication error exposed file contents: %v", err)
			}
		})
	}
}

func TestFileAuthProviderReportsReadErrorsWithoutCredentialContents(t *testing.T) {
	for name, path := range map[string]string{
		"nonexistent": filepath.Join(t.TempDir(), "missing-ONE_AUTH"),
		"not a file":  t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newFileAuthProvider(path)
			if err == nil || !strings.Contains(err.Error(), "read monitor authentication file") {
				t.Fatalf("expected useful file read error, got %v", err)
			}
		})
	}
}

func writeMonitorAuthFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ONE_AUTH")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write authentication file: %v", err)
	}
	return path
}
