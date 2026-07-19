// Package memory is an in-memory beeline.Store: a mutex-guarded map. It is the
// prototype's hot read path, standing in for Redis/DynamoDB/Aerospike. Because the
// Store interface is deliberately tiny (batch get/put, no ordering), this
// implementation is a few lines and a production KV drops in behind the same seam.
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

// Len reports how many pairs are currently stored (useful for tests and metrics).
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.data)
}
