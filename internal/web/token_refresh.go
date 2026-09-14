package web

import (
	"fmt"
	"log"
	"time"
)

// StartTokenRefresh proactively refreshes tokens that are nearing expiry on a
// configurable interval, so long-running sessions don't hit auth failures at
// peak time. Failures are surfaced through the alert webhook (deduped).
func (s *Server) StartTokenRefresh() {
	go func() {
		for {
			interval := s.settings.get().TokenRefreshIntervalSeconds
			if interval <= 0 {
				time.Sleep(60 * time.Second)
				continue
			}
			time.Sleep(time.Duration(interval) * time.Second)
			log.Printf("[token-refresh] proactive refresh tick (interval=%ds)", interval)
			for _, r := range s.tokens.RefreshAllExpired() {
				if !r.Success {
					log.Printf("[token-refresh] %s (%s) FAILED: %s", r.Email, r.ID, r.Error)
					notifyWebhookAlert(alertEventTokenRenew, fmt.Sprintf("Token 主动刷新失败：%s（%s）— %s", r.Email, r.ID, r.Error), r.ID)
					continue
				}
				log.Printf("[token-refresh] %s (%s) refreshed", r.Email, r.ID)
			}
		}
	}()
}

// OpenAuditStore loads audit.jsonl into memory for the console.
func OpenAuditStore() { openAuditStore() }
