package web

import "testing"

// The same comma-less data URL, this time straight from a caller's image_url.
// persistChatUserMessage indexed SplitN(...)[1] behind a HasPrefix("data:image/")
// guard, so "data:image/png" crashed the handler - and the same code path runs
// from the chat UI, not only from /v1.
func TestPersistChatUserMessageToleratesCommaLessDataURL(t *testing.T) {
	t.Setenv("M365_DATA_DIR", t.TempDir())
	s := &Server{chatUI: newChatUIStore()}

	for _, url := range []string{
		"data:image/png",
		"data:image/png;base64",
		"data:image/",
	} {
		url := url
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("persistChatUserMessage panicked on %q: %v", url, r)
				}
			}()
			text, imgs := s.persistChatUserMessage(map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "hi"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
				},
			})
			if text != "hi" {
				t.Errorf("text = %q, want %q", text, "hi")
			}
			if len(imgs) != 0 {
				t.Errorf("imgs = %v, want none for an unusable data URL", imgs)
			}
		}()
	}

	// A well-formed data URL still has to work, and be stored.
	text, imgs := s.persistChatUserMessage(map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url",
				"image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
		},
	})
	if text != "look" || len(imgs) != 1 {
		t.Fatalf("valid data URL: text=%q imgs=%v", text, imgs)
	}
}
