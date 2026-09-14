package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Webhook alert events. Each event is de-duplicated per subject so a storm of
// failures produces at most one notification per cooldown window.
const (
	alertEventAccountAuth = "account_auth_failure"
	alertEventKeyQuota    = "key_quota_exhausted"
	alertEventErrorRate   = "error_rate_high"
	alertEventTokenRenew  = "token_refresh_failed"
)

const alertDedupeWindow = time.Hour

type alertState struct {
	mu      sync.Mutex
	lastSeq map[string]time.Time // dedupe key -> last sent time
	client  *http.Client
}

var alerts = &alertState{
	lastSeq: map[string]time.Time{},
	client:  &http.Client{Timeout: 5 * time.Second},
}

// alertAllowed dedupes (event, subject) pairs within the cooldown window.
func (a *alertState) alertAllowed(event, subject string) bool {
	key := event + "|" + subject
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.lastSeq[key]; ok && time.Since(t) < alertDedupeWindow {
		return false
	}
	if len(a.lastSeq) > 1024 {
		for k, v := range a.lastSeq {
			if time.Since(v) > alertDedupeWindow {
				delete(a.lastSeq, k)
			}
		}
	}
	a.lastSeq[key] = time.Now()
	return true
}

// alertConfigSnapshot holds the webhook settings for a send attempt.
type alertConfigSnapshot struct {
	URL    string
	Type   string
	Events []string
}

func alertConfig() alertConfigSnapshot {
	s := currentSettings()
	return alertConfigSnapshot{URL: strings.TrimSpace(s.AlertWebhookURL), Type: strings.TrimSpace(s.AlertWebhookType), Events: s.AlertEvents}
}

func alertEventEnabled(cfg alertConfigSnapshot, event string) bool {
	if len(cfg.Events) == 0 {
		return true // default: all events
	}
	for _, e := range cfg.Events {
		if strings.TrimSpace(e) == event {
			return true
		}
	}
	return false
}

// notifyWebhookAlert sends an alert asynchronously (never blocks the request
// path). Silently no-ops when no webhook is configured, the event is disabled,
// or the same (event, subject) fired within the dedupe window.
func notifyWebhookAlert(event, title, subject string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[alert] panic recovered: %v", r)
			}
		}()
		cfg := alertConfig()
		if cfg.URL == "" || !alertEventEnabled(cfg, event) {
			return
		}
		if !alerts.alertAllowed(event, subject) {
			return
		}
		if err := sendWebhookAlert(cfg, event, title); err != nil {
			log.Printf("[alert] %s webhook send failed: %v", event, err)
		}
	}()
}

// notifyWebhookAlertSync is the synchronous variant used by the test endpoint.
func notifyWebhookAlertSync(cfg alertConfigSnapshot, event, title string) error {
	return sendWebhookAlert(cfg, event, title)
}

func sendWebhookAlert(cfg alertConfigSnapshot, event, title string) error {
	if cfg.URL == "" {
		return fmt.Errorf("no webhook url configured")
	}
	kind := strings.ToLower(cfg.Type)
	if kind == "" || kind == "auto" {
		kind = detectWebhookKind(cfg.URL)
	}
	text := fmt.Sprintf("[M365-Copilot2API] %s\n事件: %s\n时间: %s", title, event, time.Now().Format("2006-01-02 15:04:05"))
	var payload []byte
	switch kind {
	case "feishu":
		payload = mustJSONBytes(map[string]any{"msg_type": "text", "content": map[string]any{"text": text}})
	case "telegram":
		u := strings.TrimSuffix(cfg.URL, "/")
		// URL format: https://api.telegram.org/bot<token>/sendMessage already;
		// just POST the body.
		payload = mustJSONBytes(map[string]any{"chat_id": strings.TrimSpace(currentSettings().AlertTelegramChatID), "text": text})
		if strings.TrimSpace(currentSettings().AlertTelegramChatID) == "" {
			// Without a chat_id Telegram cannot route; fall back to generic
			// JSON POST so self-hosted gateways still receive it.
			payload = mustJSONBytes(map[string]any{"event": event, "title": title, "time": time.Now().Format(time.RFC3339)})
		}
		_ = u
	case "bark":
		// Bark: POST JSON body to device URL.
		payload = mustJSONBytes(map[string]any{"title": "M365-Copilot2API", "body": title, "group": "m365-copilot2api"})
	default:
		payload = mustJSONBytes(map[string]any{"event": event, "title": title, "time": time.Now().Format(time.RFC3339)})
	}
	req, err := http.NewRequest(http.MethodPost, cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := alerts.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	log.Printf("[alert] sent event=%s via=%s", event, kind)
	return nil
}

func detectWebhookKind(url string) string {
	switch {
	case strings.Contains(url, "open.feishu.cn") || strings.Contains(url, "open.larksuite.com"):
		return "feishu"
	case strings.Contains(url, "api.telegram.org"):
		return "telegram"
	case strings.Contains(url, "api.day.app") || strings.Contains(url, "/bark"):
		return "bark"
	default:
		return "generic"
	}
}

func mustJSONBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// StartAlertMonitor periodically checks the recent error rate and fires an
// alert when it exceeds the configured threshold. Runs every 10 minutes.
func (s *Server) StartAlertMonitor() {
	go func() {
		for {
			time.Sleep(10 * time.Minute)
			cfg := alertConfig()
			if cfg.URL == "" || !alertEventEnabled(cfg, alertEventErrorRate) {
				continue
			}
			threshold := s.settings.get().AlertErrorRatePercent
			if threshold <= 0 {
				continue
			}
			total, failed, _ := s.usage.recentStats(60 * time.Minute)
			if total < 20 {
				continue // not enough samples
			}
			pct := float64(failed) / float64(total) * 100
			if pct >= float64(threshold) {
				notifyWebhookAlert(alertEventErrorRate, fmt.Sprintf("最近 60 分钟错误率 %.0f%%（%d/%d，阈值 %d%%）", pct, failed, total, threshold), "error_rate")
			}
		}
	}()
}
