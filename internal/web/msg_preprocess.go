package web

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// preprocessMessages applies opt-in context hygiene right before the upstream
// prompt is built. All transforms are disabled by default, so behavior is
// unchanged unless the operator enables them in settings.
//   - DedupeToolResults collapses consecutive identical tool-role messages
//     (same ToolCallID + same content). The model sometimes re-asks the same
//     read/search and the same result is echoed back; dropping the duplicate
//     is idempotent and saves upstream context tokens.
//   - MaxHistoryMessages proactively caps the message count sent upstream
//     (system/developer messages are always kept). This is cheaper than waiting
//     for auto-compact to overflow the token budget.
//   - MaxToolResultChars + ToolResultMode bounds/compresses a single tool-role
//     message (coding tools emit huge reads / build logs). "full" truncates,
//     "smart"/"minimal" structurally compress (keep errors/warnings/tail).
//   - DedupeRepeatedToolResults folds non-adjacent identical large tool outputs
//     (the same big file read 5 times across the conversation) into a short
//     reference note.
func preprocessMessages(msgs []oaiMsg, cfg runtimeSettings) []oaiMsg {
	if cfg.DedupeToolResults {
		msgs = dedupeConsecutiveToolResults(msgs)
	}
	if cfg.MaxHistoryMessages > 0 && len(msgs) > cfg.MaxHistoryMessages {
		msgs = keepRecentMessages(msgs, cfg.MaxHistoryMessages)
	}
	switch cfg.ToolResultMode {
	case "smart", "minimal":
		msgs = compressToolResults(msgs, cfg.MaxToolResultChars, cfg.ToolResultMode == "minimal")
	default: // "full" or empty
		if cfg.MaxToolResultChars > 0 {
			msgs = capToolResults(msgs, cfg.MaxToolResultChars)
		}
	}
	if cfg.DedupeToolResults {
		// also fold non-adjacent repeated large reads (cheap, hash-based)
		msgs = dedupeRepeatedToolResults(msgs, 2000)
	}
	return msgs
}

// compressToolResults structurally compresses oversized tool-role messages.
// Build/test/install/grep outputs are 95% boilerplate; keeping the header,
// every error/warning line, and the tail preserves the signal while cutting
// the token cost by an order of magnitude.
func compressToolResults(msgs []oaiMsg, maxChars int, minimal bool) []oaiMsg {
	out := make([]oaiMsg, 0, len(msgs))
	for _, m := range msgs {
		if !strings.EqualFold(m.Role, "tool") {
			out = append(out, m)
			continue
		}
		raw := contentToString(m.Content)
		// maxChars==0 means "compress but never hard-truncate"; pick a sane
		// structural ceiling so a pathological 200k log still can't blow budget.
		ceil := maxChars
		if ceil <= 0 {
			ceil = 24000
		}
		if len(raw) <= ceil {
			out = append(out, m)
			continue
		}
		lines := strings.Split(raw, "\n")
		headerN, tailN, errWarnOnly := 30, 60, false
		if minimal {
			headerN, tailN, errWarnOnly = 8, 20, true
		}
		kept := compressLines(lines, headerN, tailN, errWarnOnly)
		if len(kept) > ceil {
			kept = kept[:ceil]
		}
		notice := fmt.Sprintf("\n\n[tool output compressed: %d of %d chars kept — header + all errors/warnings + tail; use a narrower query for full output]", len(kept), len(raw))
		m.Content = kept + notice
		out = append(out, m)
	}
	return out
}

// compressLines returns header lines, then all error/warning lines (deduped,
// order-preserving), then the tail lines. When errWarnOnly is set the header
// is shrunk to a stub so only diagnostics + tail survive.
func compressLines(lines []string, headerN, tailN int, errWarnOnly bool) string {
	var header, errWarn, tail []string
	seen := map[string]bool{}
	const pat = "error:err:failed:fail:fatal:panic:warning:warn:exception:cannot:could not:not found:denied:refused:✗:✘"
	isDiag := func(s string) bool {
		l := strings.ToLower(s)
		for _, p := range strings.Split(pat, ":") {
			if p != "" && strings.Contains(l, p) {
				return true
			}
		}
		return false
	}
	hEnd := headerN
	if hEnd > len(lines) {
		hEnd = len(lines)
	}
	header = lines[:hEnd]
	tStart := len(lines) - tailN
	if tStart < hEnd {
		tStart = hEnd
	}
	tail = lines[tStart:]
	for _, l := range lines {
		if isDiag(l) {
			if !seen[l] {
				seen[l] = true
				errWarn = append(errWarn, l)
			}
		}
	}
	if errWarnOnly {
		header = nil
	}
	var b strings.Builder
	b.WriteString(strings.Join(header, "\n"))
	if len(header) > 0 && len(errWarn) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(strings.Join(errWarn, "\n"))
	if len(errWarn) > 0 && len(tail) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(strings.Join(tail, "\n"))
	return b.String()
}

// capToolResults truncates any single tool-role message that exceeds maxChars,
// appending a clear notice. System/developer/user/assistant messages untouched.
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

// dedupeRepeatedToolResults folds non-adjacent identical large tool outputs
// into a short reference note. Keyed by a content hash so the same big file
// read many turns apart is only ever sent upstream once.
func dedupeRepeatedToolResults(msgs []oaiMsg, minChars int) []oaiMsg {
	seen := map[string]bool{}
	out := make([]oaiMsg, 0, len(msgs))
	for _, m := range msgs {
		if !strings.EqualFold(m.Role, "tool") {
			out = append(out, m)
			continue
		}
		raw := contentToString(m.Content)
		if len(raw) < minChars {
			out = append(out, m)
			continue
		}
		h := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
		if seen[h] {
			short := h[:12]
			m.Content = fmt.Sprintf("[identical tool output (sha256:%s) already provided earlier in this conversation; omitted to save context]", short)
			out = append(out, m)
			continue
		}
		seen[h] = true
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
