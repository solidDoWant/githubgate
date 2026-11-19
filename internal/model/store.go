package model

import "sync/atomic"

// Store holds the snapshot in use. Each request reads one snapshot, so one
// decision never mixes two versions.
type Store struct {
	current atomic.Pointer[Snapshot]
}

// Load returns the snapshot in use, or nil before the first full build.
func (s *Store) Load() *Snapshot { return s.current.Load() }

// Ready reports whether a snapshot has been built.
func (s *Store) Ready() bool { return s.current.Load() != nil }

// Swap puts a new snapshot in use.
func (s *Store) Swap(snapshot *Snapshot) { s.current.Store(snapshot) }
