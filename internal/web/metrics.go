package web

import (
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// metricsHandler exposes a Prometheus text-format endpoint at /metrics.
// Access rules: loopback scrapers are always allowed; remote scrapers must
// present the configured metrics token via Authorization: Bearer <token> or
// ?token=. When no token is configured, remote access is denied.
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	remote := ip == nil || !ip.IsLoopback()
	token := strings.TrimSpace(s.settings.get().MetricsToken)
	if remote {
		presented := strings.TrimSpace(r.URL.Query().Get("token"))
		if presented == "" {
			if v := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(v), "bearer ") {
				presented = strings.TrimSpace(v[7:])
			}
		}
		if token == "" || presented != token {
			writeOpenAIError(w, http.StatusForbidden, "auth_error", "metrics token required")
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	writeMetrics(&b, s)
	_, _ = fmt.Fprint(w, b.String())
}

func writeMetrics(b *strings.Builder, s *Server) {
	total, failed, pct := s.usage.recentStats(24 * time.Hour)
	recent, recentFailed, recentPct := s.usage.recentStats(60 * time.Minute)
	sum := s.usage.snapshot(30)
	summary, _ := sum["summary"].(map[string]any)
	_ = total

	b.WriteString("# HELP m365_usage_requests_total Total /v1 requests recorded in the usage log.\n")
	b.WriteString("# TYPE m365_usage_requests_total counter\n")
	fmt.Fprintf(b, "m365_usage_requests_total %d\n", summaryInt(summary, "requests"))
	b.WriteString("# HELP m365_usage_requests_failed_total Failed (>=4xx) /v1 requests.\n")
	b.WriteString("# TYPE m365_usage_requests_failed_total counter\n")
	fmt.Fprintf(b, "m365_usage_requests_failed_total %d\n", failed)
	b.WriteString("# HELP m365_usage_error_rate_percent Error rate over the window, percent.\n")
	b.WriteString("# TYPE m365_usage_error_rate_percent gauge\n")
	fmt.Fprintf(b, "m365_usage_error_rate_percent{window=\"24h\"} %s\n", strconv.FormatFloat(pct, 'f', 1, 64))
	fmt.Fprintf(b, "m365_usage_error_rate_percent{window=\"1h\"} %s\n", strconv.FormatFloat(recentPct, 'f', 1, 64))
	b.WriteString("# HELP m365_usage_tokens_total Total tokens (input+output+cache).\n")
	b.WriteString("# TYPE m365_usage_tokens_total counter\n")
	fmt.Fprintf(b, "m365_usage_tokens_total %d\n", summaryInt(summary, "tokens"))
	b.WriteString("# HELP m365_usage_avg_latency_ms Average request latency in milliseconds.\n")
	b.WriteString("# TYPE m365_usage_avg_latency_ms gauge\n")
	fmt.Fprintf(b, "m365_usage_avg_latency_ms %d\n", summaryInt(summary, "avg_ms"))
	fmt.Fprintf(b, "m365_usage_requests_1h %d\n", recent)
	fmt.Fprintf(b, "m365_usage_requests_failed_1h %d\n", recentFailed)

	// Per-model request counts from the 30-day snapshot.
	if models, ok := sum["models"].([]map[string]any); ok {
		b.WriteString("# HELP m365_usage_model_requests_total Requests per model (30d window).\n")
		b.WriteString("# TYPE m365_usage_model_requests_total counter\n")
		for _, m := range models {
			name, _ := m["name"].(string)
			fmt.Fprintf(b, "m365_usage_model_requests_total{model=%q} %d\n", sanitizeLabel(name), summaryInt(m, "requests"))
		}
	}

	// Account availability gauge from the health snapshot.
	if s.accountPool != nil {
		accounts := s.tokens.List()
		avail := 0
		for _, acc := range accounts {
			if s.accountPool.Available(acc.ID) {
				avail++
			}
		}
		b.WriteString("# HELP m365_accounts_available Healthy (non-cooling) accounts.\n")
		b.WriteString("# TYPE m365_accounts_available gauge\n")
		fmt.Fprintf(b, "m365_accounts_available %d\n", avail)
		b.WriteString("# HELP m365_accounts_total Configured accounts.\n")
		b.WriteString("# TYPE m365_accounts_total gauge\n")
		fmt.Fprintf(b, "m365_accounts_total %d\n", len(accounts))
	}

	// Runtime metrics.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	b.WriteString("# HELP m365_go_goroutines Current goroutine count.\n")
	b.WriteString("# TYPE m365_go_goroutines gauge\n")
	fmt.Fprintf(b, "m365_go_goroutines %d\n", runtime.NumGoroutine())
	b.WriteString("# HELP m365_go_heap_alloc_bytes Heap allocation in bytes.\n")
	b.WriteString("# TYPE m365_go_heap_alloc_bytes gauge\n")
	fmt.Fprintf(b, "m365_go_heap_alloc_bytes %d\n", ms.HeapAlloc)
	b.WriteString("# HELP m365_process_uptime_seconds_seconds Process uptime in seconds.\n")
	b.WriteString("# TYPE m365_process_uptime_seconds_seconds counter\n")
	fmt.Fprintf(b, "m365_process_uptime_seconds_seconds %d\n", int64(time.Since(processStart).Seconds()))
}

func summaryInt(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

func sanitizeLabel(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}

var processStart = time.Now()
