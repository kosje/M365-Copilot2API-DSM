package web

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestIPAllowed(t *testing.T) {
	cases := []struct {
		ip        string
		whitelist []string
		want      bool
	}{
		{"192.168.1.5", nil, true},
		{"192.168.1.5", []string{"192.168.1.5"}, true},
		{"192.168.1.6", []string{"192.168.1.5"}, false},
		{"10.1.2.3", []string{"10.0.0.0/8"}, true},
		{"11.1.2.3", []string{"10.0.0.0/8"}, false},
		{"10.1.2.3", []string{"10.0.0.0/8", "192.168.1.5"}, true},
		{"bogus", []string{"10.0.0.0/8"}, false},
	}
	for _, c := range cases {
		if got := ipAllowed(c.ip, c.whitelist); got != c.want {
			t.Errorf("ipAllowed(%q, %v) = %v, want %v", c.ip, c.whitelist, got, c.want)
		}
	}
}

func TestModelAllowed(t *testing.T) {
	wl := []string{"GPT-5.6-Sol", "gpt-5.6-luna"}
	if !modelAllowed("gpt-5.6-sol", wl) {
		t.Error("case-insensitive whitelist match failed")
	}
	if modelAllowed("gpt-5.6-terra", wl) {
		t.Error("model outside whitelist allowed")
	}
	if !modelAllowed("anything", nil) {
		t.Error("empty whitelist must allow all")
	}
}

func TestKeyRateWindow(t *testing.T) {
	w := newKeyRateWindow()
	now := time.Now()
	w.nowFunc = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !w.allow("k1", 3) {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if w.allow("k1", 3) {
		t.Error("4th request in minute should be denied")
	}
	if !w.allow("k2", 3) {
		t.Error("independent key should be allowed")
	}
	if !w.allow("k1", 0) {
		t.Error("limit 0 means unlimited")
	}
	// Next minute resets.
	now = now.Add(61 * time.Second)
	if !w.allow("k1", 3) {
		t.Error("new minute should reset the counter")
	}
}

func TestResolveModelAlias(t *testing.T) {
	st := &settingsStore{v: defaultRuntimeSettings()}
	st.v.ModelAliases = map[string]string{"gpt-4o": "gpt-5.6-sol", "claude-sonnet-4": "gpt-5.6-luna"}
	if got := st.resolveModelAlias("GPT-4O"); got != "gpt-5.6-sol" {
		t.Errorf("alias resolution = %q", got)
	}
	if got := st.resolveModelAlias("gpt-5.6-sol"); got != "gpt-5.6-sol" {
		t.Errorf("public model passthrough = %q", got)
	}
	if got := st.resolveModelAlias("unknown-model"); got != "unknown-model" {
		t.Errorf("unmatched passthrough = %q", got)
	}
	var nilStore *settingsStore
	if got := nilStore.resolveModelAlias("gpt-4o"); got != "gpt-4o" {
		t.Errorf("nil store must passthrough, got %q", got)
	}
}

func TestKeyExpiryAndWhitelistUpdate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_API_KEYS", dir+"/api-keys.json")
	store := openAPIKeys()
	rec, raw, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(24 * time.Hour)
	expPtr := &exp
	updated, err := store.update(rec.ID, keyUpdateOpts{
		PerMinuteRate:  int64p(30),
		ExpiresAt:      &expPtr,
		ModelWhitelist: &[]string{"gpt-5.6-sol"},
		IPWhitelist:    &[]string{"192.168.0.0/16"},
	})
	if err != nil || !updated {
		t.Fatalf("update failed: %v %v", updated, err)
	}
	// The cleartext comes from create's return value, not from the store: the
	// store no longer retains it (see TestAPIKeyCleartextNeverPersisted).
	got, ok := store.lookupRaw(raw)
	if !ok {
		t.Fatal("key not resolvable")
	}
	if got.PerMinuteRate != 30 || got.ExpiresAt == nil || len(got.ModelWhitelist) != 1 || len(got.IPWhitelist) != 1 {
		t.Fatalf("fields not persisted: %+v", got)
	}
}

// The cleartext key must not survive in memory, on disk, or in anything handed
// back to a client.
func TestAPIKeyCleartextNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/api-keys.json"
	t.Setenv("M365_API_KEYS", path)

	store := openAPIKeys()
	rec, raw, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("create must return the cleartext once")
	}
	if rec.Raw != "" {
		t.Fatalf("record returned by create leaks the cleartext: %q", rec.Raw)
	}
	if rec.Hash != "" {
		t.Fatal("record returned by create leaks the hash")
	}

	// In memory.
	store.mu.Lock()
	for _, k := range store.Keys {
		if k.Raw != "" {
			store.mu.Unlock()
			t.Fatalf("store retains cleartext for key %s", k.ID)
		}
	}
	store.mu.Unlock()

	// On disk.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), raw) {
		t.Fatal("api-keys.json contains the cleartext key")
	}
	if strings.Contains(string(b), "\"raw\"") {
		t.Fatal(`api-keys.json still carries a "raw" field`)
	}
	// The hash must be there, otherwise the key would not authenticate.
	if !strings.Contains(string(b), keyHash(raw)) {
		t.Fatal("api-keys.json does not contain the key hash")
	}

	// Via the console listing.
	for _, k := range store.list() {
		if k.Raw != "" || k.Hash != "" {
			t.Fatalf("list() leaks secrets: raw=%q hash=%q", k.Raw, k.Hash)
		}
	}

	// And a legacy file that still carries cleartext must be rewritten without
	// it, while remaining usable with the same key material.
	legacy := `{"keys":[{"id":"deadbeef","name":"legacy","prefix":"` + raw[:12] +
		`","raw":"` + raw + `","createdAt":"2026-01-01T00:00:00Z","revoked":false}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened := openAPIKeys()
	if _, ok := reopened.lookupRaw(raw); !ok {
		t.Fatal("legacy cleartext key was not migrated to a usable hash")
	}
	b2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b2), raw) {
		t.Fatal("migration left the cleartext in api-keys.json")
	}
}

func int64p(v int64) *int64 { return &v }
