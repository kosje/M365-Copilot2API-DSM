package web

import (
	"fmt"
	"strings"
)

// preprocessMessages applies opt-in context hygiene right before the upstream
// prompt is built. Both transforms are disabled by default, so behavior is
// unchanged unless the operator enables them in settings.
//   - DedupeToolResults collapses consecutive identical tool-role messages
//     (same ToolCallID + same content). The model sometimes re-asks the same
//     read/search and the same result is echoed back; dropping the duplicate
//     is idempotent and saves upstream context tokens.
//   - MaxHistoryMessages proactively caps the message count sent upstream
//     (system/developer messages are always kept). This is cheaper than waiting
//     for auto-compact to overflow the token budget.
func preprocessMessages(msgs []oaiMsg, cfg runtimeSettings) []oaiMsg {
	if cfg.DedupeToolResults {
		msgs = dedupeConsecutiveToolResults(msgs)
	}
	if cfg.MaxHistoryMessages > 0 && len(msgs) > cfg.MaxHistoryMessages {
		msgs = keepRecentMessages(msgs, cfg.MaxHistoryMessages)
	}
	if cfg.MaxToolResultChars > 0 {
		msgs = capToolResults(msgs, cfg.MaxToolResultChars)
	}
	return msgs
}

// capToolResults truncates any single tool-role message that exceeds maxChars,
// appending a clear notice. Coding tools (read / build logs / grep dumps) can
// emit huge payloads that would otherwise blow the context budget or force an
// auto-compact that discards earlier history. System/developer/user/assistant
// messages are left untouched.
func capToolResults(msgs []oaiMsg, maxChars int) []oaiMsg {
	out := make([]oaiMsg, 0, len(msgs))
	for _, m := range msgs {
		if !strings.EqualFold(m.Role, "tool") {
			out = append(out, m)
			continue
		}
		raw := contentToString(m.Content)
		if len(raw) <= maxChars {
			out = append(out, m)
			continue
		}
		kept := raw[:maxChars]
		notice := fmt.Sprintf("\n\n[result truncated: %d of %d chars kept; use a narrower query or read specific lines to see the rest]", maxChars, len(raw))
		m.Content = kept + notice
		out = append(out, m)
	}
	return out
}

func dedupeConsecutiveToolResults(msgs []oaiMsg) []oaiMsg {
	out := make([]oaiMsg, 0, len(msgs))
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "tool") && len(out) > 0 {
			prev := out[len(out)-1]
			if strings.EqualFold(prev.Role, "tool") &&
				prev.ToolCallID == m.ToolCallID &&
				contentToString(prev.Content) == contentToString(m.Content) {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// keepRecentMessages preserves all system/developer messages and the most
// recent n non-system messages. Truncating the oldest history is safe: the
// upstream M365 conversation is anchored by ConversationID/SessionID, not by the
// client-sent prefix.
func keepRecentMessages(msgs []oaiMsg, n int) []oaiMsg {
	var system, rest []oaiMsg
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "system") || strings.EqualFold(m.Role, "developer") {
			system = append(system, m)
		} else {
			rest = append(rest, m)
		}
	}
	if len(rest) <= n {
		return msgs
	}
	return append(system, rest[len(rest)-n:]...)
}
