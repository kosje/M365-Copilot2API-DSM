package web

import (
	"encoding/base64"
	"fmt"
	"strings"

	"m365-copilot2api/internal/chathub"
)

func parseContent(c any) (string, []chathub.Attachment) {
	var text strings.Builder
	var files []chathub.Attachment
	if s, ok := c.(string); ok {
		return s, nil
	}
	parts, ok := c.([]any)
	if !ok {
		return fmt.Sprint(c), nil
	}
	for _, raw := range parts {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		// Responses API uses input_text and may put image_url directly on
		// the content item rather than nesting it under image_url.
		if v, ok := m["text"].(string); ok && (typ == "text" || typ == "input_text" || typ == "output_text" || typ == "") {
			text.WriteString(v)
		}
		if direct, ok := m["image_url"].(string); ok && direct != "" && typ == "" {
			files = append(files, chathub.Attachment{Type: "image", URL: direct, MimeType: "image/*"})
		}
		switch typ {
		case "text", "input_text", "output_text":
			// handled above
		case "image_url":
			switch u := m["image_url"].(type) {
			case string:
				if u != "" {
					files = append(files, chathub.Attachment{Type: "image", URL: u, MimeType: "image/*"})
				}
			case map[string]any:
				if v, ok := u["url"].(string); ok {
					a := chathub.Attachment{Type: "image", URL: v, MimeType: "image/*"}
					if d, ok := u["detail"].(string); ok {
						a.Detail = d
					}
					files = append(files, a)
				}
			}
		case "input_image", "image":
			// Responses API accepts both image_url as a string and image_url
			// as an object containing url. Also accept nested source.url/data.
			u := stringValue(m, "image_url", "url", "source")
			if raw, ok := m["image_url"].(map[string]any); ok {
				u = stringValue(raw, "url", "data", "image_url")
			}
			if raw, ok := m["source"].(map[string]any); ok && u == "" {
				u = stringValue(raw, "url", "data", "source")
			}
			if u != "" {
				files = append(files, chathub.Attachment{Type: "image", URL: u, MimeType: "image/*"})
			}
		case "input_file", "file":
			// Document attachments (CSV/XLSX/PDF/text): upstream is a chat
			// service with no native file upload, so we extract the text
			// server-side and inline it into the prompt. Supported shapes:
			//   {type:"file", file:{name, mime, data:"data:...;base64,..."}}
			//   {type:"input_file", file_data:"data:...;base64,...", filename:"x.csv"}
			//   {type:"file", url:"data:...;base64,...", name:"x.csv"}
			name := stringValue(m, "filename", "name")
			mime := stringValue(m, "mime_type", "mimeType", "content_type")
			dataURI := ""
			if raw, ok := m["file"].(map[string]any); ok {
				if n, ok := raw["name"].(string); ok && n != "" && name == "" {
					name = n
				}
				if mm, ok := raw["mime"].(string); ok && mm != "" && mime == "" {
					mime = mm
				}
				if d, ok := raw["data"].(string); ok {
					dataURI = d
				}
				if d, ok := raw["url"].(string); ok && dataURI == "" {
					dataURI = d
				}
			}
			if dataURI == "" {
				dataURI = stringValue(m, "file_data", "data", "file_url", "url")
			}
			if dataURI == "" && name == "" {
				// no usable payload — keep legacy behaviour for file_id refs
				if u := stringValue(m, "file_id"); u != "" {
					files = append(files, chathub.Attachment{Type: "file", URL: u, Name: name, MimeType: mime})
				}
				break
			}
			text.WriteString("\n\n")
			if payload, ok := decodeDataURI(dataURI); ok {
				extracted, err := extractDocText(name, mime, payload)
				if err != nil {
					fmt.Fprintf(&text, "[附件文件 %s 无法读取：%v]", name, err)
				} else {
					text.WriteString(extracted)
				}
			} else {
				fmt.Fprintf(&text, "[附件文件 %s 的数据格式不受支持（仅支持 data URI）]", name)
			}
			text.WriteString("\n\n")
		case "input_audio", "audio":
			u := stringValue(m, "data", "audio_url", "url", "source")
			if u != "" {
				files = append(files, chathub.Attachment{Type: "audio", URL: u, MimeType: stringValue(m, "mime_type", "mimeType", "format", "content_type")})
			}
		}
	}
	return text.String(), files
}

func stringValue(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// decodeDataURI decodes "data:[mime];base64,XXXX" payloads. Plain base64
// (no prefix) is accepted as well.
func decodeDataURI(s string) ([]byte, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	const prefix = "base64,"
	if i := strings.Index(s, prefix); i >= 0 {
		s = s[i+len(prefix):]
	} else if strings.HasPrefix(s, "http") {
		return nil, false
	}
	// Strip data URI header when it survived the prefix cut (e.g. plain base64
	// input never had one; data:...;base64, was handled above).
	if i := strings.Index(s, ","); i >= 0 && strings.HasPrefix(s, "data:") {
		s = s[i+1:]
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(dec) == 0 {
		return nil, false
	}
	return dec, true
}
