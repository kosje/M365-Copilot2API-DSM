package web

import "testing"

func TestCompressToolResultsKeepsErrorsAndTail(t *testing.T) {
	// 6000 lines: a couple of errors buried in the middle, otherwise noise.
	lines := make([]string, 6000)
	for i := range lines {
		lines[i] = "installing dependency " + itoa(i)
	}
	lines[3000] = "npm ERR! code E404"
	lines[3001] = "npm ERR! 404 Not Found"
	raw := ""
	for i, l := range lines {
		if i > 0 {
			raw += "\n"
		}
		raw += l
	}
	msgs := []oaiMsg{{Role: "tool", Content: raw}}
	out := compressToolResults(msgs, 24000, false)
	got := contentToString(out[0].Content)
	if len(got) >= len(raw) {
		t.Fatalf("expected compression, got %d >= %d", len(got), len(raw))
	}
	if !contains(got, "npm ERR! code E404") || !contains(got, "npm ERR! 404 Not Found") {
		t.Fatalf("error lines dropped during compression: %q", got[:min(400, len(got))])
	}
	if !contains(got, "installing dependency 5999") {
		t.Fatalf("tail not preserved: %q", got[len(got)-min(200, len(got)):])
	}
}

func TestCapToolResultsTruncatesAndNotices(t *testing.T) {
	msgs := []oaiMsg{{Role: "tool", Content: "x"}}
	long := ""
	for i := 0; i < 5000; i++ {
		long += "a"
	}
	msgs[0].Content = long
	out := capToolResults(msgs, 100)
	got := contentToString(out[0].Content)
	if len(got) < 100 || len(got) > 400 {
		t.Fatalf("unexpected capped length %d", len(got))
	}
	if !contains(got, "result truncated") {
		t.Fatalf("truncation notice missing")
	}
}

func TestDedupeConsecutiveToolResults(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "tool", ToolCallID: "a", Content: "same"},
		{Role: "tool", ToolCallID: "a", Content: "same"},
		{Role: "tool", ToolCallID: "b", Content: "other"},
	}
	out := dedupeConsecutiveToolResults(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 after dedup, got %d", len(out))
	}
}

func TestPreprocessOffByDefaultIsNoop(t *testing.T) {
	msgs := []oaiMsg{{Role: "tool", Content: "a very long tool output that should remain untouched when nothing is enabled"}}
	out := preprocessMessages(msgs, runtimeSettings{})
	if contentToString(out[0].Content) != contentToString(msgs[0].Content) {
		t.Fatalf("preprocess changed content with empty config")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
