package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The /chat UI proxies to /v1 on behalf of a chat user. It used to do that by
// keeping the user's cleartext API key in users.json and putting it on a Bearer
// header. It now hands over the key *record* through the request context
// instead, so no usable credential has to be stored. These tests pin both
// halves of that contract: the injection is necessary (a headerless call is
// still rejected) and sufficient (the injected call gets through).
func TestChatProxyAuthWithoutCleartextKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_ADMIN_PASSWORD", "Init!Str0ng#2024A")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", dir+"/admin-password")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_API_KEYS", dir+"/api-keys.json")

	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec, raw, err := s.apiKeys.create("chat:alice")
	if err != nil {
		t.Fatal(err)
	}
	user := &chatUser{ID: "u1", Username: "alice", KeyID: rec.ID, KeyPrefix: rec.Prefix, Enabled: true}
	handler := s.Routes()
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	// Necessary: without a presented key and without in-process auth, /v1 must
	// reject the call. If this ever stops being true the injection is moot.
	bare := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	bare.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	handler.ServeHTTP(bw, bare)
	if bw.Code != http.StatusUnauthorized {
		t.Fatalf("headerless /v1 request = %d, want 401", bw.Code)
	}

	// Sufficient: the injected record gets past the middleware.
	req, err := s.internalChatRequest(user, "/v1/chat/completions", body)
	if err != nil {
		t.Fatal(err)
	}
	iw := httptest.NewRecorder()
	handler.ServeHTTP(iw, req)
	if iw.Code == http.StatusUnauthorized {
		t.Fatalf("internal request was rejected by the key middleware: %s", iw.Body.String())
	}

	// Attribution must land on the same 8-character prefix the usage log stores
	// for an externally presented key, otherwise per-key quotas would silently
	// stop counting chat traffic. Probe the middleware directly, since it injects
	// the record onto a new request rather than mutating the caller's.
	var seen string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = extractAPIKey(r)
		w.WriteHeader(http.StatusOK)
	})
	mw := s.adminMiddleware(probe)

	seen = ""
	iw2 := httptest.NewRecorder()
	mw.ServeHTTP(iw2, req)
	if iw2.Code != http.StatusOK {
		t.Fatalf("in-process request through middleware = %d", iw2.Code)
	}
	internalPrefix := seen

	external := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	external.Header.Set("Authorization", "Bearer "+raw)
	ew := httptest.NewRecorder()
	mw.ServeHTTP(ew, external)
	if ew.Code != http.StatusOK {
		t.Fatalf("external request through middleware = %d", ew.Code)
	}
	if internalPrefix == "" {
		t.Fatal("attribution prefix was empty for the in-process request")
	}
	if seen != internalPrefix {
		t.Fatalf("attribution differs: external=%q in-process=%q", seen, internalPrefix)
	}
	if internalPrefix != rec.Prefix[:8] {
		t.Fatalf("attribution prefix = %q, want %q", internalPrefix, rec.Prefix[:8])
	}

	// Revoking the key must stop the internal path as well.
	if _, err := s.apiKeys.revoke(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.internalChatRequest(user, "/v1/chat/completions", body); err == nil {
		t.Fatal("a revoked key still resolved for the chat proxy")
	}

	// The marker is scoped to /v1: it must never unlock an admin route.
	admin := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	admin = admin.WithContext(withInternalAuth(admin.Context(), rec))
	aw := httptest.NewRecorder()
	handler.ServeHTTP(aw, admin)
	if aw.Code != http.StatusUnauthorized {
		t.Fatalf("in-process auth reached an admin route: %d", aw.Code)
	}
}

// A chat user whose key has gone missing must fail loudly rather than fall back
// to an unauthenticated upstream call.
func TestInternalChatRequestRejectsMissingKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_ADMIN_PASSWORD", "Init!Str0ng#2024A")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", dir+"/admin-password")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_API_KEYS", dir+"/api-keys.json")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.internalChatRequest(&chatUser{ID: "u", Username: "bob"}, "/v1/chat/completions", nil); err == nil {
		t.Fatal("expected an error for a chat user with no KeyID")
	}
	if _, err := s.internalChatRequest(&chatUser{ID: "u", Username: "bob", KeyID: "does-not-exist"}, "/v1/chat/completions", nil); err == nil {
		t.Fatal("expected an error for a dangling KeyID")
	}
}

// users.json must not keep a cleartext key: an install upgrading from the
// revision that stored one is migrated to a KeyID reference on startup.
func TestMigrateChatUserKeysRewritesLegacyCleartext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_ADMIN_PASSWORD", "Init!Str0ng#2024A")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", dir+"/admin-password")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_API_KEYS", dir+"/api-keys.json")

	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec, raw, err := s.apiKeys.create("chat:carol")
	if err != nil {
		t.Fatal(err)
	}

	s.chatUI.mu.Lock()
	s.chatUI.Users = append(s.chatUI.Users, &chatUser{
		ID: "u3", Username: "carol", LegacyAPIKey: raw, KeyPrefix: rec.Prefix, Enabled: true,
	})
	s.chatUI.saveUsers()
	s.chatUI.mu.Unlock()

	s.migrateChatUserKeys()

	u := s.chatUI.user("u3")
	if u == nil {
		t.Fatal("user disappeared")
	}
	if u.LegacyAPIKey != "" {
		t.Fatalf("cleartext key survived in memory: %q", u.LegacyAPIKey)
	}
	if u.KeyID != rec.ID {
		t.Fatalf("KeyID = %q, want %q", u.KeyID, rec.ID)
	}
	b, err := os.ReadFile(filepath.Join(s.chatUI.Dir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), raw) {
		t.Fatal("users.json still contains the cleartext key")
	}
	// And the migrated user can still be proxied for.
	if _, err := s.internalChatRequest(u, "/v1/chat/completions", nil); err != nil {
		t.Fatalf("migrated user cannot be proxied: %v", err)
	}

	// Re-running the migration must be a no-op.
	s.migrateChatUserKeys()
	if s.chatUI.user("u3").KeyID != rec.ID {
		t.Fatal("migration is not idempotent")
	}
}
