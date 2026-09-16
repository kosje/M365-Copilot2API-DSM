package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"m365-copilot2api/internal/auth"
	"net/http"
	"strings"
	"time"
)

// maxImportBytes caps the uploaded payload (16 MB is far beyond any realistic
// accounts.json / migration archive).
const maxImportBytes = 16 << 20

type importAccountsResult struct {
	Received int      `json:"received"`
	Imported int      `json:"imported"`
	Updated  int      `json:"updated"`
	Errors   []string `json:"errors,omitempty"`
}

// importAccounts accepts a multipart upload containing either:
//   1) a raw accounts.json in the app's own format {"accounts":[...]}, or
//   2) a migration archive (gzip/tar.gz) produced by migrate.sh, from which
//      m365-migration/data/accounts.json is extracted.
// Each account is upserted into the token store. Refresh tokens are kept
// verbatim: encryptRefreshToken is idempotent for already-encrypted values, so
// re-saving does not double-encrypt. The master key falls back to a built-in
// constant when M365_MASTER_KEY is unset, which keeps tokens portable across
// instances by default.
func (s *Server) importAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	if err := r.ParseMultipartForm(maxImportBytes); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to parse upload: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing 'file' field in upload")
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxImportBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read upload")
		return
	}

	accountsJSON, err := extractAccountsJSON(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	var parsed struct {
		Accounts []auth.AccountToken `json:"accounts"`
	}
	if err := json.Unmarshal(accountsJSON, &parsed); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "accounts.json 解析失败: "+err.Error())
		return
	}
	if len(parsed.Accounts) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "accounts.json 中未找到任何账号")
		return
	}

	res := importAccountsResult{}
	for _, a := range parsed.Accounts {
		res.Received++
		existed := false
		if a.ID != "" {
			if _, ok := s.tokens.Get(a.ID); ok {
				existed = true
			}
		}
		if !existed && a.Email != "" {
			if _, ok := s.tokens.Get(a.Email); ok {
				existed = true
			}
		}
		ts := auth.TokenSet{
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
			Email:        a.Email,
			DisplayName:  a.DisplayName,
			ExpiresAt:    a.ExpiresAt,
			HomeOID:      a.OID,
			TenantID:     a.TID,
		}
		if _, err := s.tokens.Upsert(ts); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", emailOrID(a), err))
			continue
		}
		if existed {
			res.Updated++
		} else {
			res.Imported++
		}
	}
	// Refresh metering for the whole pool so newly imported accounts show
	// up-to-date quotas on the dashboard.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		s.refreshAllMetering(ctx)
	}()
	jsonOut(w, res)
}

func emailOrID(a auth.AccountToken) string {
	if a.Email != "" {
		return a.Email
	}
	return a.ID
}

// extractAccountsJSON returns the raw accounts.json bytes, whether the upload is
// a plain JSON document or a gzip/tar.gz migration archive.
func extractAccountsJSON(raw []byte) ([]byte, error) {
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		return accountsFromArchive(raw)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("上传内容既不是 JSON 也不是 gzip/tar.gz 归档")
	}
	return raw, nil
}

// accountsFromArchive reads a tar.gz and returns the first member ending in
// accounts.json (e.g. m365-migration/data/accounts.json from migrate.sh).
func accountsFromArchive(raw []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("归档不是有效的 gzip/tar.gz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档失败: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && strings.HasSuffix(hdr.Name, "accounts.json") {
			// Cap the *decompressed* size. maxImportBytes only bounds the
			// uploaded bytes, and gzip expands by orders of magnitude, so
			// without this limit a 16 MiB upload can allocate until the process
			// is killed. Check the declared size first (cheap) and then enforce
			// it while reading (the header is attacker-controlled).
			if hdr.Size > maxImportBytes {
				return nil, fmt.Errorf("归档中的 accounts.json 解压后过大（%d 字节，上限 %d 字节）", hdr.Size, maxImportBytes)
			}
			data, err := io.ReadAll(io.LimitReader(tr, maxImportBytes+1))
			if err != nil {
				return nil, fmt.Errorf("读取归档中的 accounts.json 失败: %w", err)
			}
			if int64(len(data)) > maxImportBytes {
				return nil, fmt.Errorf("归档中的 accounts.json 解压后超过 %d 字节上限", maxImportBytes)
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("归档中未找到 accounts.json")
}
