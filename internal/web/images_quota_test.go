package web

import (
	"fmt"
	"m365-copilot2api/internal/chathub"
	"testing"
)

func TestImageQuotaRefusal(t *testing.T) {
	for _, text := range []string{
		"Sorry, I can't generate any more images today.",
		"Sorry, try again tomorrow.",
		"抱歉，我今天无法再生成图片。请明天再试。",
	} {
		if !isImageQuotaRefusal(text) {
			t.Fatalf("quota refusal not detected: %q", text)
		}
	}
	if isImageQuotaRefusal("Here is your generated image.") {
		t.Fatal("ordinary image response misclassified")
	}
}

func TestImageQuotaErrorRecognizesStructuredMessages(t *testing.T) {
	for _, err := range []error{
		chathub.ErrImageLimit,
		fmt.Errorf("upstream image generation daily limit reached"),
		fmt.Errorf("image generation quota exhausted"),
	} {
		if !isImageQuotaError(err) {
			t.Fatalf("isImageQuotaError(%v) = false", err)
		}
	}
	if isImageQuotaError(fmt.Errorf("image generation system capacity temporarily unavailable")) {
		t.Fatal("system capacity error must not be classified as a daily image quota error")
	}
}
