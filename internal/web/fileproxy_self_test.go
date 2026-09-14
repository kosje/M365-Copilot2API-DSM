package web

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestExtractAsyncGWFiles(t *testing.T) {
	u := "https://kr-prod.asyncgw.teams.microsoft.com/v1/objects/abc-123/views/original/hello%20report.pdf?foo=bar"
	text := "Here is your file: [hello report.pdf](" + u + ") and also bare " + u
	files := extractAsyncGWFiles(text)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	var name string
	for _, n := range files {
		name = n
	}
	if name != "hello report.pdf" {
		t.Fatalf("bad name: %q", name)
	}
}

func TestRewriteAsyncGWToProxy(t *testing.T) {
	u := "https://kr-prod.asyncgw.teams.microsoft.com/v1/objects/abc/views/original/report.pdf"
	text := "Download: " + u
	var srv Server
	out := srv.rewriteAsyncGWToProxy(text, "conv-9")
	want := "Download: /api/chatui/fileproxy?conv=conv-9&name=report.pdf"
	if out != want {
		t.Fatalf("rewrite mismatch:\n got: %q\nwant: %q", out, want)
	}
}

func TestExtractBase64File(t *testing.T) {
	pdf := []byte("%PDF-1.3\n1 0 obj<<>>endobj\n")
	enc := base64.StdEncoding.EncodeToString(pdf)
	resp := "Sure, here is the file:\n```base64file\n" + enc + "\n```\nHope that helps!"
	data, err := extractBase64File(resp)
	if err != nil {
		t.Fatalf("extractBase64File failed: %v", err)
	}
	if !strings.HasPrefix(string(data), "%PDF") {
		t.Fatalf("decoded bytes not a PDF: %q", string(data))
	}
}
