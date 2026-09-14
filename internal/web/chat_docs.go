package web

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path"
	"strings"

	"github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
)

// ---------------------------------------------------------------------------
// Chat document attachments: extract plain text from common file formats so
// the content can be inlined into the conversation prompt for analysis.
// Upstream M365 Copilot is a chat service — it has no native file-upload API —
// so CSV/XLSX/PDF/text are flattened to text on the server.
// ---------------------------------------------------------------------------

const (
	// maxDocUploadBytes rejects absurd payloads before parsing (base64 body
	// is decoded before this check).
	maxDocUploadBytes = 20 << 20 // 20 MB
	// maxDocTextRunes caps the extracted text injected into the prompt.
	maxDocTextRunes = 60000
)

var textLikeExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".json": true, ".jsonl": true,
	".log": true, ".xml": true, ".yaml": true, ".yml": true, ".ini": true,
	".toml": true, ".conf": true, ".cfg": true, ".env": true,
	".py": true, ".js": true, ".ts": true, ".jsx": true, ".tsx": true,
	".go": true, ".java": true, ".c": true, ".cpp": true, ".h": true,
	".hpp": true, ".cs": true, ".rb": true, ".rs": true, ".php": true,
	".sh": true, ".bash": true, ".bat": true, ".ps1": true, ".sql": true,
	".html": true, ".htm": true, ".css": true, ".scss": true, ".vue": true,
	".srt": true, ".vtt": true, ".tsv": true,
}

// docSupported reports whether the file name looks like a format we can parse.
func docSupported(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".csv", ".xlsx", ".xls", ".pdf":
		return true
	default:
		return textLikeExts[strings.ToLower(path.Ext(name))]
	}
}

// extractDocText flattens a document to plain text for prompt injection.
// The result always starts with a small header describing the source file.
func extractDocText(name, mime string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("文件为空")
	}
	if len(data) > maxDocUploadBytes {
		return "", fmt.Errorf("文件超过 20MB 限制（%.1fMB）", float64(len(data))/(1<<20))
	}
	ext := strings.ToLower(path.Ext(name))
	var (
		body string
		err  error
	)
	switch ext {
	case ".csv":
		body, err = csvToText(name, data)
	case ".tsv":
		body, err = tsvToText(name, data)
	case ".xlsx", ".xls":
		body, err = xlsxToText(name, data)
	case ".pdf":
		body, err = pdfToText(name, data)
	default:
		if !textLikeExts[ext] {
			// Unknown extension: try plain text anyway, many files are textual.
			if !looksLikeText(data) {
				return "", fmt.Errorf("暂不支持该格式（%s）", ext)
			}
		}
		body, err = rawText(name, data)
	}
	if err != nil {
		return "", err
	}
	body = strings.TrimRight(capRunes(body, maxDocTextRunes), "\n")
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("未能从文件中提取到文本（可能是扫描件/加密文件/空文件）")
	}
	return fmt.Sprintf("【附件文件：%s】\n以下是该文件提取出的文本内容：\n\n%s\n【附件结束】", name, body), nil
}

// capRunes truncates s to at most n runes, appending a notice when cut.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n\n…（内容过长，已截断。如需分析后半部分，请拆分文件后重新上传）"
}

func looksLikeText(data []byte) bool {
	// Heuristic: reject binaries with NUL bytes in the first 4KB.
	n := len(data)
	if n > 4096 {
		n = 4096
	}
	return !bytes.ContainsRune(data[:n], 0)
}

func csvToText(name string, data []byte) (string, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil {
		return "", fmt.Errorf("CSV 解析失败: %v", err)
	}
	return rowsToMarkdown(name, rows, 400), nil
}

func tsvToText(name string, data []byte) (string, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = '\t'
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return "", fmt.Errorf("TSV 解析失败: %v", err)
	}
	return rowsToMarkdown(name, rows, 400), nil
}

// rowsToMarkdown renders the first rows as a compact markdown table.
func rowsToMarkdown(name string, rows [][]string, maxRows int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "文件 %s 共 %d 行，以下为表格内容（列以 | 分隔）：\n\n", name, len(rows))
	end := len(rows)
	if end > maxRows {
		end = maxRows
	}
	width := 0
	for i := 0; i < end; i++ {
		if len(rows[i]) > width {
			width = len(rows[i])
		}
	}
	if width > 30 {
		width = 30
	}
	trim := func(s string) string {
		s = strings.ReplaceAll(s, "|", "\\|")
		s = strings.ReplaceAll(s, "\n", " ")
		if len([]rune(s)) > 60 {
			s = string([]rune(s)[:60]) + "…"
		}
		return s
	}
	for i := 0; i < end; i++ {
		cells := make([]string, width)
		for j := 0; j < width; j++ {
			if j < len(rows[i]) {
				cells[j] = trim(rows[i][j])
			}
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		if i == 0 {
			b.WriteString("|" + strings.Repeat(" --- |", width) + "\n")
		}
	}
	if len(rows) > end {
		fmt.Fprintf(&b, "\n…（其余 %d 行已省略）\n", len(rows)-end)
	}
	return b.String()
}

func xlsxToText(name string, data []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("Excel 解析失败: %v", err)
	}
	defer f.Close()
	sheets := f.GetSheetList()
	var b strings.Builder
	fmt.Fprintf(&b, "Excel 文件 %s 包含 %d 个工作表。\n", name, len(sheets))
	const maxSheets = 5
	const maxRowsPerSheet = 200
	shown := 0
	for i, sheet := range sheets {
		if i >= maxSheets {
			fmt.Fprintf(&b, "\n…（其余 %d 个工作表已省略）\n", len(sheets)-maxSheets)
			break
		}
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		shown++
		fmt.Fprintf(&b, "\n### 工作表：%s（%d 行）\n\n", sheet, len(rows))
		end := len(rows)
		if end > maxRowsPerSheet {
			end = maxRowsPerSheet
		}
		for ri := 0; ri < end; ri++ {
			cells := rows[ri]
			if len(cells) > 30 {
				cells = cells[:30]
			}
			for j := range cells {
				cells[j] = strings.ReplaceAll(cells[j], "\n", " ")
				if len([]rune(cells[j])) > 60 {
					cells[j] = string([]rune(cells[j])[:60]) + "…"
				}
			}
			b.WriteString(strings.Join(cells, " | ") + "\n")
			if ri == 0 {
				b.WriteString(strings.Repeat("--- | ", len(cells)) + "\n")
			}
		}
		if len(rows) > end {
			fmt.Fprintf(&b, "…（其余 %d 行已省略）\n", len(rows)-end)
		}
	}
	if shown == 0 {
		return "", fmt.Errorf("Excel 中没有可读取的工作表")
	}
	return b.String(), nil
}

func pdfToText(name string, data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("PDF 解析失败: %v", err)
	}
	n := r.NumPage()
	var b strings.Builder
	const maxPages = 60
	shown := 0
	for i := 1; i <= n && i <= maxPages; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		txt, err := p.GetPlainText(nil)
		if err != nil {
			continue
		}
		shown++
		b.WriteString(txt)
		b.WriteString("\n")
	}
	if shown == 0 {
		return "", fmt.Errorf("PDF 中没有可提取的文本（可能是扫描图片型 PDF，需要 OCR）")
	}
	out := fmt.Sprintf("PDF 文件 %s 共 %d 页，以下为第 1-%d 页提取的文本：\n\n", name, n, shown) + b.String()
	if n > maxPages {
		out += fmt.Sprintf("\n…（其余 %d 页已省略）\n", n-maxPages)
	}
	return out, nil
}

func rawText(name string, data []byte) (string, error) {
	if !looksLikeText(data) {
		return "", fmt.Errorf("无法按文本读取该文件")
	}
	return fmt.Sprintf("文件 %s 的内容：\n\n%s", name, string(data)), nil
}
