// SPDX-FileCopyrightText: 2026 Kai Ole Hartwig <mail@ole-hartwig.eu>
// SPDX-License-Identifier: Apache-2.0

package inflightlimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlotsNeverExceedLimit(t *testing.T) {
	s := newSlots(2)
	var cur, peak, served atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !s.acquire(context.Background(), "k", 5*time.Second) {
				t.Error("a waiter timed out although slots kept freeing up")
				return
			}
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			cur.Add(-1)
			served.Add(1)
			s.release("k")
		}()
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("peak concurrency %d, limit 2", peak.Load())
	}
	if served.Load() != 50 {
		t.Fatalf("served %d of 50", served.Load())
	}
	if s.size() != 0 {
		t.Fatalf("%d keys left after every slot was released", s.size())
	}
}

func TestSlotsKeysAreIndependent(t *testing.T) {
	s := newSlots(1)
	if !s.acquire(context.Background(), "a", 0) {
		t.Fatal("a: first slot refused")
	}
	if !s.acquire(context.Background(), "b", 0) {
		t.Fatal("b is held back by a")
	}
	if s.acquire(context.Background(), "a", 0) {
		t.Fatal("a got a second slot over limit 1")
	}
}

func TestSlotsWaitThenRefuse(t *testing.T) {
	s := newSlots(1)
	s.acquire(context.Background(), "k", 0)
	start := time.Now()
	if s.acquire(context.Background(), "k", 50*time.Millisecond) {
		t.Fatal("got a slot nobody released")
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("refused after %v, before the wait was over", d)
	}
	if held, waiting := s.inFlight("k"); held != 1 || waiting != 0 {
		t.Fatalf("after the timeout: held %d waiting %d, want 1 0", held, waiting)
	}
}

func TestSlotsWaiterGetsReleasedSlot(t *testing.T) {
	s := newSlots(1)
	s.acquire(context.Background(), "k", 0)
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.release("k")
	}()
	if !s.acquire(context.Background(), "k", time.Second) {
		t.Fatal("waiter did not get the released slot")
	}
	if held, _ := s.inFlight("k"); held != 1 {
		t.Fatalf("held %d after hand-over, want 1", held)
	}
	s.release("k")
	if s.size() != 0 {
		t.Fatal("key not forgotten")
	}
}

func TestSlotsFIFO(t *testing.T) {
	s := newSlots(1)
	s.acquire(context.Background(), "k", 0)
	order := make(chan int, 3)
	for i := range 3 {
		go func() {
			if s.acquire(context.Background(), "k", 2*time.Second) {
				order <- i
				time.Sleep(5 * time.Millisecond)
				s.release("k")
			}
		}()
		// Queue them in a known order.
		for {
			if _, w := s.inFlight("k"); w == i+1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	s.release("k")
	for want := range 3 {
		if got := <-order; got != want {
			t.Fatalf("slot %d went to waiter %d", want, got)
		}
	}
}

func TestSlotsNewcomerDoesNotOvertakeWaiter(t *testing.T) {
	s := newSlots(1)
	s.acquire(context.Background(), "k", 0)
	got := make(chan bool)
	go func() { got <- s.acquire(context.Background(), "k", time.Second) }()
	for {
		if _, w := s.inFlight("k"); w == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.release("k") // goes to the waiter
	if s.acquire(context.Background(), "k", 0) {
		t.Fatal("a newcomer took the slot meant for the waiter")
	}
	if !<-got {
		t.Fatal("waiter lost its slot")
	}
}

func TestSlotsCancelledWaiterLeavesQueue(t *testing.T) {
	s := newSlots(1)
	s.acquire(context.Background(), "k", 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { done <- s.acquire(ctx, "k", time.Minute) }()
	for {
		if _, w := s.inFlight("k"); w == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if <-done {
		t.Fatal("cancelled waiter got a slot")
	}
	if _, w := s.inFlight("k"); w != 0 {
		t.Fatalf("%d waiters left after cancel", w)
	}
	s.release("k")
	if s.size() != 0 {
		t.Fatal("key not forgotten")
	}
}
