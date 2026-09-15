package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type apiKeyRecord struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Prefix        string     `json:"prefix"`
	Hash          string     `json:"hash"`
	Raw           string     `json:"raw,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastUsedAt    *time.Time `json:"lastUsedAt,omitempty"`
	Revoked       bool       `json:"revoked"`
	DailyQuota    int64      `json:"dailyQuota,omitempty"`    // max requests/day, 0 = unlimited
	TotalQuota    int64      `json:"totalQuota,omitempty"`    // max requests all-time, 0 = unlimited
	PerMinuteRate int64      `json:"perMinuteRate,omitempty"` // max requests/minute, 0 = unlimited
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`     // nil = never expires
	// ModelWhitelist restricts which public models this key may request
	// (empty = all models allowed). IPWhitelist restricts client IPs
	// (entries may be exact IPs or CIDR ranges; empty = all IPs).
	ModelWhitelist []string `json:"modelWhitelist,omitempty"`
	IPWhitelist    []string `json:"ipWhitelist,omitempty"`
}

// keyRateWindow tracks per-minute request counts for rate-limited keys.
type keyRateWindow struct {
	mu      sync.Mutex
	counts  map[string]*minuteCounter
	nowFunc func() time.Time
}

type minuteCounter struct {
	minute int64 // unix minute bucket
	n      int64
}

func newKeyRateWindow() *keyRateWindow { return &keyRateWindow{counts: map[string]*minuteCounter{}, nowFunc: time.Now} }

// allow reports whether one more request is permitted for the key this minute.
// Unknown keys (no limit configured) are always allowed.
func (w *keyRateWindow) allow(keyID string, limit int64) bool {
	if limit <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	bucket := w.nowFunc().Unix() / 60
	c, ok := w.counts[keyID]
	if !ok || c.minute != bucket {
		if len(w.counts) > 4096 {
			for k, v := range w.counts {
				if v.minute != bucket {
					delete(w.counts, k)
				}
			}
		}
		c = &minuteCounter{minute: bucket}
		w.counts[keyID] = c
	}
	if c.n >= limit {
		return false
	}
	c.n++
	return true
}

// ipAllowed reports whether the client IP satisfies the key's IP whitelist.
// An empty whitelist allows everything. Entries may be exact IPs or CIDR.
func ipAllowed(client string, whitelist []string) bool {
	if len(whitelist) == 0 {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(client))
	if ip == nil {
		return false
	}
	for _, entry := range whitelist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		if allowed := net.ParseIP(entry); allowed != nil && allowed.Equal(ip) {
			return true
		}
	}
	return false
}

// modelAllowed reports whether the requested model satisfies the whitelist.
func modelAllowed(model string, whitelist []string) bool {
	if len(whitelist) == 0 {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(model))
	for _, w := range whitelist {
		if strings.ToLower(strings.TrimSpace(w)) == m {
			return true
		}
	}
	return false
}
type apiKeyStore struct {
	mu      sync.Mutex
	Path    string
	Keys    []apiKeyRecord `json:"keys"`
	persist *persistStore
}

func newAPIKeyStore(path string) *apiKeyStore {
	s := &apiKeyStore{Path: path}
	s.persist = &persistStore{flush: s.flush}
	return s
}

func openAPIKeys() *apiKeyStore {
	p := strings.TrimSpace(os.Getenv("M365_API_KEYS"))
	if p == "" {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, ".config", "m365-copilot2api", "api-keys.json")
	}
	s := newAPIKeyStore(p)
	b, e := os.ReadFile(p)
	if e == nil && json.Unmarshal(b, s) == nil {
		migrated := false
		for i := range s.Keys {
			if s.Keys[i].Raw != "" && s.Keys[i].Hash == "" {
				s.Keys[i].Hash = keyHash(s.Keys[i].Raw)
				migrated = true
			}
		}
		if migrated {
			_ = s.flush()
		}
	}
	return s
}
func (s *apiKeyStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(s.Path, b, 0600)
}
func keyHash(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }
func (s *apiKeyStore) create(name string) (apiKeyRecord, string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return apiKeyRecord{}, "", e
	}
	raw := "m365_" + hex.EncodeToString(b)
	r := apiKeyRecord{ID: hex.EncodeToString(b[:8]), Name: name, Prefix: raw[:12], Hash: keyHash(raw), Raw: raw, CreatedAt: time.Now()}
	s.mu.Lock()
	s.Keys = append(s.Keys, r)
	s.mu.Unlock()
	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		s.Keys = s.Keys[:len(s.Keys)-1]
		s.mu.Unlock()
		return apiKeyRecord{}, "", err
	}
	r.Hash = ""
	return r, raw, nil
}
func (s *apiKeyStore) list() []apiKeyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]apiKeyRecord, len(s.Keys))
	copy(out, s.Keys)
	for i := range out {
		out[i].Hash = ""
	}
	return out
}
func (s *apiKeyStore) revoke(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID == id && !s.Keys[i].Revoked {
			s.Keys[i].Revoked = true
			s.mu.Unlock()
			if err := s.persist.flushNowBlocking(); err != nil {
				s.mu.Lock()
				s.Keys[i].Revoked = false
				s.mu.Unlock()
				return false, err
			}
			return true, nil
		}
	}
	s.mu.Unlock()
	return false, nil
}

// delete physically removes a key record, rolling back on persistence failure.
func (s *apiKeyStore) delete(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		removed := s.Keys[i]
		s.Keys = append(s.Keys[:i], s.Keys[i+1:]...)
		s.mu.Unlock()
		if err := s.persist.flushNowBlocking(); err != nil {
			s.mu.Lock()
			s.Keys = append(s.Keys[:i], append([]apiKeyRecord{removed}, s.Keys[i:]...)...)
			s.mu.Unlock()
			return false, err
		}
		return true, nil
	}
	s.mu.Unlock()
	return false, nil
}

type keyUpdateOpts struct {
	Name           *string
	Revoked        *bool
	DailyQuota     *int64
	TotalQuota     *int64
	PerMinuteRate  *int64
	ExpiresAt      **time.Time // nil = leave; *nil = clear; non-nil = set
	ModelWhitelist *[]string
	IPWhitelist    *[]string
}

func (s *apiKeyStore) update(id string, o keyUpdateOpts) (bool, error) {
	s.mu.Lock()
	found := false
	var oldName string
	var oldRevoked bool
	var oldDaily, oldTotal, oldRate int64
	var oldExpires *time.Time
	var oldModels, oldIPs []string
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		oldName = s.Keys[i].Name
		oldRevoked = s.Keys[i].Revoked
		oldDaily = s.Keys[i].DailyQuota
		oldTotal = s.Keys[i].TotalQuota
		oldRate = s.Keys[i].PerMinuteRate
		oldExpires = s.Keys[i].ExpiresAt
		oldModels = s.Keys[i].ModelWhitelist
		oldIPs = s.Keys[i].IPWhitelist
		if o.Name != nil && *o.Name != "" {
			s.Keys[i].Name = *o.Name
		}
		if o.Revoked != nil {
			s.Keys[i].Revoked = *o.Revoked
		}
		if o.DailyQuota != nil {
			if *o.DailyQuota < 0 {
				*o.DailyQuota = 0
			}
			s.Keys[i].DailyQuota = *o.DailyQuota
		}
		if o.TotalQuota != nil {
			if *o.TotalQuota < 0 {
				*o.TotalQuota = 0
			}
			s.Keys[i].TotalQuota = *o.TotalQuota
		}
		if o.PerMinuteRate != nil {
			if *o.PerMinuteRate < 0 {
				*o.PerMinuteRate = 0
			}
			s.Keys[i].PerMinuteRate = *o.PerMinuteRate
		}
		if o.ExpiresAt != nil {
			s.Keys[i].ExpiresAt = *o.ExpiresAt
		}
		if o.ModelWhitelist != nil {
			s.Keys[i].ModelWhitelist = normalizeStringList(*o.ModelWhitelist)
		}
		if o.IPWhitelist != nil {
			s.Keys[i].IPWhitelist = normalizeStringList(*o.IPWhitelist)
		}
		found = true
		break
	}
	s.mu.Unlock()
	if !found {
		return false, nil
	}
	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		for i := range s.Keys {
			if s.Keys[i].ID == id {
				s.Keys[i].Name = oldName
				s.Keys[i].Revoked = oldRevoked
				s.Keys[i].DailyQuota = oldDaily
				s.Keys[i].TotalQuota = oldTotal
				s.Keys[i].PerMinuteRate = oldRate
				s.Keys[i].ExpiresAt = oldExpires
				s.Keys[i].ModelWhitelist = oldModels
				s.Keys[i].IPWhitelist = oldIPs
				break
			}
		}
		s.mu.Unlock()
		return false, err
	}
	return true, nil
}

// normalizeStringList trims entries and drops empties.
func normalizeStringList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lookupRaw resolves a presented key to its record (after validity check).
func (s *apiKeyStore) lookupRaw(raw string) (apiKeyRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := keyHash(raw)
	for i := range s.Keys {
		if s.Keys[i].Hash == h && !s.Keys[i].Revoked {
			return s.Keys[i], true
		}
	}
	return apiKeyRecord{}, false
}
func (s *apiKeyStore) valid(raw string) bool {
	s.mu.Lock()
	h := keyHash(raw)
	found := false
	for i := range s.Keys {
		if s.Keys[i].Hash == h && !s.Keys[i].Revoked {
			now := time.Now()
			s.Keys[i].LastUsedAt = &now
			found = true
			break
		}
	}
	s.mu.Unlock()
	if found {
		s.persist.markDirty()
	}
	return found
}
