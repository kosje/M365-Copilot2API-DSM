package web

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// sessionKnowledge is the gateway-side "memory" of what a coding session has
// observed via tool results. The gateway is a stateless proxy — it cannot read
// the client's filesystem — but every file the client reads flows through a
// tool result, so the gateway can learn the repository structure from those
// observations and reuse them:
//
//   - Repo Map: paths seen in tool outputs, grouped by directory, injected as
//     a compact block into the prompt so the model keeps its bearings even
//     after auto-compact/truncation dropped the early history.
//   - File summary cache: digest (header + errors/warnings + tail) of every
//     large tool output, keyed by content hash. When the identical output
//     reappears later in the conversation, the cached digest is served
//     instead of the full payload again.
type sessionKnowledge struct {
	mu       sync.Mutex
	files    map[string]*fileEntry // path -> entry
	digests  map[string]string     // content sha -> cached digest
	lastSeen time.Time
}

type fileEntry struct {
	chars int
	seen  int
}

const (
	maxKnowledgeSessions = 256
	maxRepoMapFiles      = 300
	repoMapMaxChars      = 2000
	summaryCacheMinChars = 2000
)

var (
	knowledgeStore  = map[string]*sessionKnowledge{}
	knowledgeMu     sync.Mutex
	filePathPattern = regexp.MustCompile(`[A-Za-z0-9_.\-/\\]+\.(?:go|ts|tsx|js|jsx|mjs|py|java|kt|rs|c|h|cpp|hpp|cs|rb|php|swift|md|json|ya?ml|toml|sql|sh|css|scss|html|vue|svelte)`)
)

// sessionKeyFor derives a stable per-conversation key. Uses the upstream
// conversation ID when present; otherwise fingerprints the first user message
// so stateless clients still get a consistent bucket.
func sessionKeyFor(convID string, msgs []oaiMsg) string {
	if convID = strings.TrimSpace(convID); convID != "" {
		return "conv:" + convID
	}
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "user") {
			raw := contentToString(m.Content)
			if len(raw) > 512 {
				raw = raw[:512]
			}
			return fmt.Sprintf("fp:%x", sha256sum(raw))
		}
	}
	return ""
}

func sha256sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:8]
}

func knowledgeFor(key string) *sessionKnowledge {
	if key == "" {
		return nil
	}
	knowledgeMu.Lock()
	defer knowledgeMu.Unlock()
	k, ok := knowledgeStore[key]
	if !ok {
		if len(knowledgeStore) >= maxKnowledgeSessions {
			// evict the oldest session
			var oldestKey string
			var oldest time.Time
			for kk, vv := range knowledgeStore {
				if oldestKey == "" || vv.lastSeen.Before(oldest) {
					oldestKey, oldest = kk, vv.lastSeen
				}
			}
			delete(knowledgeStore, oldestKey)
		}
		k = &sessionKnowledge{files: map[string]*fileEntry{}, digests: map[string]string{}, lastSeen: time.Now()}
		knowledgeStore[key] = k
	}
	k.lastSeen = time.Now()
	return k
}

// observeToolResults feeds tool-output content into the knowledge store:
// extracts file-path mentions and caches large-output digests.
func (k *sessionKnowledge) observeToolResults(msgs []oaiMsg) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, m := range msgs {
		if !strings.EqualFold(m.Role, "tool") {
			continue
		}
		raw := contentToString(m.Content)
		if len(raw) < 64 {
			continue
		}
		for _, p := range filePathPattern.FindAllString(raw, 32) {
			p = strings.Trim(p, `.,;:"'()[]`)
			if len(p) < 4 || strings.Count(p, "/")+strings.Count(p, "\\") == 0 {
				continue // skip bare filenames like "go.mod" noise
			}
			e := k.files[p]
			if e == nil {
				if len(k.files) >= maxRepoMapFiles {
					continue
				}
				e = &fileEntry{}
				k.files[p] = e
			}
			e.chars += len(raw)
			e.seen++
		}
	}
}

// repoMapText renders the compact repository map block ("" when empty).
func (k *sessionKnowledge) repoMapText() string {
	if k == nil {
		return ""
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.files) == 0 {
		return ""
	}
	grouped := map[string][]string{}
	for p, e := range k.files {
		norm := strings.ReplaceAll(p, "\\", "/")
		dir := ""
		if i := strings.LastIndex(norm, "/"); i > 0 {
			dir = norm[:i]
		}
		grouped[dir] = append(grouped[dir], fmt.Sprintf("%s(%s)", norm[strings.LastIndex(norm, "/")+1:], humanChars(e.chars)))
	}
	dirs := make([]string, 0, len(grouped))
	for d := range grouped {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var b strings.Builder
	b.WriteString("[Repository map — files observed via tool results in this session; use it to keep your bearings]\n")
	for _, d := range dirs {
		files := grouped[d]
		sort.Strings(files)
		line := "- " + d + ": " + strings.Join(files, ", ")
		if b.Len()+len(line) > repoMapMaxChars {
			b.WriteString("- … (more files omitted)\n")
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func humanChars(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// cachedDigest returns the stored digest for a large tool output, computing
// and caching it on first sight.
func (k *sessionKnowledge) cachedDigest(raw string) string {
	if k == nil || len(raw) < summaryCacheMinChars {
		return ""
	}
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
	k.mu.Lock()
	d, ok := k.digests[h]
	if !ok {
		lines := strings.Split(raw, "\n")
		d = compressLines(lines, 12, 24, false)
		if len(d) > 3000 {
			d = d[:3000]
		}
		if len(k.digests) >= 512 {
			k.digests = map[string]string{} // simple reset on overflow
		}
		k.digests[h] = d
	}
	k.mu.Unlock()
	return d
}
