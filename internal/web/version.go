package web

import (
	"fmt"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"runtime"
	"strings"
	"time"
)

var (
	Version     = "dev"
	Commit      = "unknown"
	BuildTime   = "unknown"
	startedAt   = time.Now()
	updateCheck uint32
)

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	jsonOut(w, map[string]any{"version": Version, "commit": Commit, "buildTime": BuildTime, "go": runtime.Version(), "uptimeSeconds": int(time.Since(startedAt).Seconds()), "accounts": len(s.tokens.List()), "proxyPool": len(outbound.ProxyPoolStatus())})
}

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	// Real update check against GitHub releases (mirror fallback, cached).
	// Read-only: applying an update requires POST /api/admin/update/apply.
	s.updateHandler(w, r)
}

func ReleaseTag() string {
	v := strings.TrimSpace(Version)
	if v == "" || v == "dev" {
		return ""
	}
	return fmt.Sprintf("v%s", v)
}
