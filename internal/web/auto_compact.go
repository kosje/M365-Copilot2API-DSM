package web

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"m365-copilot2api/internal/chathub"
)

// auto-compact: when the context budget overflows, the dropped middle history
// is summarized through one upstream call and injected back as a pinned
// system-level summary, instead of being silently truncated. This lets very
// long agent sessions (e.g. whole-site audits with thousands of pages) keep
// their accumulated knowledge across requests.

const compactSummaryPrompt = `You are compressing prior conversation history for an AI coding assistant.
Summarize the conversation below. Preserve, in dense bullet form:
- the overall task goals, scope and constraints
- key decisions made and their rationale
- file paths, module/function/class names, and important identifiers
- significant code snippets or data structures (keep signatures, drop bodies)
- unresolved issues, open questions, and pending next steps
- the current state of the work at the point the history ends
Do not add commentary, greetings, or anything not present in the history.
Output the summary only.`

const (
	compactMaxSummaryChars = 12000
	compactMaxInputChars   = 120000
	compactTimeout         = 120 * time.Second
)

// maybeCompactContext applies the context budget. When the budget overflows
// and auto-compact is enabled, the dropped history is summarized upstream and
// prepended as a pinned summary message. Returns the final message list, a
// flag telling whether compaction produced the summary, and a budget error
// when even the pinned context cannot fit.
func (s *Server) maybeCompactContext(r *http.Request, msgs []oaiMsg, budget int) ([]oaiMsg, bool, error) {
	kept, dropped, droppedTokens, truncated, err := slidingWindowDetailed(msgs, budget)
	if err != nil {
		return nil, false, err
	}
	if !truncated || len(dropped) == 0 {
		return msgs, false, nil
	}
	var cfg runtimeSettings
	if s.settings != nil {
		cfg = s.settings.get()
	}
	if !cfg.EnableAutoCompact {
		log.Printf("[auto-compact] disabled; plain truncation dropped %d tokens", droppedTokens)
		return kept, false, nil
	}
	minTok := cfg.AutoCompactMinTokens
	if minTok <= 0 {
		minTok = 4000
	}
	if droppedTokens < minTok {
		log.Printf("[auto-compact] dropped %d tokens below threshold %d; plain truncation", droppedTokens, minTok)
		return kept, false, nil
	}
	summary, err := s.summarizeHistory(r, dropped)
	if err != nil {
		log.Printf("[auto-compact] summary failed (%v); falling back to truncation of %d tokens", err, droppedTokens)
		return kept, false, nil
	}
	log.Printf("[auto-compact] compacted %d dropped tokens into %d-char summary", droppedTokens, len(summary))
	summaryMsgs := []oaiMsg{
		{Role: "system", Content: "[Prior conversation summary — compacted automatically to fit the context window. Current conversation continues below.]\n\n" + summary},
	}
	return append(summaryMsgs, kept...), true, nil
}

// summarizeHistory runs one upstream chat call to compress the dropped history.
func (s *Server) summarizeHistory(r *http.Request, dropped []oaiMsg) (string, error) {
	text, _ := flattenPromptMessages(dropped, nil)
	text = strings.TrimSpace(text)
	if text == "" {
		return "", context.Canceled
	}
	if len(text) > compactMaxInputChars {
		// Keep both ends: the beginning establishes the task, the end is the
		// most recent state.
		head := text[:compactMaxInputChars/2]
		tail := text[len(text)-compactMaxInputChars/2:]
		text = head + "\n\n[...middle omitted...]\n\n" + tail
	}
	acc, err := s.nextHealthyAccount("")
	if err != nil {
		return "", err
	}
	cfg := currentSettingsSafe()
	tone, toneErr := reasoningTone("gpt-5.6-luna", "low")
	if toneErr != nil {
		tone = "Gpt_5_6_Reasoning"
	}
	ctx, cancel := context.WithTimeout(r.Context(), compactTimeout)
	defer cancel()
	res, err := s.chatWithAccount(ctx, acc.ID, chathub.Account{
		AccessToken: acc.AccessToken,
		OID:         acc.OID,
		TID:         acc.TID,
	}, chathub.Request{
		Text:        compactSummaryPrompt + "\n\n<conversation>\n" + text + "\n</conversation>",
		Tone:        tone,
		LicenseType: cfg.LicenseType,
		Scenario:    cfg.Scenario,
		FeatureFlags: s.featureFlags(),
	})
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(res.Text)
	if summary == "" {
		return "", context.DeadlineExceeded
	}
	if len(summary) > compactMaxSummaryChars {
		summary = summary[:compactMaxSummaryChars]
	}
	return summary, nil
}
