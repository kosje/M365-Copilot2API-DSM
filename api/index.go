// Vercel Go Function entry point. Deploys the full gateway as a single
// serverless function: every request (any path) is routed to Handler.
//
// Deploy layout:
//
//	api/index.go      (this file)
//	vercel.json       (rewrite all paths here, set maxDuration)
//
// Limitations on Vercel's free tier: there is no persistent disk, so
// M365_DATA_DIR defaults to /tmp and account tokens do not survive cold
// starts. Suitable for trials; for production use a host with a volume.
package main

import (
	"log"
	"m365-copilot2api/internal/outbound"
	"m365-copilot2api/internal/web"
	"net/http"
	"os"
	"sync"
)

var (
	initOnce sync.Once
	handler  http.Handler
	initErr  error
)

func initServer() {
	if os.Getenv("M365_DATA_DIR") == "" {
		os.Setenv("M365_DATA_DIR", "/tmp/m365-copilot2api")
	}
	web.ApplyStartupSettingsEnv()
	if err := outbound.ConfigureFromEnv(); err != nil {
		initErr = err
		return
	}
	s, e := web.New()
	if e != nil {
		initErr = e
		return
	}
	s.InitM365CloudClient()
	s.StartConvCacheGC()
	s.RefreshExpiredTokens()
	// Not a loop: this loads audit.jsonl into memory so the console can show the
	// audit log. cmd/server/main.go calls it, and skipping it here meant the
	// audit view was silently empty on this deployment shape.
	web.OpenAuditStore()
	// Deliberately not called here, unlike cmd/server/main.go, because each one
	// is a long-running background loop and this entry point runs in short-lived
	// instances: StartAutoCleanup, StartPreheatPool, StartChatJanitor,
	// StartQuotaRefresh, StartTokenRefresh, StartAlertMonitor and
	// StartUpdateChecker. A token that expires between instances is refreshed on
	// demand instead (RefreshExpiredTokens above covers the common case), and
	// nothing here relies on a loop having run.
	handler = s.Routes()
	log.Println("m365-copilot2api serverless instance ready")
}

// Handler is invoked by the Vercel Go runtime for each request.
func Handler(w http.ResponseWriter, r *http.Request) {
	initOnce.Do(initServer)
	if initErr != nil {
		http.Error(w, "gateway init failed: "+initErr.Error(), http.StatusInternalServerError)
		return
	}
	handler.ServeHTTP(w, r)
}

// main exists only so `go build ./...` accepts this package as a valid main
// package outside of Vercel's build pipeline, which wraps Handler itself.
func main() {}
