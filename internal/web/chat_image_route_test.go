package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestRequestContextEnded(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if requestContextEnded(req, nil) {
		t.Fatal("live request was classified as ended")
	}
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	if !requestContextEnded(req.WithContext(ctx), nil) {
		t.Fatal("canceled request context was not detected")
	}
}

func TestChatImageRouteTimeouts(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		total   time.Duration
		attempt time.Duration
	}{
		{seconds: 2, total: 5 * time.Second, attempt: 5 * time.Second},
		{seconds: 120, total: 2 * time.Minute, attempt: 2 * time.Minute},
		{seconds: 300, total: 5 * time.Minute, attempt: 3 * time.Minute},
		{seconds: 3600, total: 10 * time.Minute, attempt: 3 * time.Minute},
	} {
		total, attempt := chatImageRouteTimeouts(tc.seconds)
		if total != tc.total || attempt != tc.attempt {
			t.Fatalf("seconds=%d got (%s,%s), want (%s,%s)", tc.seconds, total, attempt, tc.total, tc.attempt)
		}
	}
}

func TestChatImageKeepaliveAndStreamingErrorStaySSECompatible(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	stop, ok := startChatImageKeepalive(rr, req)
	if !ok {
		t.Fatal("httptest recorder should support flushing")
	}
	stop()
	writeChatImageRouteError(rr, true, errors.New("no image returned"))
	body := rr.Body.String()
	for _, want := range []string{": image generation started", `"type":"image_generation_error"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE route response missing %q: %s", want, body)
		}
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
