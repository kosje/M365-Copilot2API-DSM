package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func adminTestClient(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	jar, _ := cookiejar.New(nil)
	c := ts.Client()
	c.Jar = jar
	t.Cleanup(ts.Close)
	return ts, c
}

func postJSON(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	r, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDefaultPasswordForcesChangeAndRotatesSessions(t *testing.T) {
	strong := "Str0ng!Passw0rd#2024"
	newStrong := "N3w!Str0ng#Passw0rd2025"
	t.Setenv("M365_ADMIN_PASSWORD", strong)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/admin-password")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())

	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"`+strong+`"}`)
	if r.StatusCode != 200 {
		t.Fatalf("login=%d", r.StatusCode)
	}
	var login map[string]any
	_ = json.NewDecoder(r.Body).Decode(&login)
	r.Body.Close()

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("protected status=%d want 200 got %d", 200, r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/change-password", `{"current_password":"`+strong+`","new_password":"`+newStrong+`"}`)
	if r.StatusCode != 200 {
		t.Fatalf("change=%d", r.StatusCode)
	}
	r.Body.Close()

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session status=%d", r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"`+newStrong+`"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new login=%d", r.StatusCode)
	}
	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new session status=%d", r.StatusCode)
	}
}

func TestAdminLoginLocksAfterFiveFailures(t *testing.T) {
	pw := "L0ck!T3st#Str0ng2024"
	t.Setenv("M365_ADMIN_PASSWORD", pw)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/admin-password-lock")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())
	for i := 0; i < 5; i++ {
		r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"wrong"}`)
		r.Body.Close()
		if r.StatusCode != 401 {
			t.Fatalf("attempt %d=%d", i+1, r.StatusCode)
		}
	}
	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"`+pw+`"}`)
	defer r.Body.Close()
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("locked=%d retry=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
}

func TestPersistedPasswordOverridesBootstrapEnv(t *testing.T) {
	path := t.TempDir() + "/admin-password"
	t.Setenv("M365_ADMIN_PASSWORD_FILE", path)
	t.Setenv("M365_ADMIN_PASSWORD", "old-bootstrap-password")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	persisted := "Persisted!Str0ng#2024"
	if err := saveAdminPassword(persisted); err != nil {
		t.Fatal(err)
	}
	got, _, err := loadAdminPassword()
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(got, persisted) {
		t.Fatalf("got hash does not match persisted password")
	}
}

func TestExpiredLoginWindowResets(t *testing.T) {
	s := &Server{loginAttempts: map[string]loginAttempt{"x": {Failures: 4, WindowStart: time.Now().Add(-16 * time.Minute)}}}
	if ok, _ := s.loginAllowed("x", time.Now()); !ok {
		t.Fatal("expired window remained locked")
	}
}

func TestLoadAdminPasswordRequiresConfig(t *testing.T) {
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/no-file")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_DATA_DIR", t.TempDir()+"/empty")
	t.Setenv("M365_REQUIRE_STRONG_ADMIN_PASSWORD", "1")
	_, _, err := loadAdminPassword()
	if err == nil {
		t.Fatal("expected error when no password configured and bootstrap disabled")
	}
}

// A first start with nothing configured must generate a random password, never
// fall back to the retired well-known constant. The old behaviour left the
// console takeable by anyone who could reach the port before the operator's
// first login, because the mustChange gate deliberately leaves
// /api/admin/change-password reachable.
func TestLoadAdminPasswordBootstrapsRandom(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/no-file")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_REQUIRE_STRONG_ADMIN_PASSWORD", "")
	hash, mustChange, err := loadAdminPassword()
	if err != nil {
		t.Fatal(err)
	}
	if !mustChange {
		t.Fatal("expected a first-start bootstrap to force a password change")
	}
	if checkPassword(hash, defaultAdminPassword) {
		t.Fatal("bootstrap must not accept the retired default password")
	}

	// The operator has to be able to learn the generated password, it must be
	// strong enough to keep, and it must actually open the stored hash.
	path := bootstrapPasswordPath()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("bootstrap password file: %v", err)
	}
	plain := strings.TrimSpace(string(b))
	if plain == "" {
		t.Fatal("bootstrap password file is empty")
	}
	if err := validNewAdminPassword(plain, nil); err != nil {
		t.Fatalf("generated bootstrap password fails the console strength policy: %v", err)
	}
	if !checkPassword(hash, plain) {
		t.Fatal("bootstrap password file does not match the stored hash")
	}
	// POSIX permission bits are only meaningful on the deployment target
	// (Linux/DSM); Windows reports 0666 for every writable file, so assert the
	// restriction only where it exists.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("bootstrap password file mode = %o, want 600", perm)
		}
	}

	// Two generated passwords in a row must differ, otherwise the "random"
	// bootstrap is not random.
	if other, gerr := generateBootstrapPassword(); gerr != nil {
		t.Fatal(gerr)
	} else if other == plain {
		t.Fatal("generateBootstrapPassword returned an identical password twice")
	}

	// mustChange must survive a restart; otherwise the generated password
	// silently becomes permanent after the first reboot.
	if _, again, err := loadAdminPassword(); err != nil {
		t.Fatal(err)
	} else if !again {
		t.Fatal("mustChange was not persisted; a restart would drop the gate")
	}
}

// An install whose persisted password is still the retired default must be
// upgraded to a random one rather than refusing to start (which is what the
// previous revision did), so an upgrade cannot brick a running service.
func TestLoadAdminPasswordReplacesPersistedDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/no-file")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_REQUIRE_STRONG_ADMIN_PASSWORD", "")

	stale, err := hashPassword(defaultAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveAdminPasswordState(stale, nil, "", false); err != nil {
		t.Fatal(err)
	}

	hash, mustChange, err := loadAdminPassword()
	if err != nil {
		t.Fatalf("a persisted default password must not block startup: %v", err)
	}
	if !mustChange {
		t.Fatal("replacing a persisted default password must force a change")
	}
	if checkPassword(hash, defaultAdminPassword) {
		t.Fatal("the retired default password is still accepted after migration")
	}
	if b, err := os.ReadFile(bootstrapPasswordPath()); err != nil {
		t.Fatalf("bootstrap password file: %v", err)
	} else if !checkPassword(hash, strings.TrimSpace(string(b))) {
		t.Fatal("bootstrap password file does not match the migrated hash")
	}
}

// An explicitly configured default password is a misconfiguration, not a state
// to migrate: fail loudly instead of silently generating something else.
func TestLoadAdminPasswordRejectsExplicitDefault(t *testing.T) {
	t.Setenv("M365_DATA_DIR", t.TempDir())
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/no-file")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD", defaultAdminPassword)
	if _, _, err := loadAdminPassword(); err == nil {
		t.Fatal("expected an explicitly configured default password to be rejected")
	}
}

func TestValidNewAdminPasswordStrength(t *testing.T) {
	if err := validNewAdminPassword("short1!A", nil); err == nil {
		t.Fatal("expected short password to fail")
	}
	if err := validNewAdminPassword("alllowercasepassword", nil); err == nil {
		t.Fatal("expected single class to fail")
	}
	if err := validNewAdminPassword("Password123!", nil); err == nil {
		t.Fatal("expected blacklisted to fail")
	}
	if err := validNewAdminPassword("Str0ng!Passw0rd#2024", nil); err != nil {
		t.Fatalf("expected strong password to pass, got %v", err)
	}
	historyPw := "Hist0ry!Str0ng#2024"
	h, _ := hashPassword(historyPw)
	if err := validNewAdminPassword(historyPw, []string{h}); err == nil {
		t.Fatal("expected history reuse to fail")
	}
}

func TestAdminPasswordHistoryBcrypt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_ADMIN_PASSWORD_FILE", dir+"/admin-password")
	t.Setenv("M365_ADMIN_PASSWORD", "Init!Str0ng#2024A")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_DATA_DIR", "")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())
	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"Init!Str0ng#2024A"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("login=%d", r.StatusCode)
	}
	newPw := "N3w!Str0ng#Pass2025B"
	r = postJSON(t, c, ts.URL+"/api/admin/change-password", `{"current_password":"Init!Str0ng#2024A","new_password":"`+newPw+`"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("change=%d", r.StatusCode)
	}
	r = postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"`+newPw+`"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("re-login=%d", r.StatusCode)
	}
	r = postJSON(t, c, ts.URL+"/api/admin/change-password", `{"current_password":"`+newPw+`","new_password":"Init!Str0ng#2024A"}`)
	defer r.Body.Close()
	if r.StatusCode != 400 {
		t.Fatalf("history reuse should be 400, got %d", r.StatusCode)
	}
}
