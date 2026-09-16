package web

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

// Bootstrap password handling for a first start with no administrator password
// configured.
//
// An earlier revision bootstrapped with a well-known constant and relied on the
// mustChange gate to force a reset at first login. That is not a safe default:
// between the first start and the first login, anyone who can reach the port
// can authenticate with the published password and then call
// /api/admin/change-password - which the mustChange gate deliberately leaves
// open so a locked-out operator can recover - and thereby take the console
// over. Two requests, no credentials.
//
// A random password written to a 0600 file (and echoed to the log, the way
// Jenkins and Grafana handle an initial admin credential) removes the window
// entirely while keeping a fresh deployment usable.

const bootstrapPasswordLength = 24

// Character pools for the generated password. Ambiguous glyphs (0/O, 1/l/I)
// are omitted because the operator has to retype this by hand.
const (
	bootstrapLower  = "abcdefghijkmnopqrstuvwxyz"
	bootstrapUpper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	bootstrapDigit  = "23456789"
	bootstrapSymbol = "!@#$%^&*-_=+"
)

// bootstrapPasswordPath is where the generated cleartext is written. It lives
// beside the persisted password file so the same data-directory selection logic
// applies, and it is deleted as soon as the password is changed.
func bootstrapPasswordPath() string {
	if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "admin-password-bootstrap.txt")
	}
	primary, _ := adminPasswordPaths()
	return primary + ".bootstrap.txt"
}

func randIndex(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// generateBootstrapPassword returns a random password that already satisfies
// the console's strength policy, so an operator who keeps it is not worse off
// than one who replaces it.
func generateBootstrapPassword() (string, error) {
	mix := bootstrapLower + bootstrapUpper + bootstrapDigit + bootstrapSymbol
	out := make([]byte, bootstrapPasswordLength)
	// Guarantee one character from each class, then fill the rest uniformly.
	for i, pool := range []string{bootstrapLower, bootstrapUpper, bootstrapDigit, bootstrapSymbol} {
		idx, err := randIndex(len(pool))
		if err != nil {
			return "", err
		}
		out[i] = pool[idx]
	}
	for i := 4; i < len(out); i++ {
		idx, err := randIndex(len(mix))
		if err != nil {
			return "", err
		}
		out[i] = mix[idx]
	}
	// Shuffle so the class-mandated characters are not in fixed positions.
	for i := len(out) - 1; i > 0; i-- {
		j, err := randIndex(i + 1)
		if err != nil {
			return "", err
		}
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}

// bootstrapAdminPassword generates a random administrator password, persists
// its hash flagged as "must change", and writes the cleartext to
// bootstrapPasswordPath. It returns the hash to install in memory.
func bootstrapAdminPassword(reason string) (string, error) {
	plain, err := generateBootstrapPassword()
	if err != nil {
		return "", fmt.Errorf("generate bootstrap administrator password: %w", err)
	}
	hash, err := hashPassword(plain)
	if err != nil {
		return "", err
	}
	if err := saveAdminPasswordState(hash, nil, "", true); err != nil {
		// Refusing to start is the only safe outcome: without a persisted hash
		// the operator could never learn the generated password, and starting
		// with an unpersisted password would silently change on every restart.
		return "", fmt.Errorf(
			"no administrator password is configured and the generated bootstrap "+
				"password could not be persisted (set M365_ADMIN_PASSWORD to skip "+
				"bootstrap): %w", err)
	}
	path := bootstrapPasswordPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create data directory for bootstrap password: %w", err)
	}
	if err := os.WriteFile(path, []byte(plain+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write bootstrap password file: %w", err)
	}
	auditLog(nil, "admin_password_bootstrapped", reason+" (generated random password)")
	log.Printf("[admin] %s\n"+
		"[admin] generated a random administrator password: %s\n"+
		"[admin] it is also stored at %s (mode 0600); sign in and change it now",
		reason, plain, path)
	return hash, nil
}

// removeBootstrapPasswordFile deletes the cleartext once the password has been
// changed, so the generated secret does not outlive its purpose.
func removeBootstrapPasswordFile() {
	path := bootstrapPasswordPath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("[admin] could not remove bootstrap password file %s: %v", path, err)
	}
}
