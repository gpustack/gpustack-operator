package datax

import (
	"sync/atomic"
)

// Snapshot is a race-free holder for the latest stored value of type T.
//
// The zero value is ready to use.
type Snapshot[T any] struct {
	v atomic.Pointer[T]
}

// Store replaces the snapshot with the given value.
func (s *Snapshot[T]) Store(v *T) {
	s.v.Store(v)
}

// Load returns the latest stored value,
// or nil if nothing has been stored yet.
func (s *Snapshot[T]) Load() *T {
	return s.v.Load()
}
