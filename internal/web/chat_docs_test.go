package web

import (
	"strings"
	"testing"
)

func TestExtractDocTextCSV(t *testing.T) {
	csv := "name,score\nalice,90\nbob,85\n"
	got, err := extractDocText("scores.csv", "text/csv", []byte(csv))
	if err != nil {
		t.Fatalf("csv: %v", err)
	}
	if !strings.Contains(got, "scores.csv") || !strings.Contains(got, "alice") || !strings.Contains(got, "| 90 |") {
		t.Fatalf("csv extraction incomplete: %q", got)
	}
	if !strings.HasPrefix(got, "【附件文件：") {
		t.Fatalf("missing header: %q", got)
	}
}

func TestExtractDocTextRawText(t *testing.T) {
	got, err := extractDocText("notes.md", "text/markdown", []byte("# 标题\n内容"))
	if err != nil {
		t.Fatalf("md: %v", err)
	}
	if !strings.Contains(got, "# 标题") {
		t.Fatalf("md content missing: %q", got)
	}
}

func TestExtractDocTextRejectsBinary(t *testing.T) {
	data := make([]byte, 512)
	for i := range data {
		data[i] = 0
	}
	if _, err := extractDocText("blob.bin", "application/octet-stream", data); err == nil {
		t.Fatal("expected error for binary blob")
	}
}

func TestExtractDocTextEmpty(t *testing.T) {
	if _, err := extractDocText("x.csv", "", nil); err == nil {
		t.Fatal("expected error for empty file")
	}
}

func TestCapRunes(t *testing.T) {
	long := strings.Repeat("a", maxDocTextRunes+10)
	got := capRunes(long, maxDocTextRunes)
	if len([]rune(got)) <= maxDocTextRunes {
		t.Fatal("expected truncation notice appended")
	}
	if !strings.Contains(got, "已截断") {
		t.Fatal("missing truncation marker")
	}
}
