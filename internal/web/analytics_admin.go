package web

import "net/http"

// adminAnalytics serves the Agent Analytics snapshot plus derived
// configuration suggestions for the admin dashboard.
func (s *Server) adminAnalytics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	globalAnalytics.forceFlush()
	snap := globalAnalytics.snapshot()
	cfg := s.settings.get()
	jsonOut(w, map[string]any{
		"analytics":   snap,
		"suggestions": analyticsSuggestions(snap, cfg),
	})
}

// adminAnalyticsReset zeroes all counters and persists the cleared snapshot.
func (s *Server) adminAnalyticsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	globalAnalytics.reset()
	jsonOut(w, map[string]any{"ok": true})
}
