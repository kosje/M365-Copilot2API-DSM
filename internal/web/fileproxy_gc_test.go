package web

import (
	"fmt"
	"testing"
	"time"
)

// The generated-file proxy keeps a map of conversation contexts, each holding an
// M365 access token, plus a cache of the bytes it retrieved. Nothing ever
// evicted them: on a NAS that stays up for months the maps only grew, and the
// tokens stayed resident.
func TestPruneFileProxyEvictsStaleContextsAndFiles(t *testing.T) {
	s := &Server{
		fileProxyCtx: map[string]m365ConvCtx{
			"fresh": {At: time.Now(), AccessToken: "tok-fresh"},
			"stale": {At: time.Now().Add(-2 * fileProxyCtxTTL), AccessToken: "tok-stale"},
		},
		fileProxyCache: map[string]string{
			"fresh|a.pdf": "/tmp/a.pdf",
			"stale|b.pdf": "/tmp/b.pdf",
			"gone|c.pdf":  "/tmp/c.pdf", // context already absent
		},
	}
	s.fileProxyMu.Lock()
	s.pruneFileProxyLocked()
	s.fileProxyMu.Unlock()

	if _, ok := s.fileProxyCtx["stale"]; ok {
		t.Error("a stale conversation context was kept")
	}
	if _, ok := s.fileProxyCtx["fresh"]; !ok {
		t.Error("a fresh conversation context was evicted")
	}
	if _, ok := s.fileProxyCache["stale|b.pdf"]; ok {
		t.Error("the cached file of a stale context was kept")
	}
	if _, ok := s.fileProxyCache["gone|c.pdf"]; ok {
		t.Error("a cached file with no conversation context was kept")
	}
	if _, ok := s.fileProxyCache["fresh|a.pdf"]; !ok {
		t.Error("the cached file of a fresh context was evicted")
	}
}

func TestPruneFileProxyEnforcesCapKeepingNewest(t *testing.T) {
	s := &Server{fileProxyCtx: map[string]m365ConvCtx{}, fileProxyCache: map[string]string{}}
	base := time.Now()
	for i := 0; i < maxFileProxyEntries+25; i++ {
		s.fileProxyCtx[fmt.Sprintf("c%04d", i)] = m365ConvCtx{
			// Later entries are newer.
			At: base.Add(time.Duration(i) * time.Second),
		}
	}
	s.fileProxyMu.Lock()
	s.pruneFileProxyLocked()
	s.fileProxyMu.Unlock()

	if len(s.fileProxyCtx) != maxFileProxyEntries {
		t.Fatalf("length = %d, want %d", len(s.fileProxyCtx), maxFileProxyEntries)
	}
	newest := fmt.Sprintf("c%04d", maxFileProxyEntries+24)
	if _, ok := s.fileProxyCtx[newest]; !ok {
		t.Error("the cap evicted the newest entry")
	}
	if _, ok := s.fileProxyCtx["c0000"]; ok {
		t.Error("the cap kept the oldest entry")
	}
}

// An empty map must not panic, and pruning must be safe to call when nothing has
// been recorded yet.
func TestPruneFileProxyOnEmptyServer(t *testing.T) {
	s := &Server{}
	s.fileProxyMu.Lock()
	s.pruneFileProxyLocked()
	s.fileProxyMu.Unlock()
}
