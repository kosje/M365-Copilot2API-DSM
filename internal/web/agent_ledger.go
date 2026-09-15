package web

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type toolEvidence struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
	Failed    bool   `json:"failed"`
}

type meteringSnapshot struct {
	MeterError         string         `json:"meterError,omitempty"`
	HasAccess          bool           `json:"hasAccess"`
	RemainingAllowance map[string]int `json:"remainingAllowance,omitempty"`
	Timestamp          time.Time      `json:"timestamp"`
}

type agentLedger struct {
	Completed           []toolEvidence     `json:"completed"`
	Pending             []toolEvidence     `json:"pending"`
	ToolRounds          int                `json:"tool_rounds"`
	RepeatedCall        bool               `json:"repeated_call"`
	RepeatedFailure     bool               `json:"repeated_failure"`
	RepetitionSignature string             `json:"repetition_signature,omitempty"`
	StuckLoop           bool               `json:"stuck_loop"`
	Metering            []meteringSnapshot `json:"metering,omitempty"`
}

var failureSignal = regexp.MustCompile(`(?i)(exit\s*(code|status)?\s*[:=]?\s*[1-9]\d*|\berror\b|\bfailed\b|\bfailure\b|exception|traceback|timed?\s*out|permission denied|not found|refused)`)
var unsupportedSuccess = regexp.MustCompile(`(?i)\b(installed|created|written|executed|ran|started|deployed|deleted|verified|completed|succeeded|successful(?:ly)?)\b`)

func compactToolResult(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit < 200 {
		limit = 200
	}
	if len(s) <= limit {
		return s
	}
	head := limit / 3
	tail := limit - head - 80
	if tail < 80 {
		tail = 80
	}
	return s[:head] + fmt.Sprintf("\n... [truncated %d bytes] ...\n", len(s)-head-tail) + s[len(s)-tail:]
}

// scopedCallID returns a globally unique tool call id. The scope parameters
// are kept for signature compatibility with callers that pass per-turn
// context; the id itself must not depend on call content or scope text,
// otherwise repeating the same tool+arguments across turns collides
// (duplicate tool call id errors from clients).
func scopedCallID(name, args string, index int, scope string) string {
	return "call_" + uuid.NewString()
}
func buildAgentLedger(messages []oaiMsg) agentLedger {
	calls := map[string]toolEvidence{}
	order := []string{}
	for _, m := range messages {
		if m.Role == "assistant" {
			for _, raw := range m.ToolCalls {
				id, _ := raw["id"].(string)
				fn, _ := raw["function"].(map[string]any)
				name, _ := fn["name"].(string)
				// Cap argument size: a Write call carries the whole file
				// content in its arguments; serializing it verbatim into the
				// EVIDENCE_LEDGER appends tens of KB of JSON debris at the very
				// end of the prompt, drowning the actual user request.
				args := compactToolResult(fmt.Sprint(fn["arguments"]), 600)
				if id != "" {
					calls[id] = toolEvidence{ID: id, Name: name, Arguments: args}
					order = append(order, id)
				}
			}
		}
		if m.Role == "tool" {
			if e, ok := calls[m.ToolCallID]; ok {
				e.Result = compactToolResult(contentToString(m.Content), 4000)
				e.Failed = failureSignal.MatchString(e.Result)
				calls[m.ToolCallID] = e
			}
		}
	}
	l := agentLedger{}
	sameLimit := loopSameLimit()
	repeatLimit := loopRepeatLimit()
	// Adjacency-aware, progress-aware loop detection. The old logic counted
	// identical calls globally across the whole history, which aborted long
	// multi-step agent chains that legitimately re-issue reads/searches/checks.
	// Now a call only extends a "stuck" run when it is *consecutive* (the
	// immediately preceding call had the same name+args) AND returns the *same*
	// result (no progress). A re-read that yields different content breaks the
	// run instead of triggering a false positive.
	prevSig := ""
	consecSame := 0
	lastResult := map[string]string{}
	prevFail := ""
	consecFail := 0
	for _, id := range order {
		e := calls[id]
		l.ToolRounds++
		sig := e.Name + "\x00" + e.Arguments
		if sig == prevSig {
			if e.Result != "" && e.Result == lastResult[sig] {
				consecSame++
			} else {
				consecSame = 1
			}
		} else {
			consecSame = 1
			prevSig = sig
		}
		lastResult[sig] = e.Result
		if consecSame >= 2 {
			l.RepeatedCall = true
			l.RepetitionSignature = sig
		}
		if consecSame >= sameLimit {
			l.StuckLoop = true
		}
		if e.Result == "" {
			l.Pending = append(l.Pending, e)
		} else {
			l.Completed = append(l.Completed, e)
			if e.Failed {
				// Consecutive identical failures (same name+args+result) signal a
				// dead-end retry loop. A successful call, or a failure whose result
				// differs (the situation is changing), resets the run.
				fs := e.Name + "\x00" + e.Arguments + "\x00" + normalizeFailure(e.Result)
				if fs == prevFail {
					consecFail++
				} else {
					consecFail = 1
					prevFail = fs
				}
				if consecFail >= 2 {
					l.RepeatedFailure = true
					l.RepetitionSignature = fs
				}
				if consecFail >= repeatLimit {
					l.StuckLoop = true
				}
			} else {
				consecFail = 0
				prevFail = ""
			}
		}
	}
	return l
}
func normalizeFailure(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`\d+`).ReplaceAllString(s, "#")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
func (l agentLedger) RouterContext() string {
	type compact struct {
		Completed    []toolEvidence `json:"completed"`
		Pending      []toolEvidence `json:"pending"`
		RepeatedCall bool           `json:"repeated_call"`
	}
	b, _ := json.Marshal(compact{l.Completed, l.Pending, l.RepeatedCall})
	hint := "Use only this compact evidence. A completed call is final evidence; do not issue the same name and arguments again."
	if l.RepeatedFailure {
		hint += " The same call failed repeatedly; change strategy instead of retrying unchanged."
	}
	return hint + "\nEVIDENCE_LEDGER: " + string(b)
}
func canonicalToolArguments(s string) string {
	s = strings.TrimSpace(s)
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return s
}

func (l agentLedger) hasCompleted(name, args string) bool {
	want := canonicalToolArguments(args)
	for _, e := range l.Completed {
		if e.Name == name && canonicalToolArguments(e.Arguments) == want {
			return true
		}
	}
	return false
}
func filterCompletedCalls(calls []detectedToolCall, l agentLedger) []detectedToolCall {
	out := calls[:0]
	for _, c := range calls {
		if !l.hasCompleted(c.Name, string(c.Arguments)) {
			out = append(out, c)
		}
	}
	return out
}
func recordMetering(l *agentLedger, meterError string, hasAccess bool, remaining map[string]int) {
	if l == nil {
		return
	}
	snap := meteringSnapshot{
		MeterError:         meterError,
		HasAccess:          hasAccess,
		RemainingAllowance: remaining,
		Timestamp:          time.Now(),
	}
	l.Metering = append(l.Metering, snap)
}

func (l agentLedger) CanContinue(maxRounds int) error {
	if maxRounds <= 0 {
		maxRounds = 32
	}
	if l.ToolRounds >= maxRounds {
		return fmt.Errorf("tool round limit reached: %d", maxRounds)
	}
	if l.StuckLoop {
		return fmt.Errorf("stuck tool loop detected: same call repeated %d+ times or same failure repeated %d+ times", loopSameLimit(), loopRepeatLimit())
	}
	if l.RepeatedFailure {
		return fmt.Errorf("repeated tool failure detected: %s", l.RepetitionSignature)
	}
	if len(l.Pending) > 0 {
		return fmt.Errorf("pending tool results must be returned before another turn")
	}
	return nil
}
func maxToolRounds() int {
	if raw, ok := os.LookupEnv("M365_MAX_TOOL_ROUNDS"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 && n <= 512 {
			return n
		}
		return 32
	}
	if n := currentSettings().MaxToolRounds; n > 0 && n <= 512 {
		return n
	}
	return 32
}
func loopSameLimit() int {
	if raw, ok := os.LookupEnv("M365_LOOP_SAME_LIMIT"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 && n <= 64 {
			return n
		}
		return 3
	}
	if n := currentSettings().LoopSameLimit; n > 0 && n <= 64 {
		return n
	}
	return 3
}
func loopRepeatLimit() int {
	if raw, ok := os.LookupEnv("M365_LOOP_REPEAT_LIMIT"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 && n <= 64 {
			return n
		}
		return 5
	}
	if n := currentSettings().LoopRepeatLimit; n > 0 && n <= 64 {
		return n
	}
	return 5
}
func activeMessages(messages []oaiMsg) []oaiMsg {
	last := -1
	for i, m := range messages {
		if m.Role == "user" {
			last = i
		}
	}
	if last <= 0 {
		return messages
	}
	return messages[last:]
}
func completionEvidenceAllows(answer string, l agentLedger) bool {
	if len(l.Pending) > 0 {
		return false
	}
	if len(l.Completed) == 0 && len(l.Pending) == 0 {
		return !unsupportedSuccess.MatchString(answer)
	}
	low := strings.ToLower(answer)
	failureKeywords := []string{"cannot confirm", "not confirmed", "unable to confirm", "no tool result", "no matching tool results were returned", "no external action has been verified"}
	hasFailure := false
	for _, h := range failureKeywords {
		if strings.Contains(low, h) {
			hasFailure = true
			break
		}
	}
	if len(l.Completed) > 0 {
		return !hasFailure
	}
	if unsupportedSuccess.MatchString(answer) {
		return false
	}
	return true
}
func completedCallIDs(l agentLedger) []string {
	o := make([]string, 0, len(l.Completed))
	for _, e := range l.Completed {
		o = append(o, e.ID)
	}
	sort.Strings(o)
	return o
}
