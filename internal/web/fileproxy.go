package web

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"m365-copilot2api/internal/chathub"
)

// m365ConvCtx records the Microsoft 365 conversation that produced a generated
// file, so the proxy can continue that exact session to retrieve the bytes.
type m365ConvCtx struct {
	AccountID   string
	AccessToken string
	OID         string
	TID         string
	ConvID      string // Microsoft conversation id
	SessID      string // Microsoft session id
	At          time.Time
}

// asyncgwURLRe matches Microsoft 365 Copilot generated-file links.
var asyncgwURLRe = regexp.MustCompile(`https?://[a-z0-9-]+\.asyncgw\.teams\.microsoft\.com/v1/objects/[^\s"'\)\]\s]+`)

// extractAsyncGWFiles returns the asyncgw URLs and their filenames found in a
// piece of chat text.
func extractAsyncGWFiles(text string) map[string]string {
	out := map[string]string{}
	for _, u := range asyncgwURLRe.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,);")
		name := ""
		if i := strings.LastIndex(u, "/"); i >= 0 {
			name = u[i+1:]
		}
		// drop any query string — it is not part of the filename
		if q := strings.IndexByte(name, '?'); q >= 0 {
			name = name[:q]
		}
		// percent-decoded filename
		if dec, err := decodePathSegment(name); err == nil && dec != "" {
			name = dec
		}
		if name != "" {
			out[u] = name
		}
	}
	return out
}

func decodePathSegment(s string) (string, error) {
	// handle %-encoding only (not full URL query)
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			var v int
			_, err := fmt.Sscanf(s[i+1:i+3], "%02x", &v)
			if err != nil {
				b.WriteByte(s[i])
				continue
			}
			b.WriteByte(byte(v))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

// recordConvCtx stores the M365 conversation context keyed by the internal
// chat conversation id so generated-file retrieval can continue that session.
func (s *Server) recordConvCtx(internalConv, accountID, accessToken, oid, tid, convID, sessID string) {
	if internalConv == "" || convID == "" {
		return
	}
	s.fileProxyMu.Lock()
	if s.fileProxyCtx == nil {
		s.fileProxyCtx = map[string]m365ConvCtx{}
	}
	s.fileProxyCtx[internalConv] = m365ConvCtx{
		AccountID:   accountID,
		AccessToken: accessToken,
		OID:         oid,
		TID:         tid,
		ConvID:      convID,
		SessID:      sessID,
		At:          time.Now(),
	}
	s.fileProxyMu.Unlock()
}

// fetchGeneratedFile re-emits the generated file as base64 in the same M365
// conversation and caches the decoded bytes locally. It returns the local file
// path on success.
func (s *Server) fetchGeneratedFile(internalConv, filename string) (string, error) {
	s.fileProxyMu.Lock()
	ctx, ok := s.fileProxyCtx[internalConv]
	cacheKey := internalConv + "|" + filename
	if ok {
		if p, hit := s.fileProxyCache[cacheKey]; hit {
			s.fileProxyMu.Unlock()
			return p, nil
		}
	}
	s.fileProxyMu.Unlock()
	if !ok {
		return "", fmt.Errorf("no conversation context for %s", internalConv)
	}
	account := chathub.Account{AccessToken: ctx.AccessToken, OID: ctx.OID, TID: ctx.TID}
	prompt := fmt.Sprintf("请把你刚刚生成的文件 %s 的完整二进制内容以 base64 编码原样输出，放在一个单独的 ```base64file 代码块中。不要省略、不要截断、不要总结，必须包含文件的全部字节，也不要在代码块前后添加任何说明文字。", filename)
	req := chathub.Request{
		Text:            prompt,
		Tone:            "magic",
		Locale:          "en-us",
		Market:          "en-us",
		ConversationID: ctx.ConvID,
		SessionID:      ctx.SessID,
		Started:        false,
	}
	cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := s.chatWithAccount(cctx, ctx.AccountID, account, req)
	if err != nil {
		return "", fmt.Errorf("base64 re-emit failed: %w", err)
	}
	data, err := extractBase64File(res.Text)
	if err != nil {
		return "", err
	}
	path, err := s.saveGeneratedFile(internalConv, filename, data)
	if err != nil {
		return "", err
	}
	s.fileProxyMu.Lock()
	if s.fileProxyCache == nil {
		s.fileProxyCache = map[string]string{}
	}
	s.fileProxyCache[cacheKey] = path
	s.fileProxyMu.Unlock()
	return path, nil
}

// extractBase64File pulls the file bytes out of a model response that contains a
// base64 dump of the file. It tolerates the model wrapping the base64 in fences
// or writing a "64file\n<base64>" prefix without fences.
func extractBase64File(text string) ([]byte, error) {
	re := regexp.MustCompile("(?s)```(?:base64file|base64)\\s*\\n(.*?)```")
	m := re.FindStringSubmatch(text)
	candidate := ""
	if m != nil {
		candidate = m[1]
	} else if idx := strings.Index(text, "64file"); idx >= 0 {
		candidate = text[idx+len("64file"):]
	} else {
		runs := regexp.MustCompile(`[A-Za-z0-9+/=]{200,}`).FindAllString(text, -1)
		best := ""
		for _, r := range runs {
			if len(r) > len(best) {
				best = r
			}
		}
		candidate = best
	}
	candidate = strings.NewReplacer("\r", "", "\n", "", "\t", "", " ", "").Replace(candidate)
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r
		case r >= 'a' && r <= 'z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '+' || r == '/' || r == '=':
			return r
		}
		return -1
	}, candidate)
	for len(clean)%4 != 0 {
		if len(clean) == 0 {
			break
		}
		clean = clean[:len(clean)-1]
	}
	if len(clean) < 32 {
		return nil, fmt.Errorf("no usable base64 found (len=%d)", len(clean))
	}
	dec, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("base64 decode failed: %w", err)
	}
	if !validFileMagic(dec) {
		return nil, fmt.Errorf("decoded bytes are not a recognized file type (magic=%q)", string(dec[:min(len(dec), 8)]))
	}
	return dec, nil
}

func validFileMagic(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	switch {
	case b[0] == '%' && len(b) >= 5 && string(b[:5]) == "%PDF-": // PDF
		return true
	case b[0] == 0x50 && b[1] == 0x4B: // ZIP / docx / xlsx / pptx / apk
		return true
	case b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF: // JPEG
		return true
	case b[0] == 0x89 && b[1] == 0x50 && b[2] == 0x4E && b[3] == 0x47: // PNG
		return true
	case string(b[:4]) == "GIF8": // GIF
		return true
	case b[0] == 0x25 && len(b) >= 4 && string(b[:4]) == "%!PS": // PS
		return true
	case string(b[:2]) == "PK": // some zip variants
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// saveGeneratedFile stores the decoded file under data/files and returns the
// absolute path.
func (s *Server) saveGeneratedFile(internalConv, filename string, data []byte) (string, error) {
	dir := chatDataDir()
	base := filepath.Join(dir, "files")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	// stable, unique-ish name: hash(conv|name) + safe filename
	h := sha1Hex(internalConv + "|" + filename)
	safe := sanitizeFileName(filename)
	if safe == "" {
		safe = "file"
	}
	finalName := h + "_" + safe
	path := filepath.Join(base, finalName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func sanitizeFileName(n string) string {
	n = filepath.Base(n)
	var b strings.Builder
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

func sha1Hex(s string) string {
	h := sha1.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

func mimeByExt(name string) string {
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".pdf":
			ct = "application/pdf"
		case ".docx":
			ct = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
		case ".xlsx":
			ct = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		case ".pptx":
			ct = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
		case ".csv":
			ct = "text/csv"
		case ".txt":
			ct = "text/plain; charset=utf-8"
		case ".py":
			ct = "text/plain; charset=utf-8"
		case ".zip":
			ct = "application/zip"
		case ".json":
			ct = "application/json"
		case ".md":
			ct = "text/markdown"
		default:
			ct = "application/octet-stream"
		}
	}
	return ct
}

func urlEncode(s string) string {
	return url.QueryEscape(s)
}

// rewriteAsyncGWToProxy replaces unreachable asyncgw Copilot file links with
// local gateway proxy URLs that re-emit and serve the file bytes. The internal
// conversation id links the proxy back to the originating M365 session.
func (s *Server) rewriteAsyncGWToProxy(text, internalConv string) string {
	files := extractAsyncGWFiles(text)
	if len(files) == 0 {
		return text
	}
	for u, name := range files {
		proxy := fmt.Sprintf("/api/chatui/fileproxy?conv=%s&name=%s",
			url.QueryEscape(internalConv), url.QueryEscape(name))
		text = strings.ReplaceAll(text, u, proxy)
	}
	return text
}

// chatFileProxy serves a Microsoft 365 Copilot generated file through the local
// gateway. It re-emits the file as base64 in the originating M365 conversation
// (lazy on first request, cached afterwards) so the user gets a working
// download even though the raw asyncgw link is unreachable.
func (s *Server) chatFileProxy(w http.ResponseWriter, r *http.Request) {
	u := s.chatAuth(r)
	if u == nil {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	conv := r.URL.Query().Get("conv")
	name := r.URL.Query().Get("name")
	if conv == "" || name == "" {
		http.Error(w, "missing conv/name", http.StatusBadRequest)
		return
	}
	path, err := s.fetchGeneratedFile(conv, name)
	if err != nil {
		http.Error(w, "无法取回文件（会话可能已结束或文件已过期）："+err.Error(), http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "file missing", http.StatusNotFound)
		return
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	ctype := mimeByExt(name)
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", urlEncode(name)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
}
