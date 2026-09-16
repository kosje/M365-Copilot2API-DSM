package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Persistent admin audit log. Every auditLog call is appended to
// audit.jsonl in the data directory and kept in memory (bounded) for the
// console to display via /api/admin/audit.

type auditEntry struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"`
	IP     string    `json:"ip,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

const maxAuditInMemory = 500

// auditMaxBytes bounds a single audit.jsonl. It is a variable rather than a
// constant so tests can lower it without having to write megabytes of entries.
var auditMaxBytes = int64(5 << 20)

// auditKeepGenerations is how many rolled-over copies are kept alongside the
// live file, so the audit trail costs at most (1 + auditKeepGenerations) ×
// auditMaxBytes on disk no matter how long the process runs.
const auditKeepGenerations = 1

type auditStore struct {
	mu      sync.Mutex
	path    string
	entries []auditEntry
}

var globalAudit = &auditStore{}

func auditPath() string {
	dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
	if dir == "" {
		h, _ := os.UserHomeDir()
		dir = filepath.Join(h, ".config", "m365-copilot2api")
	}
	return filepath.Join(dir, "audit.jsonl")
}

func openAuditStore() {
	globalAudit.mu.Lock()
	defer globalAudit.mu.Unlock()
	globalAudit.path = auditPath()
	f, err := os.Open(globalAudit.path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e auditEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			globalAudit.entries = append(globalAudit.entries, e)
		}
	}
	if len(globalAudit.entries) > maxAuditInMemory {
		globalAudit.entries = globalAudit.entries[len(globalAudit.entries)-maxAuditInMemory:]
	}
}

func (a *auditStore) append(e auditEntry) {
	a.mu.Lock()
	a.entries = append(a.entries, e)
	if len(a.entries) > maxAuditInMemory {
		a.entries = a.entries[len(a.entries)-maxAuditInMemory:]
	}
	path := a.path
	a.mu.Unlock()
	if path == "" {
		return
	}
	// The log grows on every auditLog call and nothing trimmed it, so on a
	// device that stays up for months it grew without bound. Bound it here,
	// where the write already happens, rather than from a background loop.
	a.rotateIfNeeded(path)
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(b)
}

// rotateIfNeeded rolls audit.jsonl over once it passes auditMaxBytes, keeping
// auditKeepGenerations previous copies. Rotation is checked on every append
// rather than from a ticker: audit events are rare, so one os.Stat per entry is
// cheap, and it means the bound holds even if the process never ticks.
func (a *auditStore) rotateIfNeeded(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < auditMaxBytes {
		return
	}
	for gen := auditKeepGenerations; gen >= 1; gen-- {
		from := path
		if gen > 1 {
			from = fmt.Sprintf("%s.%d", path, gen-1)
		}
		to := fmt.Sprintf("%s.%d", path, gen)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
}

func (a *auditStore) list(limit int) []auditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit <= 0 || limit > len(a.entries) {
		limit = len(a.entries)
	}
	out := make([]auditEntry, limit)
	copy(out, a.entries[len(a.entries)-limit:])
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// adminAudit serves the persisted audit trail for the console.
func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	limit, _ := parseIntParam(r, "limit")
	if limit <= 0 {
		limit = 200
	}
	jsonOut(w, map[string]any{"entries": globalAudit.list(limit)})
}

// adminAlertTest sends a test notification through the configured webhook.
func (s *Server) adminAlertTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	cfg := alertConfig()
	if cfg.URL == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "告警 webhook 未配置")
		return
	}
	if err := notifyWebhookAlertSync(cfg, "test", "这是一条测试告警（来自管理台「测试通知」按钮）"); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "webhook_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"ok": true})
}

func parseIntParam(r *http.Request, name string) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, false
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
