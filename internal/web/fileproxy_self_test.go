package web

import (
	"bytes"
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
	// Ends with %%EOF like a real PDF: validFileTail now rejects a file whose
	// terminator is missing, since that is what a re-emit cut short looks like.
	pdf := []byte("%PDF-1.3\n1 0 obj<<>>endobj\nstartxref\n9\n%%EOF\n")
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

func TestValidFileTailRejectsTruncation(t *testing.T) {
	full := []byte("%PDF-1.3\nbody bytes here\nstartxref\n2024\n%%EOF\n")
	if err := validFileTail(full); err != nil {
		t.Fatalf("complete PDF rejected: %v", err)
	}
	// What a model hitting its output limit produces: a correct header and no
	// terminator. The magic check alone passes this, which is the whole point.
	cut := full[:20]
	if !validFileMagic(cut) {
		t.Fatal("precondition: truncated PDF should still pass the magic check")
	}
	if err := validFileTail(cut); err == nil {
		t.Fatal("truncated PDF accepted")
	}

	zip := append([]byte{'P', 'K', 0x03, 0x04}, bytes.Repeat([]byte{0}, 64)...)
	if err := validFileTail(zip); err == nil {
		t.Fatal("zip without end-of-central-directory accepted")
	}
	zip = append(zip, 'P', 'K', 0x05, 0x06)
	if err := validFileTail(zip); err != nil {
		t.Fatalf("complete zip rejected: %v", err)
	}

	// Formats with no terminator rule must pass rather than be rejected.
	if err := validFileTail([]byte("%!PS-Adobe-3.0\nstuff")); err != nil {
		t.Fatalf("unchecked format rejected: %v", err)
	}
}
