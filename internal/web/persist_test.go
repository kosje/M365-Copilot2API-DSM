package web

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// persist.go had no tests, and its background loop was the reason the suite was
// intermittently red. These cover the store's own contract and guard the
// TestMain invariant that keeps the loop out of the test binary.

// TestPersistLoopIsNotRunning locks in the TestMain fix: persistLoop flushes
// every registered store on a 5s ticker, and a tick that lands while a test's
// t.TempDir() is being removed makes that cleanup fail on Windows. If the
// StopPersistLoop call is ever dropped, this fails immediately instead of the
// suite going back to failing three runs in four.
func TestPersistLoopIsNotRunning(t *testing.T) {
	select {
	case <-persistStopped:
		// Expected: TestMain already stopped the loop, and StopPersistLoop closes
		// this channel before returning.
	case <-time.After(5 * time.Second):
		t.Fatal("the persist loop is running during tests; a tick can race a " +
			"test's TempDir cleanup (see the note in TestMain)")
	}
}

func TestFlushPendingSkipsCleanStore(t *testing.T) {
	var calls int
	p := &persistStore{flush: func() error { calls++; return nil }}

	p.flushPending()
	if calls != 0 {
		t.Fatalf("a clean store was flushed %d time(s)", calls)
	}
}

func TestFlushPendingWritesOnceAndClearsDirty(t *testing.T) {
	var calls int
	p := &persistStore{flush: func() error { calls++; return nil }}

	p.markDirty()
	p.flushPending()
	if calls != 1 {
		t.Fatalf("flush called %d time(s), want 1", calls)
	}
	// A second pass must be a no-op: flushPending only writes what was marked.
	p.flushPending()
	if calls != 1 {
		t.Fatalf("flush called %d time(s) after the dirty flag was cleared", calls)
	}
}

// A failed flush has to stay dirty, otherwise the change is silently lost until
// something else marks the store again.
func TestFlushPendingKeepsStoreDirtyOnFailure(t *testing.T) {
	wantErr := errors.New("disk full")
	var calls int
	p := &persistStore{flush: func() error { calls++; return wantErr }}

	p.markDirty()
	p.flushPending()
	if calls != 1 {
		t.Fatalf("flush called %d time(s), want 1", calls)
	}
	// Still dirty, so the next tick retries.
	p.flushPending()
	if calls != 2 {
		t.Fatalf("the store was not retried after a failed flush (calls=%d)", calls)
	}
}

func TestFlushNowBlockingSurfacesErrorAndStaysDirty(t *testing.T) {
	wantErr := errors.New("read-only fs")
	p := &persistStore{flush: func() error { return wantErr }}

	if err := p.flushNowBlocking(); !errors.Is(err, wantErr) {
		t.Fatalf("flushNowBlocking error = %v, want %v", err, wantErr)
	}
	p.dirtyMu.Lock()
	dirty := p.dirty
	p.dirtyMu.Unlock()
	if !dirty {
		t.Fatal("a failed blocking flush left the store clean; the change would be lost")
	}
}

func TestFlushNowBlockingReportsSuccess(t *testing.T) {
	var calls int
	p := &persistStore{flush: func() error { calls++; return nil }}

	if err := p.flushNowBlocking(); err != nil {
		t.Fatalf("flushNowBlocking: %v", err)
	}
	if calls != 1 {
		t.Fatalf("flush called %d time(s), want 1", calls)
	}
	// flushNowBlocking is the synchronous path, so it writes even when nothing was
	// marked dirty - callers use it right after mutating state.
	p.dirtyMu.Lock()
	dirty := p.dirty
	p.dirtyMu.Unlock()
	if dirty {
		t.Fatal("a successful blocking flush left the store dirty")
	}
}

// markDirty must register the store so FlushAllPersist (the graceful-shutdown
// path in cmd/server) reaches it.
func TestMarkDirtyRegistersStoreForFlushAll(t *testing.T) {
	var mu sync.Mutex
	var calls int
	p := &persistStore{flush: func() error { mu.Lock(); calls++; mu.Unlock(); return nil }}

	p.markDirty()
	FlushAllPersist()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got == 0 {
		t.Fatal("FlushAllPersist did not reach a store registered by markDirty")
	}
}
