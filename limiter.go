// SPDX-FileCopyrightText: 2026 Kai Ole Hartwig <mail@ole-hartwig.eu>
// SPDX-License-Identifier: Apache-2.0

package inflightlimit

import (
	"context"
	"sync"
	"time"
)

// slots counts concurrently held slots per key, with at most limit held per
// key. A caller that finds its key full may queue for a bounded time; slots
// are handed to queued callers in arrival order, so a release never lets a
// newcomer overtake a waiting request.
//
// Memory is bounded by what is in flight: a key is forgotten as soon as it
// holds no slot and has no waiter.
type slots struct {
	mu    sync.Mutex
	limit int
	keys  map[string]*slotEntry
}

type slotEntry struct {
	held    int
	waiters []chan struct{}
}

func newSlots(limit int) *slots {
	return &slots{limit: limit, keys: make(map[string]*slotEntry)}
}

// acquire takes a slot for key. With wait > 0 it queues for up to wait when
// the key is full. It reports whether a slot was taken; every true must be
// followed by exactly one release.
func (s *slots) acquire(ctx context.Context, key string, wait time.Duration) bool {
	s.mu.Lock()
	e := s.keys[key]
	if e == nil {
		e = &slotEntry{}
		s.keys[key] = e
	}
	if e.held < s.limit && len(e.waiters) == 0 {
		e.held++
		s.mu.Unlock()
		return true
	}
	if wait <= 0 {
		s.mu.Unlock()
		return false
	}
	ch := make(chan struct{})
	e.waiters = append(e.waiters, ch)
	s.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
	case <-ctx.Done():
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range e.waiters {
		if w == ch {
			e.waiters = append(e.waiters[:i], e.waiters[i+1:]...)
			return false
		}
	}
	// release handed the slot over between the timeout and the lock: it is
	// ours, and the caller has to release it like any other.
	return true
}

// release gives a slot of key back, to the longest waiting caller if there
// is one.
func (s *slots) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.keys[key]
	if e == nil {
		return
	}
	if len(e.waiters) > 0 {
		ch := e.waiters[0]
		e.waiters = e.waiters[1:]
		close(ch) // the slot passes to the waiter, held stays the same
		return
	}
	e.held--
	if e.held <= 0 {
		delete(s.keys, key)
	}
}

// inFlight returns the slots held for key and the callers queued for it.
func (s *slots) inFlight(key string) (held, waiting int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.keys[key]; e != nil {
		return e.held, len(e.waiters)
	}
	return 0, 0
}

// size returns the number of keys currently tracked (for tests).
func (s *slots) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}
