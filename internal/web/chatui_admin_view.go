package web

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------- admin: browse user conversations & manage generated images ----------

// chatAdminConvs lists (or fetches) conversations of a chat user, for admin review.
//   GET  /api/chatui/admin/convs?user=<id>          → conversation list (no messages)
//   GET  /api/chatui/admin/convs?user=<id>&id=<cid> → full message list of one conversation
func (s *Server) chatAdminConvs(w http.ResponseWriter, r *http.Request) {
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	userID := strings.TrimSpace(r.URL.Query().Get("user"))
	if userID == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "user is required")
		return
	}
	s.chatUI.mu.Lock()
	defer s.chatUI.mu.Unlock()

	// basic user info for the header
	username := userID
	for i := range s.chatUI.Users {
		if s.chatUI.Users[i].ID == userID {
			username = s.chatUI.Users[i].Username
			break
		}
	}
	if convID := strings.TrimSpace(r.URL.Query().Get("id")); convID != "" {
		conv := s.chatUI.loadConv(userID, convID)
		if conv == nil {
			writeOpenAIError(w, 404, "not_found", "conversation not found")
			return
		}
		msgs := make([]map[string]any, 0, len(conv.Messages))
		for _, m := range conv.Messages {
			item := map[string]any{"role": m.Role, "content": m.Content, "time": m.Time, "model": m.Model}
			if len(m.Images) > 0 {
				item["images"] = m.Images
			}
			if len(m.Gen) > 0 {
				item["gen"] = m.Gen
			}
			msgs = append(msgs, item)
		}
		jsonOut(w, map[string]any{"user": userID, "username": username, "conversationId": conv.ID, "title": conv.Title, "messages": msgs})
		return
	}
	type convSummary struct {
		ID        string    `json:"id"`
		Title     string    `json:"title"`
		UpdatedAt time.Time `json:"updatedAt"`
		Messages  int       `json:"messages"`
		Images    int       `json:"images"`
	}
	out := make([]convSummary, 0, 16)
	entries, _ := os.ReadDir(filepath.Join(s.chatUI.Dir, "convs", userID))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		c := s.chatUI.loadConv(userID, strings.TrimSuffix(e.Name(), ".json"))
		if c == nil {
			continue
		}
		images := 0
		for _, m := range c.Messages {
			images += len(m.Gen)
		}
		out = append(out, convSummary{ID: c.ID, Title: c.Title, UpdatedAt: c.UpdatedAt, Messages: len(c.Messages), Images: images})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	jsonOut(w, map[string]any{"user": userID, "username": username, "conversations": out})
}

// chatAdminImages lists generated/stored images with metadata and (optionally)
// which user/conversation referenced them.
//   GET /api/chatui/admin/images?user=<id|all>
func (s *Server) chatAdminImages(w http.ResponseWriter, r *http.Request) {
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	userFilter := strings.TrimSpace(r.URL.Query().Get("user"))
	s.chatUI.mu.Lock()
	defer s.chatUI.mu.Unlock()

	// build fileID → reference map from all conversations
	type ref struct {
		User     string `json:"user"`
		Username string `json:"username"`
		ConvID   string `json:"convId"`
		Conv     string `json:"conv"`
		Role     string `json:"role"` // gen = AI-generated, upload = user-uploaded
	}
	refs := map[string][]ref{}
	nameOf := map[string]string{}
	for i := range s.chatUI.Users {
		u := s.chatUI.Users[i]
		nameOf[u.ID] = u.Username
		if userFilter != "" && userFilter != "all" && u.ID != userFilter {
			continue
		}
		entries, _ := os.ReadDir(filepath.Join(s.chatUI.Dir, "convs", u.ID))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			c := s.chatUI.loadConv(u.ID, strings.TrimSuffix(e.Name(), ".json"))
			if c == nil {
				continue
			}
			for _, m := range c.Messages {
				for _, id := range m.Gen {
					refs[id] = append(refs[id], ref{User: u.ID, Username: u.Username, ConvID: c.ID, Conv: c.Title, Role: "gen"})
				}
				for _, id := range m.Images {
					refs[id] = append(refs[id], ref{User: u.ID, Username: u.Username, ConvID: c.ID, Conv: c.Title, Role: "upload"})
				}
			}
		}
	}
	imgs, _ := os.ReadDir(filepath.Join(s.chatUI.Dir, "images"))
	type imgItem struct {
		ID    string    `json:"id"`
		URL   string    `json:"url"`
		Size  int64     `json:"size"`
		ModAt time.Time `json:"modifiedAt"`
		Refs  []ref     `json:"refs"`
	}
	out := make([]imgItem, 0, len(imgs))
	for _, e := range imgs {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		item := imgItem{ID: id, URL: "/api/chatui/admin/image?id=" + id, Size: info.Size(), ModAt: info.ModTime()}
		if rs, ok := refs[id]; ok {
			item.Refs = rs
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModAt.After(out[j].ModAt) })
	jsonOut(w, map[string]any{"images": out, "count": len(out)})
}

// chatAdminImageFile serves an image for the admin console (admin session, no chat login).
func (s *Server) chatAdminImageFile(w http.ResponseWriter, r *http.Request) {
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" || strings.ContainsAny(id, "/\\") {
		http.NotFound(w, r)
		return
	}
	f, ct, err := s.chatUI.openImage(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, id, time.Now(), f)
}

// chatAdminDeleteImage deletes an image file and strips every reference to it
// from all users' conversations (Gen and uploaded Images lists).
func (s *Server) chatAdminDeleteImage(w http.ResponseWriter, r *http.Request) {
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var b struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&b) != nil || strings.TrimSpace(b.ID) == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "id is required")
		return
	}
	id := strings.TrimSpace(b.ID)
	if strings.ContainsAny(id, "/\\") {
		writeOpenAIError(w, 400, "invalid_request_error", "bad image id")
		return
	}
	// strip references from every conversation first
	s.chatUI.mu.Lock()
	stripped := 0
	for i := range s.chatUI.Users {
		u := s.chatUI.Users[i]
		dir := filepath.Join(s.chatUI.Dir, "convs", u.ID)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			c := s.chatUI.loadConv(u.ID, strings.TrimSuffix(e.Name(), ".json"))
			if c == nil {
				continue
			}
			changed := false
			for mi := range c.Messages {
				if n := removeIDFromList(c.Messages[mi].Gen, id); n > 0 {
					stripped += n
					changed = true
				}
				if n := removeIDFromList(c.Messages[mi].Images, id); n > 0 {
					stripped += n
					changed = true
				}
			}
			// drop messages that became fully empty placeholders (gen-only assistant msgs)
			kept := c.Messages[:0]
			for _, m := range c.Messages {
				if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" && len(m.Gen) == 0 && len(m.Images) == 0 {
					changed = true
					continue
				}
				kept = append(kept, m)
			}
			c.Messages = kept
			if changed {
				s.chatUI.saveConv(c)
			}
		}
	}
	// delete the file
	removed := false
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif"} {
		p := filepath.Join(s.chatUI.Dir, "images", id+ext)
		if _, err := os.Stat(p); err == nil {
			if os.Remove(p) == nil {
				removed = true
			}
			break
		}
	}
	s.chatUI.mu.Unlock()
	log.Printf("[chat-admin] image delete id=%s removed=%v refs_stripped=%d", id, removed, stripped)
	jsonOut(w, map[string]any{"status": "ok", "removed": removed, "refsStripped": stripped})
}

func removeIDFromList(list []string, id string) int {
	out := list[:0]
	n := 0
	for _, v := range list {
		if v == id {
			n++
			continue
		}
		out = append(out, v)
	}
	return n
}
