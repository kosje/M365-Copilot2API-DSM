package web

import (
	"bytes"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStoredPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestGeneratedImageSurvivesServerRestart(t *testing.T) {
	t.Setenv("M365_DATA_DIR", t.TempDir())
	data := testStoredPNG(t)
	id, err := (&Server{}).storeGeneratedImage(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	// A different Server has no in-memory images, as after a package restart.
	rr := httptest.NewRecorder()
	(&Server{}).generatedImageFile(rr, httptest.NewRequest("GET", "/v1/images/files/"+id, nil))
	if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), data) || rr.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("download after restart failed: %d", rr.Code)
	}
	old := time.Now().Add(-20 * time.Minute)
	path := filepath.Join(generatedImageDir(), id)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Server{}).loadGeneratedImage(id); err != nil {
		t.Fatalf("image expired at old 15 minute limit: %v", err)
	}
	old = time.Now().Add(-generatedImageRetention - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	(&Server{}).generatedImageFile(rr, httptest.NewRequest("GET", "/v1/images/files/"+id, nil))
	if rr.Code != 404 || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expired image status=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "请重新生成") {
		t.Fatalf("unreadable error message: %q", rr.Body.String())
	}
}

func TestGeneratedImageStorageFailureReturnsNoURL(t *testing.T) {
	t.Setenv("M365_DATA_DIR", t.TempDir())
	if err := os.WriteFile(chatDataDir(), []byte("blocked directory"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := (&Server{}).storeGeneratedImage(testStoredPNG(t), "image/png")
	if err == nil || id != "" {
		t.Fatalf("storage failure claimed success: id=%q err=%v", id, err)
	}
}

func TestGeneratedImageStoreBoundsCount(t *testing.T) {
	t.Setenv("M365_DATA_DIR", t.TempDir())
	s := &Server{}
	data := testStoredPNG(t)
	first, err := s.storeGeneratedImage(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(generatedImageDir(), first), old, old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxGeneratedImages; i++ {
		if _, err := s.storeGeneratedImage(data, "image/png"); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(generatedImageDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != maxGeneratedImages {
		t.Fatalf("unbounded count: %d", len(entries))
	}
	if _, err := s.loadGeneratedImage(first); !os.IsNotExist(err) {
		t.Fatalf("oldest image not evicted: %v", err)
	}
}

func TestChatImagePromptSkipsTrailingClientMetadata(t *testing.T) {
	prompt := "按以下提示词生图：生成一张唯美东方仙侠主题竖版海报。白衣男子持玉笛，不添加文字。"
	meta := "You are powered by the model gpt-image-2. 15000000 tokens left"
	for _, messages := range [][]oaiMsg{
		{{Role: "user", Content: prompt}, {Role: "user", Content: meta}},
		{{Role: "user", Content: []any{map[string]any{"type": "text", "text": prompt}, map[string]any{"type": "text", "text": meta}}}},
		{{Role: "user", Content: prompt + "\n<system-reminder>" + meta + "</system-reminder>"}},
	} {
		got := chatImagePrompt(messages)
		if got != prompt {
			t.Fatalf("wrong image description: %q", got)
		}
		if !shouldUseChatImageRoute("gpt-5.6-sol", got, nil) {
			t.Fatal("ordinary model missed image route")
		}
	}
	if got := chatImagePrompt([]oaiMsg{{Role: "user", Content: prompt}, {Role: "assistant", Content: "done"}, {Role: "user", Content: meta}}); got != "" {
		t.Fatalf("replayed old request: %q", got)
	}
	if got := chatImagePrompt([]oaiMsg{{Role: "user", Content: meta}}); strings.TrimSpace(got) != "" {
		t.Fatalf("metadata treated as image prompt: %q", got)
	}
}
