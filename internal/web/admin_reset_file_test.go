package web

import (
	"os"
	"path/filepath"
	"testing"
)

// The DSM package hands the install wizard's password over through a file
// instead of M365_ADMIN_PASSWORD, so the cleartext never lands in
// /proc/<pid>/environ for the lifetime of the service process. The file must be
// consumed exactly once, and re-running an install with the same password must
// not undo a password the operator changed in the console afterwards.
func TestAdminPasswordResetFileIsConsumedOnce(t *testing.T) {
	dir := t.TempDir()
	reset := filepath.Join(dir, "admin-password-reset")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", filepath.Join(dir, "admin-password"))
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_RESET_FILE", reset)

	first := "Wizard!Str0ng#2026A"
	if err := os.WriteFile(reset, []byte(first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hash, mustChange, err := loadAdminPassword()
	if err != nil {
		t.Fatal(err)
	}
	if mustChange {
		t.Fatal("an install wizard password is an explicit choice; it must not force a change")
	}
	if !checkPassword(hash, first) {
		t.Fatal("wizard password was not applied")
	}
	if _, err := os.Stat(reset); !os.IsNotExist(err) {
		t.Fatal("the reset file must be deleted once consumed")
	}

	// A later restart with no reset file keeps the password.
	if hash2, _, err := loadAdminPassword(); err != nil {
		t.Fatal(err)
	} else if !checkPassword(hash2, first) {
		t.Fatal("password did not survive a restart")
	}

	// The operator changes the password in the console...
	second := "Console!Ch0sen#2026B"
	if err := saveAdminPassword(second); err != nil {
		t.Fatal(err)
	}
	changed, _, _, err := loadAdminCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(changed, second) {
		t.Fatal("console password change was not persisted")
	}

	// ...then reinstalls with the SAME wizard password. That must not silently
	// revert the console change.
	if err := os.WriteFile(reset, []byte(first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _, _, err := loadAdminCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(after, second) {
		t.Fatal("reinstalling with an unchanged wizard password reverted the console password")
	}
	if _, err := os.Stat(reset); !os.IsNotExist(err) {
		t.Fatal("an ignored reset file must still be consumed")
	}

	// A new wizard password does take effect.
	third := "Reinstall!Str0ng#2026C"
	if err := os.WriteFile(reset, []byte(third), 0o600); err != nil {
		t.Fatal(err)
	}
	final, _, _, err := loadAdminCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(final, third) {
		t.Fatal("a changed wizard password was not applied")
	}
}

// The retired constant must not be accepted through the reset file either.
func TestAdminPasswordResetFileRejectsRetiredDefault(t *testing.T) {
	dir := t.TempDir()
	reset := filepath.Join(dir, "admin-password-reset")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", filepath.Join(dir, "admin-password"))
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_RESET_FILE", reset)
	if err := os.WriteFile(reset, []byte(defaultAdminPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadAdminPassword(); err == nil {
		t.Fatal("expected the retired default password to be rejected")
	}
}

// An empty or absent reset file is not an error: the service must still start
// using whatever password is persisted.
func TestAdminPasswordResetFileAbsentIsHarmless(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", filepath.Join(dir, "admin-password"))
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD_RESET_FILE", filepath.Join(dir, "does-not-exist"))

	want := "Persisted!Str0ng#2026D"
	if err := saveAdminPassword(want); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "admin-password-reset"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, _, err := loadAdminCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(got, want) {
		t.Fatal("an empty reset file disturbed the persisted password")
	}
}
