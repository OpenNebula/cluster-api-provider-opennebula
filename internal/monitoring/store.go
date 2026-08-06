/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package monitoring

import (
	"fmt"
	"sort"
	"sync"
)

type Store struct {
	mu                sync.RWMutex
	allowedNamespaces map[string]struct{}
	profiles          map[string]Profile
	changes           chan struct{}
}

func NewStore(allowedNamespaces []string) *Store {
	allowed := make(map[string]struct{}, len(allowedNamespaces))
	for _, namespace := range allowedNamespaces {
		allowed[namespace] = struct{}{}
	}
	return &Store{
		allowedNamespaces: allowed,
		profiles:          make(map[string]Profile),
		changes:           make(chan struct{}, 1),
	}
}

func (s *Store) Upsert(configMapKey string, document []byte) error {
	profile, err := ParseProfile(document, s.allowedNamespaces)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.profiles[configMapKey]; !exists && len(s.profiles) >= MaxProfiles {
		return fmt.Errorf("loaded profile limit %d reached", MaxProfiles)
	}
	for key, existing := range s.profiles {
		if key != configMapKey && existing.Metadata.Name == profile.Metadata.Name {
			return fmt.Errorf("profile name %q is already loaded", profile.Metadata.Name)
		}
	}
	s.profiles[configMapKey] = profile
	s.notify()
	return nil
}

func (s *Store) Delete(configMapKey string) {
	s.mu.Lock()
	_, existed := s.profiles[configMapKey]
	delete(s.profiles, configMapKey)
	s.mu.Unlock()
	if existed {
		s.notify()
	}
}

func (s *Store) List() []Profile {
	s.mu.RLock()
	profiles := make([]Profile, 0, len(s.profiles))
	for _, profile := range s.profiles {
		profiles = append(profiles, profile)
	}
	s.mu.RUnlock()
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].Metadata.Name < profiles[j].Metadata.Name
	})
	return profiles
}

func (s *Store) Changes() <-chan struct{} { return s.changes }

func (s *Store) notify() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}
