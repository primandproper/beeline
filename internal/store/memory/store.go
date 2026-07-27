// Package memory holds in-memory implementations of the estimate Store and the
// control-plane Repository.
//
// These are **test doubles, not a deployable backend.** Nothing here is wired
// into the CLI: a `serve` process always runs against shared Postgres, because
// per-head in-memory state would make heads disagree. What these exist for is the
// packages above the storage seams (control, httpapi, query, refresh, follower),
// which need somewhere to put fixtures without a database.
//
// A double is only worth having if it cannot lie about the thing it stands in
// for, so both are pinned to the real backends by conformance suites:
// internal/store/storetest for the Store, internal/control/repositorytest for the
// Repository. Change behavior here and those suites are what tell you whether the
// change was a fix or a divergence.
package memory

import (
	"context"
	"sync"

	"github.com/primandproper/beeline/internal/beeline"
)

// Store holds estimates keyed by pair. The zero value is not usable; call New.
type Store struct {
	data map[beeline.PairKey]beeline.Stored
	mu   sync.RWMutex
}

// New returns an empty Store.
func New() *Store {
	return &Store{data: make(map[beeline.PairKey]beeline.Stored)}
}

// BatchGet returns one result per key, positionally aligned. A nil element marks a
// miss, matching the interface contract.
func (s *Store) BatchGet(_ context.Context, keys []beeline.PairKey) ([]*beeline.Stored, error) {
	out := make([]*beeline.Stored, len(keys))

	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range keys {
		if v, ok := s.data[keys[i]]; ok {
			stored := v
			out[i] = &stored
		}
	}

	return out, nil
}

// Put writes each entry, overwriting any prior value for the same key.
func (s *Store) Put(_ context.Context, entries []beeline.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range entries {
		s.data[entries[i].Key] = entries[i].Stored
	}

	return nil
}

// Reset drops every stored estimate. The control plane calls it on a runtime
// re-tessellation: the old pairs' keys (and often their resolution) no longer
// belong to the working set, so their cached values are dead weight.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data = make(map[beeline.PairKey]beeline.Stored)
}

// DeleteArea drops every stored estimate belonging to one service area, leaving
// other areas untouched. The control plane calls it when an area is disabled (or its
// geometry changes) so the shared store keeps only enabled areas' cached values. The
// prototype scans the map, which is fine because disable is a rare, operator-driven
// event; a production store would key by area or maintain a secondary index.
func (s *Store) DeleteArea(_ context.Context, area beeline.AreaID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for k := range s.data {
		if k.Area == area {
			delete(s.data, k)
		}
	}

	return nil
}

// Delete drops the given keys from the store, ignoring any that are absent. The
// demand-decay janitor calls it with the keys the freshness index swept, so a cold
// demand pair's cached estimate leaves the hot store together with its index entry.
func (s *Store) Delete(_ context.Context, keys []beeline.PairKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range keys {
		delete(s.data, keys[i])
	}

	return nil
}

// Len reports how many pairs are currently stored (useful for tests and metrics).
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.data)
}
