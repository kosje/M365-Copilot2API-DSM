package web

import (
	"strings"
	"testing"
)

// Regression guard for the v1.6.12 fix: a tool-less request that asks to create
// a file but comes back with a downloadable artifact link must trip BOTH the
// fileOpIntent gate and isArtifactFallback, so the artifact-eject retry in
// server.go fires (instead of passing the asyncgw link straight to the user).
func TestFileOpIntentAndArtifactFallback(t *testing.T) {
	// TEST 1 exact user prompt (no tools, no absolute path).
	prompt := "在这个文件夹下创建个bat文件，内容是清理windows垃圾文件"
	if !fileOpIntent(prompt) {
		t.Fatalf("fileOpIntent(%q) = false, want true (gate would never open)", prompt)
	}

	// Response the upstream model actually returned in TEST 1.
	artifact := "已创建 BAT 文件：[下载BAT文件](https://kr-prod.asyncgw.teams.microsoft.com/v1/objects/abc-123/views/original/cleanup.bat?foo=bar)"
	if !isArtifactFallback(artifact) {
		t.Fatalf("isArtifactFallback(...) = false, want true (retry would never fire)")
	}

	// The combined gate used in server.go:3129 must be true for tools=0.
	tools := 0
	if !(tools == 0 && fileOpIntent(prompt) && isArtifactFallback(artifact)) {
		t.Fatalf("combined eject gate is false for tools=0 file-intent artifact response")
	}

	// Sanity: an honest no-tools reply must NOT be flagged as an artifact.
	honest := "我无法直接访问你电脑上的路径。请开启 Agent 模式并授予文件完全访问权限，模型才能直接写入你本机文件。"
	if isArtifactFallback(honest) {
		t.Fatalf("isArtifactFallback(honest reply) = true, would wrongly eject an honest answer")
	}

	// Backstop: stripArtifactLinks must remove both the markdown link and the
	// bare asyncgw URL so the caller never sees a fake download.
	dirty := "已创建 BAT 文件：[下载 BAT 文件](https://kr-prod.asyncgw.teams.microsoft.com/v1/objects/abc/views/original/cleanup.bat) 内容见下。"
	clean := stripArtifactLinks(dirty)
	if strings.Contains(clean, "asyncgw") || strings.Contains(clean, "teams.microsoft.com") {
		t.Fatalf("stripArtifactLinks left an artifact URL: %q", clean)
	}
	if !strings.Contains(clean, "已创建 BAT 文件") {
		t.Fatalf("stripArtifactLinks removed non-link text: %q", clean)
	}
}
