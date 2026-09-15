package web

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// analytics aggregates agent-behaviour statistics for the admin dashboard.
// It is always on and cheap: every record is a mutex-guarded counter bump.
// The snapshot is persisted next to settings.json (throttled) so the numbers
// survive restarts on the NAS.
type analytics struct {
	mu sync.Mutex
	v  analyticsSnapshot
	dirty bool
	lastSave time.Time
	path string
}

type analyticsSnapshot struct {
	StartedAt        int64  `json:"startedAt"`
	RequestsTotal    int64  `json:"requestsTotal"`
	CompletionsTotal int64  `json:"completionsTotal"`
	FailoversTotal   int64  `json:"failoversTotal"`
	StuckLoopRejects int64  `json:"stuckLoopRejects"`
	AutoCompacts     int64  `json:"autoCompacts"`
	ToolCallsObserved int64 `json:"toolCallsObserved"`
	ToolResultsCompressed int64 `json:"toolResultsCompressed"`
	ToolResultsCapped     int64 `json:"toolResultsCapped"`
	ToolResultsDeduped    int64 `json:"toolResultsDeduped"`
	FileSummariesServed   int64 `json:"fileSummariesServed"`
	CharsSaved       int64  `json:"charsSaved"`
	RepoMapInjections int64  `json:"repoMapInjections"`
	LastRequestAt    int64  `json:"lastRequestAt"`
}

var globalAnalytics = &analytics{v: analyticsSnapshot{StartedAt: time.Now().Unix()}}

func init() {
	globalAnalytics.path = analyticsPath()
	globalAnalytics.load()
}

func analyticsPath() string {
	if dir := dirOf(settingsPath()); dir != "" {
		return filepath.Join(dir, "analytics.json")
	}
	return ""
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return ""
}

func (a *analytics) load() {
	if a.path == "" {
		return
	}
	b, err := os.ReadFile(a.path)
	if err != nil {
		return
	}
	var s analyticsSnapshot
	if json.Unmarshal(b, &s) == nil && s.StartedAt > 0 {
		// keep StartedAt from disk only if it looks like a prior run of this
		// same install; otherwise start fresh but keep counters.
		saved := a.v
		a.v = s
		if saved.StartedAt > 0 && saved.StartedAt < s.StartedAt {
			a.v.StartedAt = saved.StartedAt
		}
	}
}

func (a *analytics) persistLocked() {
	if a.path == "" || !a.dirty {
		return
	}
	if time.Since(a.lastSave) < 30*time.Second {
		return
	}
	a.lastSave = time.Now()
	a.dirty = false
	b, err := json.Marshal(a.v)
	if err != nil {
		return
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("[analytics] persist failed: %v", err)
		return
	}
	_ = os.Rename(tmp, a.path)
}

// forceFlush persists immediately regardless of throttle (used by the reset
// endpoint and before serving a snapshot read so the dashboard is fresh).
func (a *analytics) forceFlush() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastSave = time.Time{}
	a.persistLocked()
}

func (a *analytics) bump(f func(*analyticsSnapshot)) {
	a.mu.Lock()
	f(&a.v)
	a.dirty = true
	a.persistLocked()
	a.mu.Unlock()
}

func (a *analytics) snapshot() analyticsSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

func (a *analytics) reset() {
	a.mu.Lock()
	a.v = analyticsSnapshot{StartedAt: time.Now().Unix()}
	a.dirty = true
	a.lastSave = time.Time{}
	a.persistLocked()
	a.mu.Unlock()
}

// ---- record helpers (cheap; safe from any goroutine) ----

func analyticsRequest()            { globalAnalytics.bump(func(s *analyticsSnapshot) { s.RequestsTotal++; s.LastRequestAt = time.Now().Unix() }) }
func analyticsCompletion()         { globalAnalytics.bump(func(s *analyticsSnapshot) { s.CompletionsTotal++ }) }
func analyticsFailover()           { globalAnalytics.bump(func(s *analyticsSnapshot) { s.FailoversTotal++ }) }
func analyticsStuckLoopReject()    { globalAnalytics.bump(func(s *analyticsSnapshot) { s.StuckLoopRejects++ }) }
func analyticsAutoCompact()        { globalAnalytics.bump(func(s *analyticsSnapshot) { s.AutoCompacts++ }) }
func analyticsRepoMapInjection()   { globalAnalytics.bump(func(s *analyticsSnapshot) { s.RepoMapInjections++ }) }
func analyticsToolCallsObserved(n int) {
	if n <= 0 { return }
	globalAnalytics.bump(func(s *analyticsSnapshot) { s.ToolCallsObserved += int64(n) })
}
func analyticsToolCompressed(saved int) {
	if saved <= 0 { return }
	globalAnalytics.bump(func(s *analyticsSnapshot) { s.ToolResultsCompressed++; s.CharsSaved += int64(saved) })
}
func analyticsToolCapped(saved int) {
	if saved <= 0 { return }
	globalAnalytics.bump(func(s *analyticsSnapshot) { s.ToolResultsCapped++; s.CharsSaved += int64(saved) })
}
func analyticsToolDeduped(saved int) {
	if saved <= 0 { return }
	globalAnalytics.bump(func(s *analyticsSnapshot) { s.ToolResultsDeduped++; s.CharsSaved += int64(saved) })
}
func analyticsFileSummaryServed(saved int) {
	if saved <= 0 { return }
	globalAnalytics.bump(func(s *analyticsSnapshot) { s.FileSummariesServed++; s.CharsSaved += int64(saved) })
}

// analyticsSuggestions derives simple, actionable recommendations from the
// counters plus the current settings. Pure function; rendered by the admin UI.
func analyticsSuggestions(s analyticsSnapshot, cfg runtimeSettings) []string {
	var out []string
	reqs := s.RequestsTotal
	if reqs >= 10 {
		if !cfg.AutonomyBoost {
			out = append(out, "建议开启 autonomyBoost（自主性增强）：让上游模型驱动客户端 Agent 循环一次跑完，减少「做一半就停」。")
		}
		if cfg.ToolResultMode == "" || cfg.ToolResultMode == "full" {
			out = append(out, "建议把 toolResultMode 设为 smart：结构化压缩工具输出，通常可省一个数量级的上下文 token。")
		}
		if !cfg.HideReasoning {
			out = append(out, "若客户端（WorkBuddy/Trae）窗口被推理内容撑高，建议开启 hideReasoning。")
		}
	}
	if s.StuckLoopRejects > 0 {
		out = append(out, "检测到死循环拦截：若属长任务误判，可在 Advanced 页适当调高 loopSameLimit / loopRepeatLimit。")
	}
	if s.FailoversTotal > 0 {
		out = append(out, "上游曾出现断流/限流并已自动切换账号：无需处理；若频繁发生建议检查账号健康度。")
	}
	if s.CharsSaved > 50000 {
		out = append(out, "上下文压缩累计已节省大量 token，保持当前压缩配置即可。")
	}
	if len(out) == 0 {
		out = append(out, "当前配置运行良好，暂无优化建议。")
	}
	return out
}
