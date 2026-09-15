package web

import "strings"

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
	return msgs
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
