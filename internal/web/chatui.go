package web

// Chat UI ("测试或轻应用") — a small self-hosted chat frontend at /chat with:
//   - its own lightweight user accounts (separate from the admin console)
//   - per-user daily quotas (chat requests / image generations, 0 = unlimited)
//   - conversation persistence under <data>/chat/convs/<userID>/
//   - uploaded & generated image cache under <data>/chat/images/ (TTL)
//   - storage usage stats + retention config + janitor cleanup
// Chat requests are proxied in-process to s.openaiChat / s.imageGenerations
// using a per-user dedicated API key record (referenced by ID, never by
// cleartext), so the existing usage log attributes every request to the right
// user automatically.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	chatSessionCookie = "m365_chat_session"
	chatSessionTTL    = 7 * 24 * time.Hour
	maxChatUploadFile = 12 << 20 // per-image cap after base64 (~9MB raw)
	maxChatImages     = 4         // per message
	maxChatConvMsgs   = 300
)

type chatUser struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	// LegacyAPIKey is the cleartext gateway key an older revision stored here.
	// It is read only, so that installs upgrading from that revision can be
	// migrated to KeyID by Server.migrateChatUserKeys, and must never be
	// written again: users.json is a plain file, and the chat proxy no longer
	// needs the cleartext.
	LegacyAPIKey string     `json:"api_key,omitempty"`
	KeyID        string     `json:"key_id"` // apiKeyRecord.ID for revocation on user delete
	KeyPrefix    string     `json:"key_prefix"`
	DailyChat    int        `json:"daily_chat"`  // requests/day, 0 = unlimited
	DailyImage   int        `json:"daily_image"` // generations/day, 0 = unlimited
	Enabled      bool       `json:"enabled"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

type chatConfig struct {
	ConvRetentionDays   int `json:"conv_retention_days"`   // 0 = keep forever
	ImageRetentionHours int `json:"image_retention_hours"` // 0 = keep forever
	MaxStorageMB        int `json:"max_storage_mb"`        // 0 = unlimited
}

type chatMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"` // uploaded image file IDs
	Gen     []string `json:"gen_images,omitempty"` // generated image file IDs
	Model   string   `json:"model,omitempty"` // upstream model used for assistant replies
	Time    time.Time `json:"time"`
}

type chatConv struct {
	ID        string        `json:"id"`
	UserID    string        `json:"user_id"`
	Title     string        `json:"title"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Messages  []chatMessage `json:"messages"`
}

type chatQuota struct {
	Chat  int `json:"chat"`
	Image int `json:"image"`
}

type chatSession struct {
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type chatUIStore struct {
	mu       sync.Mutex
	Dir      string
	Users    []*chatUser
	Config   chatConfig
	Sessions map[string]chatSession
}

func chatDataDir() string {
	dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
	if dir == "" {
		h, _ := os.UserHomeDir()
		dir = filepath.Join(h, ".config", "m365-copilot2api")
	}
	return filepath.Join(dir, "chat")
}

func newChatUIStore() *chatUIStore {
	s := &chatUIStore{Dir: chatDataDir(), Sessions: map[string]chatSession{}}
	_ = os.MkdirAll(s.Dir, 0700)
	_ = os.MkdirAll(filepath.Join(s.Dir, "convs"), 0700)
	_ = os.MkdirAll(filepath.Join(s.Dir, "images"), 0700)
	s.Config = chatConfig{ConvRetentionDays: 7, ImageRetentionHours: 72, MaxStorageMB: 512}
	if b, err := os.ReadFile(filepath.Join(s.Dir, "config.json")); err == nil {
		_ = json.Unmarshal(b, &s.Config)
	}
	if b, err := os.ReadFile(filepath.Join(s.Dir, "users.json")); err == nil {
		_ = json.Unmarshal(b, &s.Users)
	}
	if b, err := os.ReadFile(filepath.Join(s.Dir, "sessions.json")); err == nil {
		_ = json.Unmarshal(b, &s.Sessions)
	}
	// drop expired sessions
	now := time.Now()
	for k, v := range s.Sessions {
		if v.ExpiresAt.Before(now) {
			delete(s.Sessions, k)
		}
	}
	return s
}

func (s *chatUIStore) saveUsers() {
	b, _ := json.MarshalIndent(s.Users, "", "  ")
	_ = writeFileAtomic(filepath.Join(s.Dir, "users.json"), b, 0600)
}

func (s *chatUIStore) saveConfig() {
	b, _ := json.MarshalIndent(s.Config, "", "  ")
	_ = writeFileAtomic(filepath.Join(s.Dir, "config.json"), b, 0600)
}

func (s *chatUIStore) saveSessions() {
	b, _ := json.MarshalIndent(s.Sessions, "", "  ")
	_ = writeFileAtomic(filepath.Join(s.Dir, "sessions.json"), b, 0600)
}

func (s *chatUIStore) user(id string) *chatUser {
	for _, u := range s.Users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func (s *chatUIStore) userByName(name string) *chatUser {
	for _, u := range s.Users {
		if strings.EqualFold(u.Username, name) {
			return u
		}
	}
	return nil
}

// ---------- quota ----------

func chatQuotaPath(t time.Time) string {
	return filepath.Join(chatDataDir(), "quota-"+t.Format("2006-01-02")+".json")
}

func readChatQuota(t time.Time) map[string]chatQuota {
	m := map[string]chatQuota{}
	if b, err := os.ReadFile(chatQuotaPath(t)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

var chatQuotaMu sync.Mutex

func bumpChatQuota(userID, kind string) chatQuota {
	chatQuotaMu.Lock()
	defer chatQuotaMu.Unlock()
	m := readChatQuota(time.Now())
	q := m[userID]
	if kind == "image" {
		q.Image++
	} else {
		q.Chat++
	}
	m[userID] = q
	b, _ := json.Marshal(m)
	_ = writeFileAtomic(chatQuotaPath(time.Now()), b, 0600)
	return q
}

func todayChatQuota(userID string) chatQuota {
	chatQuotaMu.Lock()
	defer chatQuotaMu.Unlock()
	return readChatQuota(time.Now())[userID]
}

// ---------- conversations ----------

func (s *chatUIStore) convPath(userID, convID string) string {
	return filepath.Join(s.Dir, "convs", userID, convID+".json")
}

func (s *chatUIStore) loadConv(userID, convID string) *chatConv {
	if convID == "" || strings.Contains(convID, "/") || strings.Contains(convID, "\\") || strings.Contains(convID, "..") {
		return nil
	}
	b, err := os.ReadFile(s.convPath(userID, convID))
	if err != nil {
		return nil
	}
	var c chatConv
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return &c
}

func (s *chatUIStore) saveConv(c *chatConv) {
	_ = os.MkdirAll(filepath.Join(s.Dir, "convs", c.UserID), 0700)
	if len(c.Messages) > maxChatConvMsgs {
		c.Messages = c.Messages[len(c.Messages)-maxChatConvMsgs:]
	}
	b, _ := json.Marshal(c)
	_ = writeFileAtomic(s.convPath(c.UserID, c.ID), b, 0600)
}

func (s *chatUIStore) listConvs(userID string) []map[string]any {
	out := []map[string]any{}
	dir := filepath.Join(s.Dir, "convs", userID)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	type item struct {
		id    string
		title string
		upd   time.Time
		n     int
	}
	var items []item
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		c := s.loadConv(userID, strings.TrimSuffix(e.Name(), ".json"))
		if c == nil {
			continue
		}
		items = append(items, item{c.ID, c.Title, c.UpdatedAt, len(c.Messages)})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].upd.After(items[j].upd) })
	for _, it := range items {
		out = append(out, map[string]any{"id": it.id, "title": it.title, "updatedAt": it.upd, "messages": it.n})
	}
	return out
}

// ---------- image cache ----------

func (s *chatUIStore) saveImage(data []byte, contentType string) (string, error) {
	ext := ".png"
	switch contentType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	}
	id := uuid.NewString() + ext
	p := filepath.Join(s.Dir, "images", id)
	if err := os.WriteFile(p, data, 0600); err != nil {
		return "", err
	}
	return id, nil
}

// saveReport persists a generated HTML report under <dir>/reports/<uuid>.html
// and returns the file id for /api/chatui/file/<id>.
func (s *chatUIStore) saveReport(data []byte) (string, error) {
	id := uuid.NewString() + ".html"
	dir := filepath.Join(s.Dir, "reports")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, id)
	if err := os.WriteFile(p, data, 0600); err != nil {
		return "", err
	}
	return id, nil
}

func (s *chatUIStore) openReport(id string) (*os.File, error) {
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return nil, os.ErrNotExist
	}
	if !strings.HasSuffix(strings.ToLower(id), ".html") {
		return nil, os.ErrNotExist
	}
	return os.Open(filepath.Join(s.Dir, "reports", id))
}

var imageExtTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".gif": "image/gif",
}

func (s *chatUIStore) openImage(id string) (*os.File, string, error) {
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return nil, "", os.ErrNotExist
	}
	ext := strings.ToLower(filepath.Ext(id))
	ct, ok := imageExtTypes[ext]
	if !ok {
		return nil, "", os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(s.Dir, "images", id))
	if err != nil {
		return nil, "", err
	}
	return f, ct, nil
}

// ---------- storage stats ----------

func dirStats(dir string) (files int64, bytes int64) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range ents {
		if e.IsDir() {
			f, b := dirStats(filepath.Join(dir, e.Name()))
			files += f
			bytes += b
			continue
		}
		if info, err := e.Info(); err == nil {
			files++
			bytes += info.Size()
		}
	}
	return files, bytes
}

// ---------- janitor ----------

func (s *chatUIStore) cleanupExpired() (removed int, freed int64) {
	now := time.Now()
	// conversations past retention
	if s.Config.ConvRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -s.Config.ConvRetentionDays)
		convsRoot := filepath.Join(s.Dir, "convs")
		userDirs, _ := os.ReadDir(convsRoot)
		for _, ud := range userDirs {
			if !ud.IsDir() {
				continue
			}
			uDir := filepath.Join(convsRoot, ud.Name())
			files, _ := os.ReadDir(uDir)
			for _, f := range files {
				p := filepath.Join(uDir, f.Name())
				if info, err := f.Info(); err == nil && info.ModTime().Before(cutoff) {
					if sz := info.Size(); os.Remove(p) == nil {
						removed++
						freed += sz
					}
				}
			}
		}
	}
	// images past TTL
	if s.Config.ImageRetentionHours > 0 {
		cutoff := now.Add(-time.Duration(s.Config.ImageRetentionHours) * time.Hour)
		imgs, _ := os.ReadDir(filepath.Join(s.Dir, "images"))
		for _, f := range imgs {
			if f.IsDir() {
				continue
			}
			p := filepath.Join(s.Dir, "images", f.Name())
			if info, err := f.Info(); err == nil && info.ModTime().Before(cutoff) {
				if sz := info.Size(); os.Remove(p) == nil {
					removed++
					freed += sz
				}
			}
		}
	}
	// enforce storage cap: delete oldest images first
	if s.Config.MaxStorageMB > 0 {
		limit := int64(s.Config.MaxStorageMB) << 20
		_, total := dirStats(s.Dir)
		for total > limit {
			oldest := ""
			var oldestTime time.Time
			imgs, _ := os.ReadDir(filepath.Join(s.Dir, "images"))
			for _, f := range imgs {
				if f.IsDir() {
					continue
				}
				if info, err := f.Info(); err == nil {
					if oldest == "" || info.ModTime().Before(oldestTime) {
						oldest = f.Name()
						oldestTime = info.ModTime()
					}
				}
			}
			if oldest == "" {
				break
			}
			p := filepath.Join(s.Dir, "images", oldest)
			if info, err := os.Stat(p); err == nil {
				if sz := info.Size(); os.Remove(p) == nil {
					removed++
					freed += sz
					total -= sz
					continue
				}
			}
			break
		}
	}
	// prune quota files older than 7 days, expired sessions
	qs, _ := filepath.Glob(filepath.Join(s.Dir, "quota-*.json"))
	for _, q := range qs {
		day := strings.TrimSuffix(filepath.Base(q), ".json")
		day = strings.TrimPrefix(day, "quota-")
		if t, err := time.Parse("2006-01-02", day); err == nil && now.Sub(t) > 7*24*time.Hour {
			_ = os.Remove(q)
		}
	}
	s.mu.Lock()
	for k, v := range s.Sessions {
		if v.ExpiresAt.Before(now) {
			delete(s.Sessions, k)
		}
	}
	s.saveSessions()
	s.mu.Unlock()
	return removed, freed
}

// ---------- auth helpers ----------

func (s *Server) chatAuth(r *http.Request) *chatUser {
	c, err := r.Cookie(chatSessionCookie)
	if err == nil && c.Value != "" {
		s.chatUI.mu.Lock()
		sess, ok := s.chatUI.Sessions[c.Value]
		s.chatUI.mu.Unlock()
		if ok && sess.ExpiresAt.After(time.Now()) {
			s.chatUI.mu.Lock()
			u := s.chatUI.user(sess.UserID)
			s.chatUI.mu.Unlock()
			if u != nil && u.Enabled {
				return u
			}
		}
	}
	return nil
}

// internalChatRequest builds an in-process request to a /v1 endpoint on behalf
// of a chat user, already authenticated.
//
// The user's key *record* travels through the request context rather than the
// cleartext being put on the wire, so the chat layer never needs to store a
// usable credential: users.json holds only KeyID (see internalAuthFrom and the
// /v1 branch of adminMiddleware).
func (s *Server) internalChatRequest(u *chatUser, target string, body []byte) (*http.Request, error) {
	if u == nil {
		return nil, fmt.Errorf("no chat user for internal request")
	}
	keyID := u.KeyID
	if keyID == "" && u.LegacyAPIKey != "" {
		// Not migrated yet (should not happen once New() has run).
		if id, ok := s.apiKeys.recordIDForKeyHash(u.LegacyAPIKey); ok {
			keyID = id
		}
	}
	if keyID == "" {
		return nil, fmt.Errorf("chat user %q has no API key configured", u.Username)
	}
	rec, ok := s.apiKeys.lookupByID(keyID)
	if !ok {
		return nil, fmt.Errorf("the API key for chat user %q is missing or revoked", u.Username)
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(withInternalAuth(req.Context(), rec)), nil
}

// migrateChatUserKeys replaces the cleartext key an older revision stored in
// users.json with a KeyID reference, and rewrites the file so the cleartext
// does not survive on disk.
func (s *Server) migrateChatUserKeys() {
	s.chatUI.mu.Lock()
	changed := false
	for _, u := range s.chatUI.Users {
		if u.LegacyAPIKey == "" {
			continue
		}
		if u.KeyID == "" {
			if id, ok := s.apiKeys.recordIDForKeyHash(u.LegacyAPIKey); ok {
				u.KeyID = id
			}
		}
		u.LegacyAPIKey = ""
		changed = true
	}
	if changed {
		s.chatUI.saveUsers()
	}
	s.chatUI.mu.Unlock()
	if changed {
		log.Printf("[chat] migrated chat users off stored cleartext API keys")
	}
}

// ---------- handlers ----------

func (s *Server) chatPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/chat" {
		http.Redirect(w, r, "/chat", http.StatusMovedPermanently)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	f, err := webContent.Open("chat.html")
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "chat interface unavailable")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "chat.html", st.ModTime(), f)
}

func (s *Server) chatLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&b) != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "bad json")
		return
	}
	s.chatUI.mu.Lock()
	u := s.chatUI.userByName(strings.TrimSpace(b.Username))
	s.chatUI.mu.Unlock()
	if u == nil || !u.Enabled || !checkPassword(u.PasswordHash, b.Password) {
		s.chatFail(w)
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		writeOpenAIError(w, 500, "internal_error", "token generation failed")
		return
	}
	token := hex.EncodeToString(buf)
	now := time.Now()
	s.chatUI.mu.Lock()
	s.chatUI.Sessions[token] = chatSession{UserID: u.ID, ExpiresAt: now.Add(chatSessionTTL)}
	u.LastLoginAt = &now
	s.chatUI.saveSessions()
	s.chatUI.saveUsers()
	s.chatUI.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: chatSessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(chatSessionTTL.Seconds())})
	jsonOut(w, map[string]any{"status": "ok", "username": u.Username})
}

func (s *Server) chatFail(w http.ResponseWriter) {
	time.Sleep(500 * time.Millisecond)
	writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "invalid username or password")
}

func (s *Server) chatSessionH(w http.ResponseWriter, r *http.Request) {
	u := s.chatAuth(r)
	if u == nil {
		jsonOut(w, map[string]any{"authenticated": false})
		return
	}
	q := todayChatQuota(u.ID)
	jsonOut(w, map[string]any{
		"authenticated": true,
		"username":      u.Username,
		"quota": map[string]any{
			"chat":  map[string]any{"used": q.Chat, "limit": u.DailyChat},
			"image": map[string]any{"used": q.Image, "limit": u.DailyImage},
		},
	})
}

func (s *Server) chatLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(chatSessionCookie); err == nil && c.Value != "" {
		s.chatUI.mu.Lock()
		delete(s.chatUI.Sessions, c.Value)
		s.chatUI.saveSessions()
		s.chatUI.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: chatSessionCookie, Value: "", Path: "/", MaxAge: -1})
	jsonOut(w, map[string]any{"status": "ok"})
}

func (s *Server) chatConvs(w http.ResponseWriter, r *http.Request) {
	u := s.chatAuth(r)
	if u == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "chat login required")
		return
	}
	if r.Method == http.MethodGet {
		jsonOut(w, map[string]any{"conversations": s.chatUI.listConvs(u.ID)})
		return
	}
	// POST delete / rename
	var b struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Title  string `json:"title"`
	}
	if json.NewDecoder(r.Body).Decode(&b) != nil || b.ID == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "bad json")
		return
	}
	if b.Action == "rename" {
		s.chatUI.mu.Lock()
		defer s.chatUI.mu.Unlock()
		c := s.chatUI.loadConv(u.ID, b.ID)
		if c == nil {
			writeOpenAIError(w, 404, "not_found", "conversation not found")
			return
		}
		t := strings.TrimSpace(b.Title)
		if t == "" {
			writeOpenAIError(w, 400, "invalid_request_error", "title required")
			return
		}
		r := []rune(t)
		if len(r) > 60 {
			r = r[:60]
		}
		c.Title = string(r)
		c.UpdatedAt = time.Now()
		s.chatUI.saveConv(c)
		jsonOut(w, map[string]any{"status": "renamed", "title": c.Title})
		return
	}
	s.chatUI.mu.Lock()
	defer s.chatUI.mu.Unlock()
	if c := s.chatUI.loadConv(u.ID, b.ID); c != nil {
		_ = os.Remove(s.chatUI.convPath(u.ID, b.ID))
	}
	jsonOut(w, map[string]any{"status": "deleted"})
}

// chatModels returns the model list available to the chat UI ("auto" first).
func (s *Server) chatModels(w http.ResponseWriter, r *http.Request) {
	u := s.chatAuth(r)
	if u == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "chat login required")
		return
	}
	type chatModel struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := []chatModel{{ID: "auto", Name: "智能路由（推荐）"}}
	for _, g := range gatewayModels {
		if g.ID == "gpt-image-2" {
			continue
		}
		name := g.DisplayName
		if name == "" {
			name = g.ID
		}
		out = append(out, chatModel{ID: g.ID, Name: name})
	}
	jsonOut(w, map[string]any{"models": out})
}

func (s *Server) chatConvGet(w http.ResponseWriter, r *http.Request) {
	u := s.chatAuth(r)
	if u == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "chat login required")
		return
	}
	id := r.URL.Query().Get("id")
	s.chatUI.mu.Lock()
	defer s.chatUI.mu.Unlock()
	c := s.chatUI.loadConv(u.ID, id)
	if c == nil {
		writeOpenAIError(w, 404, "not_found", "conversation not found")
		return
	}
	jsonOut(w, c)
}

// chatQuotaCheck returns remaining or ok for kind.
func (s *Server) chatQuotaCheck(u *chatUser, kind string) (bool, string) {
	limit := u.DailyChat
	if kind == "image" {
		limit = u.DailyImage
	}
	if limit <= 0 {
		return true, ""
	}
	q := todayChatQuota(u.ID)
	used := q.Chat
	if kind == "image" {
		used = q.Image
	}
	if used >= limit {
		return false, fmt.Sprintf("今日%s额度已用完（%d/%d），明天再试或联系管理员调整。", map[string]string{"chat": "对话", "image": "画图"}[kind], used, limit)
	}
	return true, ""
}

// ---------- chat proxy ----------

type teeWriter struct {
	http.ResponseWriter
	buf     bytes.Buffer
	lineBuf bytes.Buffer
	code    int
}

func (t *teeWriter) WriteHeader(c int) { t.code = c; t.ResponseWriter.WriteHeader(c) }

// Write streams to the client AND buffers a cleaned copy for persistence.
// SSE frames are line-delimited, so we strip internal markers at line
// granularity to avoid corrupting a JSON frame that spans write boundaries.
func (t *teeWriter) Write(p []byte) (int, error) {
	t.lineBuf.Write(p)
	for {
		idx := bytes.IndexByte(t.lineBuf.Bytes(), '\n')
		if idx < 0 {
			break
		}
		line := t.lineBuf.Next(idx + 1)
		cleaned := cleanInternalMarkers(string(line))
		t.buf.WriteString(cleaned)
		if _, err := t.ResponseWriter.Write([]byte(cleaned)); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// flushRemaining forwards any trailing bytes (a frame not terminated by '\n',
// e.g. a non-streaming JSON body) after the upstream handler returns.
func (t *teeWriter) flushRemaining() {
	if t.lineBuf.Len() == 0 {
		return
	}
	cleaned := cleanInternalMarkers(t.lineBuf.String())
	t.buf.WriteString(cleaned)
	_, _ = t.ResponseWriter.Write([]byte(cleaned))
	t.lineBuf.Reset()
}

func (t *teeWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// chatInternalMarkerRE strips tool-call / citation leakage (e.g. [cite][call_…]
// or bare call_<uuid>) that upstream models or the gateway occasionally emit
// verbatim into the chat body. publicInternalCitationPattern (defined in
// public_identity.go) covers the <cite>turnXsearchY</cite> and cite… forms
// and is applied unconditionally here regardless of the identity policy env.
var chatInternalMarkerRE = regexp.MustCompile(`(?i)\[/?cite\]|\[cite\]\[call_[0-9a-fA-F-]+\]|call_[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func cleanInternalMarkers(s string) string {
	s = publicInternalCitationPattern.ReplaceAllString(s, "")
	s = chatInternalMarkerRE.ReplaceAllString(s, "")
	return s
}

// dataURLFromFile loads a cached image as a data: URL for replay to the model.
func (s *chatUIStore) dataURLFromFile(id string) (string, bool) {
	f, ct, err := s.openImage(id)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(b), true
}

func (s *Server) chatProxy(w http.ResponseWriter, r *http.Request) {
	u := s.chatAuth(r)
	if u == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "chat login required")
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if ok, msg := s.chatQuotaCheck(u, "chat"); !ok {
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "read body")
		return
	}
	var in struct {
		ConversationID string          `json:"conversationId"`
		Model          string          `json:"model"`
		Stream         bool            `json:"stream"`
		Messages       json.RawMessage `json:"messages"`
		// Regenerate rewinds trailing assistant reply(ies) so the model can
		// answer the same last user message again; TruncateUser additionally
		// drops the last user message (edit & resend). History for both modes
		// is supplied by the client.
		Regenerate   bool `json:"regenerate"`
		TruncateUser bool `json:"truncateUser"`
	}
	if json.Unmarshal(raw, &in) != nil || len(in.Messages) == 0 {
		writeOpenAIError(w, 400, "invalid_request_error", "bad json")
		return
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = "auto"
	}

	// conversation bookkeeping + persist uploaded images
	s.chatUI.mu.Lock()
	conv := s.chatUI.loadConv(u.ID, in.ConversationID)
	if conv == nil {
		conv = &chatConv{ID: uuid.NewString(), UserID: u.ID, Title: "新对话", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	}
	// Regenerate / edit-resend: rewind the persisted conversation to match the
	// client-side history before the new reply (and possibly edited user
	// message) is appended below.
	if in.Regenerate || in.TruncateUser {
		for len(conv.Messages) > 0 && conv.Messages[len(conv.Messages)-1].Role == "assistant" {
			conv.Messages = conv.Messages[:len(conv.Messages)-1]
		}
		if in.TruncateUser && len(conv.Messages) > 0 && conv.Messages[len(conv.Messages)-1].Role == "user" {
			conv.Messages = conv.Messages[:len(conv.Messages)-1]
		}
	}
	var userMsg chatMessage
	var upMsgs []any
	if err := json.Unmarshal(in.Messages, &upMsgs); err != nil {
		s.chatUI.mu.Unlock()
		writeOpenAIError(w, 400, "invalid_request_error", "bad messages")
		return
	}
	// only persist the last user message (history comes from the client)
	lastUser := -1
	for i := len(upMsgs) - 1; i >= 0; i-- {
		if m, ok := upMsgs[i].(map[string]any); ok && m["role"] == "user" {
			lastUser = i
			break
		}
	}
	if lastUser >= 0 {
		if m, ok := upMsgs[lastUser].(map[string]any); ok {
			text, imgs := s.persistChatUserMessage(m)
			userMsg = chatMessage{Role: "user", Content: text, Images: imgs, Time: time.Now()}
			if conv.Title == "新对话" && text != "" {
				t := []rune(strings.TrimSpace(text))
				if len(t) > 40 {
					t = t[:40]
				}
				conv.Title = string(t)
			}
		}
	}
	conv.Messages = append(conv.Messages, userMsg)
	s.chatUI.saveConv(conv)
	convID := conv.ID
	s.chatUI.mu.Unlock()

	// build upstream request
	upBody, _ := json.Marshal(map[string]any{"model": model, "stream": in.Stream, "messages": upMsgs})
	url := "/v1/chat/completions"
	if in.Stream {
		url += "?stream=true"
	}
	upReq, reqErr := s.internalChatRequest(u, url, upBody)
	if reqErr != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "configuration_error", reqErr.Error())
		return
	}
	// Tag the request so openaiChat can record the M365 conversation context
	// needed to later re-emit and proxy any generated files.
	upReq.Header.Set("X-M365-Internal-Conv", convID)
	tw := &teeWriter{ResponseWriter: w, code: http.StatusOK}
	s.openaiChat(tw, upReq)
	tw.flushRemaining()

	// extract assistant text (and any upstream-generated images) and persist
	if tw.code == http.StatusOK {
		text := extractUpstreamText(tw.buf.Bytes(), in.Stream)
		// Rewrite unreachable Microsoft asyncgw file links to local proxy URLs
		// so the user can actually download generated PDF/Word/Excel/PPTX/ZIP.
		text = s.rewriteAsyncGWToProxy(text, convID)
		var gen []string
		var failed []string
		for _, iu := range extractUpstreamImages(tw.buf.Bytes(), in.Stream) {
			b64, ct, err := downloadImageAsBase64(iu)
			if err != nil {
				log.Printf("[chatui] upstream chat image download failed: %v", err)
				failed = append(failed, iu)
				continue
			}
			rawData, err := base64.StdEncoding.DecodeString(b64)
			if err != nil || len(rawData) == 0 {
				failed = append(failed, iu)
				continue
			}
			if id, err := s.chatUI.saveImage(rawData, ct); err == nil {
				gen = append(gen, id)
			} else {
				failed = append(failed, iu)
			}
		}
		// Images the server could not download (CDN unreachable) fall back to
		// inline markdown URLs, which the web chat renders as <img>.
		for _, iu := range failed {
			text += "\n\n![](" + iu + ")\n\n[⬇ 图片原地址（若上图未显示，点此自行下载）](" + iu + ")"
		}
		if text != "" || len(gen) > 0 {
			// When the per-key auto pool resolved "auto" to a concrete model,
			// persist that model so the history badge shows what actually ran.
			persistModel := model
			if am := strings.TrimSpace(tw.Header().Get("X-M365-Auto-Model")); am != "" {
				persistModel = am
			}
			s.chatUI.mu.Lock()
			if c := s.chatUI.loadConv(u.ID, convID); c != nil {
				c.Messages = append(c.Messages, chatMessage{Role: "assistant", Content: text, Gen: gen, Model: persistModel, Time: time.Now()})
				c.UpdatedAt = time.Now()
				s.chatUI.saveConv(c)
			}
			s.chatUI.mu.Unlock()
		}
		bumpChatQuota(u.ID, "chat")
	}
	_ = userMsg
}

// persistChatUserMessage extracts text + image parts from an OpenAI-style user
// message, saves images into the cache, and returns (text, fileIDs). The map is
// left untouched for upstream use (data URLs are forwarded as-is).
func (s *Server) persistChatUserMessage(m map[string]any) (string, []string) {
	text := ""
	var imgs []string
	switch c := m["content"].(type) {
	case string:
		text = c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				if t, ok := pm["text"].(string); ok {
					sb.WriteString(t)
				}
			case "image_url":
				if iu, ok := pm["image_url"].(map[string]any); ok {
					if u, ok := iu["url"].(string); ok && strings.HasPrefix(u, "data:image/") {
						if data, err := base64.StdEncoding.DecodeString(strings.SplitN(u, ",", 2)[1]); err == nil {
							ct := "image/png"
							if idx := strings.Index(u, ";"); idx > 5 {
								ct = u[5:idx]
							}
							if id, err := s.chatUI.saveImage(data, ct); err == nil && len(imgs) < maxChatImages {
								imgs = append(imgs, id)
							}
						}
					}
				}
			}
		}
		text = sb.String()
	}
	return text, imgs
}

// extractUpstreamText pulls the assistant text out of an OpenAI response
// (streaming SSE or plain JSON).
// extractUpstreamImages pulls generated-image URLs out of an openaiChat
// response buffer. Stream mode carries them in the terminal chunk ("images"
// top-level field); non-stream JSON exposes the same field.
func extractUpstreamImages(b []byte, stream bool) []string {
	var urls []string
	if stream {
		for _, line := range strings.Split(string(b), "\\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			p := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if p == "" || p == "[DONE]" {
				continue
			}
			var chunk struct {
				Images []string `json:"images"`
			}
			if json.Unmarshal([]byte(p), &chunk) == nil && len(chunk.Images) > 0 {
				urls = append(urls, chunk.Images...)
			}
		}
	} else {
		var out struct {
			Images []string `json:"images"`
		}
		if json.Unmarshal(b, &out) == nil {
			urls = out.Images
		}
	}
	return urls
}

func extractUpstreamText(b []byte, stream bool) string {
	if len(b) == 0 {
		return ""
	}
	if !stream {
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(b, &out) == nil && len(out.Choices) > 0 {
			return cleanInternalMarkers(out.Choices[0].Message.Content)
		}
		return ""
	}
	var sb strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Choices) > 0 {
			sb.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	return cleanInternalMarkers(sb.String())
}

// ---------- image generation ----------

func (s *Server) chatImageGen(w http.ResponseWriter, r *http.Request) {
	u := s.chatAuth(r)
	if u == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "chat login required")
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if ok, msg := s.chatQuotaCheck(u, "image"); !ok {
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}
	var in struct {
		ConversationID string `json:"conversationId"`
		Prompt         string `json:"prompt"`
		Size           string `json:"size"`
		Count          int    `json:"count"`
		Style          string `json:"style"`
		Negative       string `json:"negative"`
		Quality        string `json:"quality"`
		EditImage      string `json:"editImage"` // data URI of an uploaded image → 图生图 (edit) mode
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImageEditRequestBytes)).Decode(&in) != nil || strings.TrimSpace(in.Prompt) == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "prompt is required")
		return
	}
	size := normalizeImageSize(in.Size)
	count := in.Count
	if count <= 0 {
		count = 1
	}
	if count > 4 {
		count = 4
	}
	// 图生图：带编辑底图时走 upstream edit 操作（保持原图未提及部分）。
	isEdit := strings.HasPrefix(strings.ToLower(strings.TrimSpace(in.EditImage)), "data:image/")
	finalPrompt := composeImagePrompt(in.Prompt, in.Style, in.Negative, in.Quality)

	s.chatUI.mu.Lock()
	conv := s.chatUI.loadConv(u.ID, in.ConversationID)
	if conv == nil {
		conv = &chatConv{ID: uuid.NewString(), UserID: u.ID, Title: "新对话", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	}
	conv.Messages = append(conv.Messages, chatMessage{Role: "user", Content: "🎨 " + imageUserDisplay(in.Prompt, size, in.Style, in.Quality, count, isEdit), Time: time.Now()})
	if conv.Title == "新对话" {
		t := []rune("🎨 " + in.Prompt)
		if len(t) > 40 {
			t = t[:40]
		}
		conv.Title = string(t)
	}
	convID := conv.ID
	s.chatUI.saveConv(conv)
	s.chatUI.mu.Unlock()

	upBody, _ := json.Marshal(map[string]any{
		"prompt": finalPrompt, "n": 1, "size": size,
		"response_format": "b64_json", "model": "gpt-image-2",
		"operation": func() string {
			if isEdit {
				return "edit"
			}
			return ""
		}(),
	})
	if isEdit {
		upBody, _ = json.Marshal(map[string]any{
			"prompt": finalPrompt, "n": 1, "size": size,
			"response_format": "b64_json", "model": "gpt-image-2",
			"operation": "edit",
			"attachments": []map[string]string{{
				"type": "image", "url": strings.TrimSpace(in.EditImage),
				"name": "edit.png", "mimeType": "image/png",
			}},
		})
	}
	// 多图循环生成：每张独立调用一次上游（上游单次只稳定返回一张）。
	// 第一张失败 → 整体报错；后续失败 → 已成功的照常返回，附带失败说明。
	type genResult struct {
		fileID string
		err    string
	}
	results := make([]genResult, 0, count)
	for i := 0; i < count; i++ {
		upReq, reqErr := s.internalChatRequest(u, "/v1/images/generations", upBody)
		if reqErr != nil {
			if i == 0 {
				writeOpenAIError(w, http.StatusServiceUnavailable, "configuration_error", reqErr.Error())
				return
			}
			results = append(results, genResult{err: reqErr.Error()})
			continue
		}
		cw := &chatCaptureWriter{header: http.Header{}, buf: bytes.Buffer{}, code: http.StatusOK}
		s.imageGenerations(cw, upReq)
		if cw.code != http.StatusOK {
			if i == 0 {
				w.Header().Set("Content-Type", "application/json")
				status := cw.code
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = w.Write(cw.buf.Bytes())
				return
			}
			results = append(results, genResult{err: fmt.Sprintf("第 %d 张生成失败（HTTP %d）", i+1, cw.code)})
			continue
		}
		var out struct {
			Data []struct {
				B64 string `json:"b64_json"`
				URL string `json:"url"`
			} `json:"data"`
			Warning string `json:"warning"`
		}
		if json.Unmarshal(cw.buf.Bytes(), &out) != nil || len(out.Data) == 0 {
			if i == 0 {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "no image returned")
				return
			}
			results = append(results, genResult{err: fmt.Sprintf("第 %d 张未返回图片", i+1)})
			continue
		}
		// 兜底：图片已在上游生成，但服务器下载失败（设备可能无代理直连图片 CDN）。
		if out.Data[0].B64 == "" && out.Data[0].URL != "" {
			if i == 0 {
				var sb strings.Builder
				sb.WriteString("⚠️ 图片已生成，但服务器下载图片失败（设备可能无法直连图片 CDN，需要代理）。请自行点击下面的链接下载：\n")
				for j, d := range out.Data {
					if d.URL != "" {
						sb.WriteString(fmt.Sprintf("\n[下载图片 %d](%s)", j+1, d.URL))
					}
				}
				s.chatUI.mu.Lock()
				if c := s.chatUI.loadConv(u.ID, convID); c != nil {
					c.Messages = append(c.Messages, chatMessage{Role: "assistant", Content: cleanInternalMarkers(sb.String()), Time: time.Now()})
					c.UpdatedAt = time.Now()
					s.chatUI.saveConv(c)
				}
				s.chatUI.mu.Unlock()
				urls := make([]string, 0, len(out.Data))
				for _, d := range out.Data {
					if d.URL != "" {
						urls = append(urls, d.URL)
					}
				}
				jsonOut(w, map[string]any{"status": "ok", "conversationId": convID, "warning": "download_failed", "urls": urls})
				return
			}
			if u := out.Data[0].URL; u != "" {
				results = append(results, genResult{err: fmt.Sprintf("第 %d 张下载失败，可点击原地址自行下载：%s", i+1, u)})
			} else {
				results = append(results, genResult{err: fmt.Sprintf("第 %d 张下载失败", i+1)})
			}
			continue
		}
		if out.Data[0].B64 == "" {
			if i == 0 {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "no image returned")
				return
			}
			results = append(results, genResult{err: fmt.Sprintf("第 %d 张未返回图片", i+1)})
			continue
		}
		img, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
		if err != nil || len(img) == 0 {
			if i == 0 {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "invalid image data")
				return
			}
			results = append(results, genResult{err: fmt.Sprintf("第 %d 张数据无效", i+1)})
			continue
		}
		id, err := s.chatUI.saveImage(img, "image/png")
		if err != nil {
			if i == 0 {
				writeOpenAIError(w, 500, "internal_error", "save image failed")
				return
			}
			results = append(results, genResult{err: fmt.Sprintf("第 %d 张保存失败", i+1)})
			continue
		}
		results = append(results, genResult{fileID: id})
	}
	genIDs := make([]string, 0, len(results))
	outURLs := make([]string, 0, len(results))
	failures := make([]string, 0)
	for _, r := range results {
		if r.fileID != "" {
			genIDs = append(genIDs, r.fileID)
			outURLs = append(outURLs, "/api/chatui/file/"+r.fileID)
		} else {
			failures = append(failures, r.err)
		}
	}
	if len(genIDs) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "no image returned")
		return
	}
	s.chatUI.mu.Lock()
	if c := s.chatUI.loadConv(u.ID, convID); c != nil {
		c.Messages = append(c.Messages, chatMessage{Role: "assistant", Content: "", Gen: genIDs, Model: "gpt-image-2", Time: time.Now()})
		c.UpdatedAt = time.Now()
		s.chatUI.saveConv(c)
	}
	s.chatUI.mu.Unlock()
	for range genIDs {
		bumpChatQuota(u.ID, "image") // 每张成功图片各计一次画图配额
	}
	out := map[string]any{"status": "ok", "conversationId": convID, "urls": outURLs, "url": outURLs[0]}
	if len(failures) > 0 {
		out["partial"] = failures
	}
	jsonOut(w, out)
}

// chatCaptureWriter buffers the whole response (used for in-process non-stream calls).
type chatCaptureWriter struct {
	header http.Header
	buf    bytes.Buffer
	code   int
}

func (c *chatCaptureWriter) Header() http.Header { return c.header }
func (c *chatCaptureWriter) WriteHeader(code int) { c.code = code }
func (c *chatCaptureWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *chatCaptureWriter) Flush() {}

// chatReport saves an assistant-generated HTML report server-side so the user
// can open it as a standalone page (tables can be copied into Excel etc.).
func (s *Server) chatReport(w http.ResponseWriter, r *http.Request) {
	u := s.chatAuth(r)
	if u == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "chat login required")
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var in struct {
		HTML string `json:"html"`
		Name string `json:"name"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20)).Decode(&in) != nil || strings.TrimSpace(in.HTML) == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "html is required")
		return
	}
	doc := strings.TrimSpace(in.HTML)
	lower := strings.ToLower(doc)
	if !strings.Contains(lower, "<!doctype html") && !strings.Contains(lower, "<html") {
		// fragment: wrap into a minimal document
		doc = "<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>" + strings.ReplaceAll(strings.TrimSpace(in.Name), "<", "") + "</title><style>body{font-family:system-ui,-apple-system,'Segoe UI','Microsoft YaHei',sans-serif;margin:24px;line-height:1.6;color:#1f2328}table{border-collapse:collapse;margin:12px 0}th,td{border:1px solid #d0d7de;padding:6px 12px}th{background:#f6f8fa}tr:nth-child(even){background:#fafbfc}h1,h2,h3{margin:16px 0 8px}</style></head><body>\n" + doc + "\n</body></html>"
	}
	id, err := s.chatUI.saveReport([]byte(doc))
	if err != nil {
		writeOpenAIError(w, 500, "internal_error", "save report failed")
		return
	}
	jsonOut(w, map[string]any{"status": "ok", "id": id, "url": "/api/chatui/file/" + id})
}

// chatFile serves a cached image, or a generated HTML report.
//
// Access is by unguessable identifier only: image and report ids are UUIDs
// (saveImage/saveReport) and there is no owner index, so any logged-in chat user
// who obtains an id can fetch the bytes. That is a deliberate capability model,
// not an oversight - building a per-file owner index would mean threading a user
// id through every save site. Treat these ids as secrets: they must not be
// logged, put in an outbound URL as a query parameter, or shown in a screenshot.
// Anything that does need an ownership check should follow chatFileProxy, which
// verifies the conversation belongs to the caller.
func (s *Server) chatFile(w http.ResponseWriter, r *http.Request) {
	u := s.chatAuth(r)
	if u == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "chat login required")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/chatui/file/")
	f, ct, err := s.chatUI.openImage(id)
	if err != nil {
		// reports (generated HTML) live in a separate directory
		if rf, rerr := s.chatUI.openReport(id); rerr == nil {
			defer rf.Close()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// Opaque-origin sandbox: scripts/links run but the page cannot
			// touch chat cookies, storage or same-origin APIs.
			w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-popups allow-forms allow-modals")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeContent(w, r, id, time.Now(), rf)
			return
		}
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, id, time.Now(), f)
}

// ---------- admin: users / storage / config ----------

func (s *Server) chatAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	if r.Method == http.MethodGet {
		// overview: users + quota today + storage stats + config
		s.chatUI.mu.Lock()
		defer s.chatUI.mu.Unlock()
		users := make([]map[string]any, 0, len(s.chatUI.Users))
		q := readChatQuota(time.Now())
		for _, u := range s.chatUI.Users {
			uu := map[string]any{
				"id": u.ID, "username": u.Username, "enabled": u.Enabled,
				"dailyChat": u.DailyChat, "dailyImage": u.DailyImage,
				"createdAt": u.CreatedAt, "keyPrefix": u.KeyPrefix,
				"quotaToday": q[u.ID],
			}
			if u.LastLoginAt != nil {
				uu["lastLoginAt"] = *u.LastLoginAt
			}
			users = append(users, uu)
		}
		convsFiles, convsBytes := dirStats(filepath.Join(s.chatUI.Dir, "convs"))
		imgFiles, imgBytes := dirStats(filepath.Join(s.chatUI.Dir, "images"))
		jsonOut(w, map[string]any{
			"users": users,
			"storage": map[string]any{
				"convsFiles": convsFiles, "convsBytes": convsBytes,
				"imageFiles": imgFiles, "imageBytes": imgBytes,
			},
			"config": s.chatUI.Config,
		})
		return
	}
	// POST actions
	var b struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		// create/update fields
		Username   string `json:"username"`
		Password   string `json:"password"`
		DailyChat  *int   `json:"dailyChat"`
		DailyImage *int   `json:"dailyImage"`
		Enabled    *bool  `json:"enabled"`
		// config fields
		ConvRetentionDays   *int `json:"convRetentionDays"`
		ImageRetentionHours *int `json:"imageRetentionHours"`
		MaxStorageMB        *int `json:"maxStorageMB"`
		Scope               string `json:"scope"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b) != nil || b.Action == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "bad json")
		return
	}
	// cleanup runs its own locking; handle it before taking the store lock
	// (cleanupExpired locks s.mu internally for session pruning).
	if b.Action == "cleanup" {
		scope := b.Scope
		if scope == "" {
			scope = "expired"
		}
		if scope == "expired" || scope == "images" {
			n, freed := s.chatUI.cleanupExpired()
			log.Printf("[chat-cleanup] scope=%s removed=%d freed=%d", scope, n, freed)
			jsonOut(w, map[string]any{"status": "ok", "removed": n, "freedBytes": freed})
			return
		}
		if scope == "all-images" {
			imgs, _ := os.ReadDir(filepath.Join(s.chatUI.Dir, "images"))
			removed, freed := 0, int64(0)
			for _, f := range imgs {
				if f.IsDir() {
					continue
				}
				p := filepath.Join(s.chatUI.Dir, "images", f.Name())
				if info, err := f.Info(); err == nil {
					if sz := info.Size(); os.Remove(p) == nil {
						removed++
						freed += sz
					}
				}
			}
			jsonOut(w, map[string]any{"status": "ok", "removed": removed, "freedBytes": freed})
			return
		}
		writeOpenAIError(w, 400, "invalid_request_error", "unknown scope")
		return
	}
	s.chatUI.mu.Lock()
	defer s.chatUI.mu.Unlock()
	switch b.Action {
	case "create":
		name := strings.TrimSpace(b.Username)
		if name == "" || len(name) > 32 {
			writeOpenAIError(w, 400, "invalid_request_error", "invalid username")
			return
		}
		if s.chatUI.userByName(name) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "username already exists")
			return
		}
		if len(b.Password) < 6 {
			writeOpenAIError(w, 400, "invalid_request_error", "password must be at least 6 characters")
			return
		}
		h, err := hashPassword(b.Password)
		if err != nil {
			writeOpenAIError(w, 500, "internal_error", err.Error())
			return
		}
		// The cleartext key is discarded on purpose: the proxy authenticates
		// with the record looked up by KeyID (see internalChatRequest).
		rec, _, err := s.apiKeys.create("chat:" + name)
		if err != nil {
			writeOpenAIError(w, 500, "internal_error", "create api key failed")
			return
		}
		dc, di := 0, 0
		if b.DailyChat != nil {
			dc = *b.DailyChat
		}
		if b.DailyImage != nil {
			di = *b.DailyImage
		}
		u := &chatUser{ID: uuid.NewString(), Username: name, PasswordHash: h, KeyID: rec.ID, KeyPrefix: rec.Prefix, DailyChat: dc, DailyImage: di, Enabled: true, CreatedAt: time.Now()}
		s.chatUI.Users = append(s.chatUI.Users, u)
		s.chatUI.saveUsers()
		jsonOut(w, map[string]any{"status": "created", "id": u.ID})
	case "update":
		u := s.chatUI.user(b.ID)
		if u == nil {
			writeOpenAIError(w, 404, "not_found", "user not found")
			return
		}
		if b.DailyChat != nil {
			u.DailyChat = max(0, *b.DailyChat)
		}
		if b.DailyImage != nil {
			u.DailyImage = max(0, *b.DailyImage)
		}
		if b.Enabled != nil {
			u.Enabled = *b.Enabled
		}
		if b.Password != "" {
			if len(b.Password) < 6 {
				writeOpenAIError(w, 400, "invalid_request_error", "password must be at least 6 characters")
				return
			}
			h, err := hashPassword(b.Password)
			if err != nil {
				writeOpenAIError(w, 500, "internal_error", err.Error())
				return
			}
			u.PasswordHash = h
		}
		s.chatUI.saveUsers()
		jsonOut(w, map[string]any{"status": "updated"})
	case "delete":
		for i, u := range s.chatUI.Users {
			if u.ID == b.ID {
				if u.KeyID != "" {
					_, _ = s.apiKeys.delete(u.KeyID)
				}
				s.chatUI.Users = append(s.chatUI.Users[:i], s.chatUI.Users[i+1:]...)
				s.chatUI.saveUsers()
				_ = os.RemoveAll(filepath.Join(s.chatUI.Dir, "convs", u.ID))
				jsonOut(w, map[string]any{"status": "deleted"})
				return
			}
		}
		writeOpenAIError(w, 404, "not_found", "user not found")
	case "config":
		if b.ConvRetentionDays != nil {
			s.chatUI.Config.ConvRetentionDays = max(0, *b.ConvRetentionDays)
		}
		if b.ImageRetentionHours != nil {
			s.chatUI.Config.ImageRetentionHours = max(0, *b.ImageRetentionHours)
		}
		if b.MaxStorageMB != nil {
			s.chatUI.Config.MaxStorageMB = max(0, *b.MaxStorageMB)
		}
		s.chatUI.saveConfig()
		jsonOut(w, map[string]any{"status": "updated", "config": s.chatUI.Config})
	default:
		writeOpenAIError(w, 400, "invalid_request_error", "unknown action")
	}
}

// StartChatJanitor runs cleanup every 30 minutes.
func (s *Server) StartChatJanitor() {
	go func() {
		for {
			time.Sleep(30 * time.Minute)
			n, freed := s.chatUI.cleanupExpired()
			if n > 0 {
				log.Printf("[chat-janitor] removed=%d freed=%d", n, freed)
			}
		}
	}()
}
