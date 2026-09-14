package web

import (
	"strings"
	"testing"
)

func TestComposeImagePrompt(t *testing.T) {
	p := composeImagePrompt("a cat on the moon", "anime", "text, watermark", "hd")
	if !strings.Contains(p, "a cat on the moon") {
		t.Fatalf("description missing: %q", p)
	}
	if !strings.Contains(p, "anime illustration") {
		t.Fatalf("style missing: %q", p)
	}
	if !strings.Contains(p, "ultra high definition") {
		t.Fatalf("quality missing: %q", p)
	}
	if !strings.Contains(p, "Strictly avoid") || !strings.Contains(p, "watermark") {
		t.Fatalf("negative missing: %q", p)
	}
	// auto style → no style injection
	p2 := composeImagePrompt("dog", "auto", "", "")
	if strings.Contains(p2, "Style:") {
		t.Fatalf("unexpected style injection: %q", p2)
	}
}

func TestNormalizeImageSize(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1024x1024",
		"16:9":      "1024x1024", // invalid → default
		" 1280x720": "1280x720",
		"9999x1":    "1024x1024",
		"":          "1024x1024",
	}
	for in, want := range cases {
		if got := normalizeImageSize(in); got != want {
			t.Errorf("normalizeImageSize(%q)=%q want %q", in, got, want)
		}
	}
}

func TestImageUserDisplay(t *testing.T) {
	got := imageUserDisplay("a cat", "864x1152", "anime", "hd", 2, true)
	for _, want := range []string{"a cat", "图生图", "864x1152", "动漫", "高清", "x2"} {
		if !strings.Contains(got, want) {
			t.Errorf("display %q missing %q", got, want)
		}
	}
	if got := imageUserDisplay("a cat", "1024x1024", "", "", 1, false); got != "a cat" {
		t.Errorf("default display should be plain prompt, got %q", got)
	}
}
