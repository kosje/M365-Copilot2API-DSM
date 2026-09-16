package web

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The audit log grows on every auditLog call and nothing trimmed it, so on a NAS
// that stays up for months it grew without limit. It must roll over instead.
func TestAuditStoreRotatesInsteadOfGrowingForever(t *testing.T) {
	dir := t.TempDir()
	a := &auditStore{path: filepath.Join(dir, "audit.jsonl")}

	old := auditMaxBytes
	auditMaxBytes = 4096
	t.Cleanup(func() { auditMaxBytes = old })

	const entries = 400
	const padding = 40
	for i := 0; i < entries; i++ {
		a.append(auditEntry{
			Time:   time.Now(),
			Event:  "test_event",
			Detail: fmt.Sprintf("entry %03d %s", i, strings.Repeat("x", padding)),
		})
	}

	live, err := os.Stat(a.path)
	if err != nil {
		t.Fatalf("audit.jsonl: %v", err)
	}
	var rolled int64
	var rolledFiles int
	for gen := 1; gen <= auditKeepGenerations; gen++ {
		fi, err := os.Stat(fmt.Sprintf("%s.%d", a.path, gen))
		if err != nil {
			t.Fatalf("rolled file %d missing: %v", gen, err)
		}
		rolled += fi.Size()
		rolledFiles++
	}
	if rolledFiles != auditKeepGenerations {
		t.Fatalf("expected %d rolled file(s), found %d", auditKeepGenerations, rolledFiles)
	}

	// Every line must still be a complete JSON object: a rotate that lands
	// mid-write would corrupt the trail rather than truncate it.
	for _, p := range []string{a.path, a.path + ".1"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
				t.Errorf("%s line %d is not a complete JSON object", p, i+1)
				break
			}
		}
	}

	// The bound has to hold in total, not per file.
	total := live.Size() + rolled
	if total > int64(auditMaxBytes*(auditKeepGenerations+1)) {
		t.Fatalf("audit trail is %d bytes, expected at most %d",
			total, int64(auditMaxBytes*(auditKeepGenerations+1)))
	}

	// Newest entries must still be readable through list().
	got := a.list(10)
	if len(got) != 10 {
		t.Fatalf("list(10) returned %d entries", len(got))
	}
	if got[0].Event != "test_event" {
		t.Fatalf("newest entry event = %q", got[0].Event)
	}
}

// A fresh log must not rotate on its first write, and an empty directory must
// not confuse the rotation.
func TestAuditStoreDoesNotRotateAFreshLog(t *testing.T) {
	dir := t.TempDir()
	a := &auditStore{path: filepath.Join(dir, "audit.jsonl")}
	a.append(auditEntry{Time: time.Now(), Event: "first"})
	if _, err := os.Stat(a.path + ".1"); !os.IsNotExist(err) {
		t.Fatal("a fresh log was rotated away on its first write")
	}
	if _, err := os.Stat(a.path); err != nil {
		t.Fatalf("audit.jsonl: %v", err)
	}
}
