package web

import (
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
	rec, _, err := store.create("test")
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
	got, ok := store.lookupRaw(mustRawKey(t, store, rec.ID))
	if !ok {
		t.Fatal("key not resolvable")
	}
	if got.PerMinuteRate != 30 || got.ExpiresAt == nil || len(got.ModelWhitelist) != 1 || len(got.IPWhitelist) != 1 {
		t.Fatalf("fields not persisted: %+v", got)
	}
}

// mustRawKey reads back the raw key material (only persisted when Raw != "").
func mustRawKey(t *testing.T, s *apiKeyStore, id string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.Keys {
		if k.ID == id && k.Raw != "" {
			return k.Raw
		}
	}
	t.Fatal("raw key not found (create persists Raw)")
	return ""
}

func int64p(v int64) *int64 { return &v }
