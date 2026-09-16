package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// defaultAdminPassword is the RETIRED bootstrap password.
//
// It is no longer accepted: a first start with nothing configured now
// generates a random password (see admin_bootstrap.go). The constant is kept
// only so that installs still carrying it are detected and migrated away from
// it - see loadAdminCredentials. Do not reintroduce it as a fallback.
const defaultAdminPassword = "admin123"

type loginAttempt struct {
	Failures                 int
	WindowStart, LockedUntil time.Time
}

type adminPasswordData struct {
	Hash      string   `json:"hash"`
	History   []string `json:"history,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// MustChange records that this hash came from the random first-start
	// bootstrap rather than from an operator choice. It is persisted so the
	// console keeps forcing a change across restarts; any explicit password
	// change or env override clears it.
	MustChange bool `json:"must_change,omitempty"`
}

var commonPasswordBlacklist = []string{
	"password", "123456", "123456789", "qwerty", "admin", "admin123",
	"letmein", "welcome", "monkey", "dragon", "123123", "iloveyou",
	"password1", "1234", "12345", "adminadmin", "root", "toor",
}

func adminPasswordPaths() (primary, legacy string) {
	if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "admin-password.json"), filepath.Join(dir, "admin-password")
	}
	if p := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD_FILE")); p != "" {
		return p, ""
	}
	if p := strings.TrimSpace(os.Getenv("M365_CONFIG")); p != "" {
		return filepath.Join(filepath.Dir(p), "admin-password.json"), filepath.Join(filepath.Dir(p), "admin-password")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "m365-copilot2api", "admin-password.json"), filepath.Join(home, ".config", "m365-copilot2api", "admin-password")
}

func adminPasswordPath() string {
	primary, _ := adminPasswordPaths()
	return primary
}

// adminResetFlagPath returns the path of the operator-created reset flag file
// (<data dir>/admin-reset), or "" when the data dir is unknown.
func adminResetFlagPath() string {
	if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "admin-reset")
	}
	return ""
}

func isBcryptHash(s string) bool {
	return strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$") || strings.HasPrefix(s, "$2y$")
}

func hashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func checkPassword(hash, plain string) bool {
	if isBcryptHash(hash) {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(hash), []byte(plain)) == 1
}

func readPersistedAdminData() (adminPasswordData, bool) {
	primary, legacy := adminPasswordPaths()
	if b, err := os.ReadFile(primary); err == nil && strings.TrimSpace(string(b)) != "" {
		trimmed := strings.TrimSpace(string(b))
		var data adminPasswordData
		if err := json.Unmarshal([]byte(trimmed), &data); err == nil && data.Hash != "" {
			return data, true
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "{") {
			return adminPasswordData{}, false
		}
	}
	if legacy != "" && legacy != primary {
		if b, err := os.ReadFile(legacy); err == nil && strings.TrimSpace(string(b)) != "" {
			plain := strings.TrimSpace(string(b))
			if plain != "" {
				h, _ := hashPassword(plain)
				return adminPasswordData{Hash: h}, false
			}
		}
	} else {
		// when M365_ADMIN_PASSWORD_FILE points to non-json, primary is json variant, check legacy plain
		if b, err := os.ReadFile(legacy); err == nil && strings.TrimSpace(string(b)) != "" {
			plain := strings.TrimSpace(string(b))
			if plain != "" && !strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
				// treat as plain stored in the env-specified file
				// try parse as json first
				var data adminPasswordData
				if err := json.Unmarshal(b, &data); err == nil && data.Hash != "" {
					return data, true
				}
				h, _ := hashPassword(plain)
				return adminPasswordData{Hash: h}, false
			}
		}
	}
	return adminPasswordData{}, false
}

func loadAdminCredentials() (string, []string, bool, error) {
	primary, legacy := adminPasswordPaths()

	// Explicit reset path: operator drops an empty "admin-reset" file in the
	// data dir and provides a password via M365_ADMIN_PASSWORD (e.g. from the
	// fnOS install wizard). The file is consumed once; the password is
	// replaced even if a (forgotten) password was persisted before.
	if envP := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD")); envP != "" && envP != defaultAdminPassword {
		if flag := adminResetFlagPath(); flag != "" {
			if _, err := os.Stat(flag); err == nil {
				h, herr := hashPassword(envP)
				if herr == nil {
					if err := saveAdminPasswordWithHistory(h, nil, ""); err == nil {
						_ = os.Remove(flag)
						auditLog(nil, "admin_password_reset", "admin-reset flag consumed; password replaced from env")
						return h, nil, false, nil
					}
				}
			}
		}
	}

	// Install-wizard / env password: whenever M365_ADMIN_PASSWORD differs from
	// the last value applied from the environment (tracked in
	// <data dir>/admin-envhash), reset the administrator password to it.
	// Reinstalling or upgrading with a new wizard password therefore takes
	// effect immediately with no extra steps. An unchanged value (e.g. a
	// routine restart after the password was changed in the web UI) does NOT
	// override the web-set password.
	if envP := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD")); envP != "" && envP != defaultAdminPassword {
		if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
			marker := filepath.Join(dir, "admin-envhash")
			sum := sha256.Sum256([]byte(envP))
			mark := hex.EncodeToString(sum[:])
			changed := true
			if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == mark {
				changed = false
			}
			if changed {
				var prev adminPasswordData
				if bb, err := os.ReadFile(primary); err == nil {
					_ = json.Unmarshal([]byte(strings.TrimSpace(string(bb))), &prev)
				}
				if h, herr := hashPassword(envP); herr == nil {
					if err := saveAdminPasswordWithHistory(h, prev.History, prev.Hash); err == nil {
						_ = os.WriteFile(marker, []byte(mark), 0600)
						auditLog(nil, "admin_password_reset", "install wizard password applied (env value changed)")
						log.Printf("[admin] password reset from M365_ADMIN_PASSWORD (wizard/env value changed)")
						return h, prev.History, false, nil
					}
				}
			}
		}
	}

	if b, err := os.ReadFile(primary); err == nil && strings.TrimSpace(string(b)) != "" {
		trimmed := strings.TrimSpace(string(b))
		var data adminPasswordData
		if err := json.Unmarshal([]byte(trimmed), &data); err == nil && data.Hash != "" {
			if checkPassword(data.Hash, defaultAdminPassword) {
				if envP := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD")); envP != "" {
					if envP == defaultAdminPassword {
						return "", nil, false, errors.New("M365_ADMIN_PASSWORD must not be the retired default password; choose a strong one")
					}
					h, _ := hashPassword(envP)
					_ = saveAdminPasswordWithHistory(h, data.History, data.Hash)
					auditLog(nil, "admin_password_override_default", "env overrides persisted default")
					return h, data.History, false, nil
				}
				// A persisted default password with no env override means this
				// install predates the switch away from the constant bootstrap.
				// Replace it with a fresh random secret: the service stays
				// usable, but no published password remains reachable.
				h, err := bootstrapAdminPassword("persisted default administrator password replaced")
				if err != nil {
					return "", nil, false, err
				}
				return h, nil, true, nil
			}
			return data.Hash, data.History, data.MustChange, nil
		}
		plain := trimmed
		if plain == defaultAdminPassword {
			if envP := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD")); envP != "" {
				if envP == defaultAdminPassword {
					return "", nil, false, errors.New("M365_ADMIN_PASSWORD must not be the retired default password; choose a strong one")
				}
				h, _ := hashPassword(envP)
				_ = saveAdminPasswordWithHistory(h, nil, "")
				auditLog(nil, "admin_password_override_default", "env overrides persisted plain default")
				return h, nil, false, nil
			}
			h, err := bootstrapAdminPassword("persisted plain default administrator password replaced")
			if err != nil {
				return "", nil, false, err
			}
			return h, nil, true, nil
		}
		if plain != "" {
			h, _ := hashPassword(plain)
			// migrate to json
			_ = saveAdminPasswordWithHistory(h, nil, "")
			return h, nil, false, nil
		}
	}
	if legacy != "" && legacy != primary {
		if b, err := os.ReadFile(legacy); err == nil && strings.TrimSpace(string(b)) != "" {
			plain := strings.TrimSpace(string(b))
			if plain == defaultAdminPassword {
				if envP := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD")); envP != "" {
					if envP == defaultAdminPassword {
						return "", nil, false, errors.New("M365_ADMIN_PASSWORD must not be the retired default password; choose a strong one")
					}
					h, _ := hashPassword(envP)
					_ = saveAdminPasswordWithHistory(h, nil, "")
					auditLog(nil, "admin_password_override_default", "env overrides legacy default")
					return h, nil, false, nil
				}
				h, err := bootstrapAdminPassword("legacy default administrator password replaced")
				if err != nil {
					return "", nil, false, err
				}
				return h, nil, true, nil
			}
			if plain != "" {
				// check if it's already json (when env file is plain path but contains json)
				var data adminPasswordData
				if err := json.Unmarshal(b, &data); err == nil && data.Hash != "" {
					if checkPassword(data.Hash, defaultAdminPassword) {
						h, err := bootstrapAdminPassword("legacy JSON default administrator password replaced")
						if err != nil {
							return "", nil, false, err
						}
						return h, nil, true, nil
					}
					return data.Hash, data.History, data.MustChange, nil
				}
				h, _ := hashPassword(plain)
				_ = saveAdminPasswordWithHistory(h, nil, "")
				return h, nil, false, nil
			}
		}
	}
	if bootstrap := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE")); bootstrap != "" {
		if b, err := os.ReadFile(bootstrap); err == nil && strings.TrimSpace(string(b)) != "" {
			plain := strings.TrimSpace(string(b))
			if plain == defaultAdminPassword {
				return "", nil, false, errors.New("the retired default password must not be used in M365_ADMIN_PASSWORD_BOOTSTRAP_FILE")
			}
			if plain != "" {
				h, _ := hashPassword(plain)
				return h, nil, false, nil
			}
		}
	}
	if p := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD")); p != "" {
		if p == defaultAdminPassword {
			return "", nil, false, errors.New("M365_ADMIN_PASSWORD must not be the retired default password; choose a strong one")
		}
		h, err := hashPassword(p)
		if err != nil {
			return "", nil, false, err
		}
		return h, nil, false, nil
	}
	// Nothing is configured. Historically this branch bootstrapped with the
	// well-known constant "admin123" and relied on the mustChange gate, which
	// left the console takeable by anyone who could reach the port before the
	// operator's first login. Generate a random password instead and hand it
	// over once, through a 0600 file and the log.
	//
	// M365_REQUIRE_STRONG_ADMIN_PASSWORD=1 refuses to bootstrap at all, for
	// operators who would rather have a failed start than a generated secret.
	if strings.TrimSpace(os.Getenv("M365_REQUIRE_STRONG_ADMIN_PASSWORD")) == "1" {
		return "", nil, false, errors.New("administrator password is not configured and bootstrap is disabled (M365_REQUIRE_STRONG_ADMIN_PASSWORD=1); set M365_ADMIN_PASSWORD")
	}
	h, err := bootstrapAdminPassword("no administrator password configured")
	if err != nil {
		return "", nil, false, err
	}
	return h, nil, true, nil
}

func loadAdminPassword() (string, bool, error) {
	hash, _, mustChange, err := loadAdminCredentials()
	return hash, mustChange, err
}

func saveAdminPasswordWithHistory(newHash string, history []string, oldHash string) error {
	return saveAdminPasswordState(newHash, history, oldHash, false)
}

// saveAdminPasswordState persists the administrator password hash. mustChange
// marks the hash as bootstrapped: the console then refuses every admin surface
// except change-password until the operator picks their own password.
func saveAdminPasswordState(newHash string, history []string, oldHash string, mustChange bool) error {
	primary, _ := adminPasswordPaths()
	if err := os.MkdirAll(filepath.Dir(primary), 0700); err != nil {
		return err
	}
	newHistory := []string{}
	if oldHash != "" {
		newHistory = append(newHistory, oldHash)
	}
	newHistory = append(newHistory, history...)
	if len(newHistory) > 5 {
		newHistory = newHistory[:5]
	}
	data := adminPasswordData{Hash: newHash, History: newHistory, UpdatedAt: time.Now(), MustChange: mustChange}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeFileAtomic(primary, b, 0600)
}

func saveAdminPassword(password string) error {
	if err := validNewAdminPassword(password, nil); err != nil {
		// allow saving bootstrap passwords without strength check when called from load migration?
		// but enforce for explicit save via change-password; load path bypasses this
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	existing, ok := readPersistedAdminData()
	var history []string
	var oldHash string
	if ok {
		history = existing.History
		oldHash = existing.Hash
	} else {
		// try to get old hash via load credentials without env fallback to avoid env pollution
		primary, _ := adminPasswordPaths()
		if b, err := os.ReadFile(primary); err == nil {
			var d adminPasswordData
			if json.Unmarshal(b, &d) == nil && d.Hash != "" {
				history = d.History
				oldHash = d.Hash
			}
		}
	}
	return saveAdminPasswordWithHistory(hash, history, oldHash)
}

func auditLog(r *http.Request, event, detail string) {
	ip := ""
	if r != nil {
		ip = clientIP(r)
	}
	if detail != "" {
		log.Printf("[audit] event=%s ip=%s detail=%s", event, ip, detail)
	} else {
		log.Printf("[audit] event=%s ip=%s", event, ip)
	}
	// Persist to audit.jsonl (bounded in memory) for the console.
	globalAudit.append(auditEntry{Time: time.Now(), Event: event, IP: ip, Detail: detail})
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if net.ParseIP(host).IsLoopback() {
		parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
				return ip.String()
			}
		}
	}
	if host != "" {
		return host
	}
	return r.RemoteAddr
}
func validNewAdminPassword(p string, history []string) error {
	if p == defaultAdminPassword {
		return errors.New("new password must not be the default password")
	}
	if len(p) < 12 {
		return errors.New("new password must be at least 12 characters")
	}
	if len(p) > 72 {
		return errors.New("new password is too long (max 72 characters due to bcrypt limit)")
	}
	if len(p) > 256 {
		return errors.New("new password is too long")
	}
	classes := 0
	hasLower, hasUpper, hasDigit, hasSpecial := false, false, false, false
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		default:
			hasSpecial = true
		}
	}
	if hasLower {
		classes++
	}
	if hasUpper {
		classes++
	}
	if hasDigit {
		classes++
	}
	if hasSpecial {
		classes++
	}
	if classes < 3 {
		return errors.New("new password must contain at least 3 of 4 character classes (lower, upper, digit, special)")
	}
	lower := strings.ToLower(p)
	for _, bad := range commonPasswordBlacklist {
		if lower == bad || strings.Contains(lower, bad) {
			return errors.New("new password is too common or contains a blacklisted word")
		}
	}
	if isCommonSequence(p) {
		return errors.New("new password contains a common sequence or repeated characters")
	}
	if score := zxcvbnScore(p); score < 3 {
		return errors.New("new password is too weak (zxcvbn score < 3), choose a stronger password")
	}
	for _, h := range history {
		if h == "" {
			continue
		}
		if checkPassword(h, p) {
			return errors.New("new password was used recently, choose a different one")
		}
	}
	return nil
}

func isCommonSequence(p string) bool {
	lower := strings.ToLower(p)
	sequences := []string{"123456", "abcdef", "qwerty", "password", "admin", "letmein", "welcome", "111111", "000000"}
	for _, s := range sequences {
		if strings.Contains(lower, s) {
			return true
		}
	}
	// repeated characters like aaaa or 1111
	repeat := 1
	for i := 1; i < len(p); i++ {
		if p[i] == p[i-1] {
			repeat++
			if repeat >= 4 {
				return true
			}
		} else {
			repeat = 1
		}
	}
	// sequential run like abc, 123
	for i := 0; i < len(p)-2; i++ {
		a, b, c := p[i], p[i+1], p[i+2]
		if b == a+1 && c == b+1 {
			return true
		}
		if b == a-1 && c == b-1 {
			return true
		}
	}
	return false
}

func zxcvbnScore(p string) int {
	score := 0
	if len(p) >= 12 {
		score++
	}
	if len(p) >= 16 {
		score++
	}
	classes := 0
	hasLower, hasUpper, hasDigit, hasSpecial := false, false, false, false
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		default:
			hasSpecial = true
		}
	}
	if hasLower {
		classes++
	}
	if hasUpper {
		classes++
	}
	if hasDigit {
		classes++
	}
	if hasSpecial {
		classes++
	}
	if classes >= 3 {
		score++
	}
	if classes == 4 {
		score++
	}
	if isCommonSequence(p) {
		score -= 2
	}
	lower := strings.ToLower(p)
	for _, bad := range commonPasswordBlacklist {
		if strings.Contains(lower, bad) {
			score -= 2
			break
		}
	}
	if score < 0 {
		score = 0
	}
	if score > 4 {
		score = 4
	}
	return score
}

func (s *Server) loginAllowed(ip string, now time.Time) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.loginAttempts[ip]
	if now.Before(a.LockedUntil) {
		return false, time.Until(a.LockedUntil)
	}
	if a.WindowStart.IsZero() || now.Sub(a.WindowStart) > 15*time.Minute {
		delete(s.loginAttempts, ip)
	}
	return true, 0
}

const maxLoginAttemptEntries = 4096

func (s *Server) recordLoginFailure(ip string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.loginAttempts[ip]; !exists && len(s.loginAttempts) >= maxLoginAttemptEntries {
		for key, attempt := range s.loginAttempts {
			if now.Sub(attempt.WindowStart) > 15*time.Minute && now.After(attempt.LockedUntil) {
				delete(s.loginAttempts, key)
			}
		}
		if len(s.loginAttempts) >= maxLoginAttemptEntries {
			var oldestIP string
			var oldestStart time.Time
			for k, a := range s.loginAttempts {
				if oldestIP == "" || a.WindowStart.Before(oldestStart) {
					oldestIP, oldestStart = k, a.WindowStart
				}
			}
			if oldestIP != "" {
				delete(s.loginAttempts, oldestIP)
			}
		}
	}
	a := s.loginAttempts[ip]
	if a.WindowStart.IsZero() || now.Sub(a.WindowStart) > 15*time.Minute {
		a = loginAttempt{WindowStart: now}
	}
	a.Failures++
	if a.Failures >= 5 {
		a.LockedUntil = now.Add(15 * time.Minute)
	}
	s.loginAttempts[ip] = a
}
func (s *Server) clearLoginFailures(ip string) {
	s.mu.Lock()
	delete(s.loginAttempts, ip)
	s.mu.Unlock()
}
func (s *Server) adminChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, 401, "auth_error", "administrator login required")
		return
	}
	var b struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&b) != nil {
		auditLog(r, "admin_password_change_failed", "bad json")
		writeOpenAIError(w, 400, "invalid_request_error", "bad json")
		return
	}
	s.mu.Lock()
	currentHash := s.adminPassword
	history := append([]string{}, s.adminPasswordHistory...)
	s.mu.Unlock()
	if !checkPassword(currentHash, b.Current) {
		auditLog(r, "admin_password_change_failed", "current password invalid")
		writeOpenAIError(w, 401, "auth_error", "current password is invalid")
		return
	}
	allHistory := append([]string{currentHash}, history...)
	if err := validNewAdminPassword(b.New, allHistory); err != nil {
		auditLog(r, "admin_password_change_failed", err.Error())
		writeOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	newHash, err := hashPassword(b.New)
	if err != nil {
		auditLog(r, "admin_password_change_failed", "hash error")
		writeOpenAIError(w, 500, "storage_error", "administrator password could not be hashed")
		return
	}
	if err := saveAdminPasswordWithHistory(newHash, history, currentHash); err != nil {
		auditLog(r, "admin_password_change_failed", "save error: "+err.Error())
		writeOpenAIError(w, 500, "storage_error", "administrator password could not be saved; check the persistent data directory permissions")
		return
	}
	// The generated first-start password has served its purpose; do not leave
	// the cleartext lying around.
	removeBootstrapPasswordFile()
	s.mu.Lock()
	s.adminPassword = newHash
	newHist := []string{}
	if currentHash != "" {
		newHist = append(newHist, currentHash)
	}
	newHist = append(newHist, history...)
	if len(newHist) > 5 {
		newHist = newHist[:5]
	}
	s.adminPasswordHistory = newHist
	s.mustChangePassword = false
	s.adminSessions = map[string]time.Time{}
	s.mu.Unlock()
	auditLog(r, "admin_password_changed", "password changed successfully")
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Path: "/", HttpOnly: true, Secure: secureAdminCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	jsonOut(w, map[string]any{"status": "password_changed", "reauthenticate": true})
}
