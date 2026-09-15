package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteChatCompletionTextNonStreamingUsage(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	server := &Server{}
	server.writeChatCompletionText(rr, req, "gpt-5.6-sol", "![image](http://example/image.png)", false, true, 17)

	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	usage := out["usage"].(map[string]any)
	if got := int64(usage["prompt_tokens"].(float64)); got != 17 {
		t.Fatalf("prompt_tokens=%d want 17", got)
	}
	choices := out["choices"].([]any)
	content := choices[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "![image]") {
		t.Fatalf("image markdown missing: %q", content)
	}
}

func TestWriteChatCompletionTextStreamingUsageOption(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sendUsage bool
		wantUsage bool
	}{
		{name: "disabled", sendUsage: false, wantUsage: false},
		{name: "enabled", sendUsage: true, wantUsage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			server := &Server{}
			server.writeChatCompletionText(rr, req, "gpt-5.6-sol", "![image](http://example/image.png)", true, tc.sendUsage, 23)
			body := rr.Body.String()
			if !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, `"finish_reason":"stop"`) {
				t.Fatalf("invalid SSE completion: %s", body)
			}
			if got := strings.Contains(body, `"usage":`); got != tc.wantUsage {
				t.Fatalf("usage present=%v want %v: %s", got, tc.wantUsage, body)
			}
		})
	}
}
