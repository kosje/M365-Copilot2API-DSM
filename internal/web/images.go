package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	designerAppServiceScope  = "https://designerappservice.officeapps.live.com/.default"
	maxGeneratedImageBytes   = 20 << 20
	maxImageEditRequestBytes = maxGeneratedImageBytes + (2 << 20)
	generatedImageTTL        = 15 * time.Minute
	maxGeneratedImages       = 128
)

type generatedImage struct {
	Data        []byte
	ContentType string
	ExpiresAt   time.Time
}

type imageGenerationRequest struct {
	Prompt         string               `json:"prompt"`
	N              int                  `json:"n"`
	Size           string               `json:"size"`
	ResponseFormat string               `json:"response_format"`
	Model          string               `json:"model"`
	AccountID      string               `json:"accountId"`
	User           string               `json:"user"`
	Operation      string               `json:"operation,omitempty"`
	Attachments    []chathub.Attachment `json:"attachments,omitempty"`
}

// nextImageGenAccount 返回下一个「健康且画图配额未耗尽」的轮询账号（跳过 avoidID）。
// 与 nextHealthyAccount 的区别：额外检查 ImageGenAvailable（画图每日上限冷却），
// 避免把请求反复打到已耗尽画图配额的账号上。
func (s *Server) nextImageGenAccount(avoidID string) (auth.AccountToken, error) {
	seen := map[string]bool{}
	if avoidID != "" {
		seen[avoidID] = true
	}
	return s.nextUntriedImageGenAccount(seen)
}

func (s *Server) nextUntriedImageGenAccount(seen map[string]bool) (auth.AccountToken, error) {
	for i := 0; i < maxAccountProbe; i++ {
		acc, ok := s.tokens.Next()
		if !ok {
			return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
		}
		if seen[acc.ID] {
			continue
		}
		if !s.accountAvailable(acc.ID) {
			continue
		}
		if !s.accountPool.ImageGenAvailable(acc.ID) {
			continue
		}
		return s.tokens.EnsureValid(acc.ID)
	}
	return auth.AccountToken{}, fmt.Errorf("no account with image quota available for failover")
}

func (s *Server) imageGenerations(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var b imageGenerationRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxImageEditRequestBytes)
	if json.NewDecoder(r.Body).Decode(&b) != nil || strings.TrimSpace(b.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	if b.N <= 0 {
		b.N = 1
	}
	if b.N > 10 {
		writeOpenAIError(w, 400, "invalid_request_error", "n must be between 1 and 10")
		return
	}
	format := strings.ToLower(strings.TrimSpace(b.ResponseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "response_format must be url or b64_json")
		return
	}
	size := b.Size
	if size == "" {
		size = "1024x1024"
	}
	endpoint := "/v1/images/generations"
	prompt := fmt.Sprintf("Generate an image with GPT Image 2. Size: %s. Description: %s. Return the image URL directly.", size, b.Prompt)
	if b.Operation == "edit" {
		if len(b.Attachments) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "image is required")
			return
		}
		endpoint = "/v1/images/edits"
		prompt = fmt.Sprintf("Edit the first attached image with GPT Image 2. Size: %s. Instructions: %s. Preserve everything not requested to change. Return the edited image URL directly.", size, b.Prompt)
	}
	// 画图配额是账号级限制：命中每日上限时轮换到下一个有配额的账号重试，
	// 而不是直接把错误抛给客户端（客户端显式指定账号时除外）。
	explicit := firstNonEmpty(b.AccountID, b.User) != ""
	var res chathub.Result
	found := false
	var successAcc auth.AccountToken
	var lastErr error
	tried := map[string]bool{}
	for attempt := 0; attempt < maxAccountProbe; attempt++ {
		var acc auth.AccountToken
		var err error
		if attempt == 0 {
			acc, err = s.resolveAccount(firstNonEmpty(b.AccountID, b.User))
			if err == nil && !explicit && !s.accountPool.ImageGenAvailable(acc.ID) {
				// 首选账号的画图配额仍在冷却期，直接跳到下一个
				if next, nerr := s.nextImageGenAccount(acc.ID); nerr == nil {
					log.Printf("[image-gen] account=%s image quota cooling down; rotating", acc.ID)
					acc = next
				}
			}
		} else {
			acc, err = s.nextUntriedImageGenAccount(tried)
		}
		if err != nil {
			if attempt == 0 {
				writeUpstreamError(w, err)
				return
			}
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		tried[acc.ID] = true
		if acc.OID == "" || acc.TID == "" {
			acc.OID, acc.TID = extractOIDTID(acc.AccessToken)
		}
		if acc.OID == "" || acc.TID == "" {
			if attempt == 0 {
				writeOpenAIError(w, 400, "invalid_request_error", "account missing oid/tid — re-login with PKCE")
				return
			}
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ImageTimeoutSeconds)*time.Second)
		res, err = s.chatWithAccount(ctx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{Text: prompt, Tone: "magic", Attachments: b.Attachments, LicenseType: s.settings.get().LicenseType, Scenario: s.settings.get().Scenario, FeatureFlags: s.featureFlags()})
		cancel()
		if err != nil {
			lastErr = err
			if isImageQuotaError(err) {
				// 标记该账号画图配额耗尽（冷却到 UTC 明日零点），
				// 后续画图请求选号时直接跳过它。
				s.accountPool.MarkImageGenTokensThrottled(acc.ID)
			} else if errors.Is(err, chathub.ErrMeteringThrottled) {
				// Temporary Designer capacity throttles are not a daily quota
				// exhaustion, but retrying the same account immediately causes a
				// long sequence of image timeouts. Keep it out of image rotation
				// for a short period and allow another account to serve the request.
				s.accountPool.MarkImageGenSystemThrottled(acc.ID)
			}
			retryable := isImageQuotaError(err) || IsEmptyCompletion(err) || IsRateLimited(err) || upstreamStatus(err) == http.StatusTooManyRequests || IsRetryable(err)
			if explicit || !retryable {
				writeUpstreamError(w, err)
				return
			}
			log.Printf("[image-gen] account=%s hit image limit; rotating to next account", acc.ID)
			continue
		}
		if len(res.Images) == 0 {
			if urls := extractImageURLs(res.RawResult); len(urls) > 0 {
				res.Images = urls
			}
		}
		if len(res.Images) == 0 {
			if urls := extractImageURLs(res.Text); len(urls) > 0 {
				res.Images = urls
			}
		}
		if len(res.Images) == 0 && !explicit && isImageQuotaRefusal(strings.Join([]string{res.Text, res.RawResult}, "\n")) {
			// 上游以文本拒绝（配额耗尽）而非 429 状态码，同样轮换
			s.accountPool.MarkImageGenTokensThrottled(acc.ID)
			log.Printf("[image-gen] account=%s quota refusal in text; rotating to next account", acc.ID)
			continue
		}
		found = true
		successAcc = acc
		break
	}
	if !found {
		if lastErr == nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "no image returned")
			return
		}
		if isImageQuotaError(lastErr) || IsRateLimited(lastErr) {
			w.Header().Set("Retry-After", "86400")
			writeOpenAIError(w, http.StatusTooManyRequests, "image_limit_error", "image generation daily limit reached; try again tomorrow")
			return
		}
		writeUpstreamError(w, lastErr)
		return
	}
	log.Printf("[image-gen] conversation=%s images=%d text_len=%d events=%d raw_len=%d", res.ConversationID, len(res.Images), len(res.Text), len(res.Events), len(res.RawResult))
	if len(res.Images) == 0 {
		refusalText := strings.Join([]string{res.Text, res.RawResult}, "\n")
		if isImageQuotaRefusal(refusalText) {
			w.Header().Set("Retry-After", "86400")
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "M365 image generation quota is exhausted; try again later or use another account")
			return
		}
		textPreview := res.Text
		if len(textPreview) > 500 {
			textPreview = textPreview[:500]
		}
		rawPreview := ""
		if len(res.RawResult) > 0 {
			rawPreview = res.RawResult
			if len(rawPreview) > 500 {
				rawPreview = rawPreview[:500]
			}
		}
		debug := map[string]any{"text": textPreview, "raw_len": len(res.RawResult), "events": len(res.Events), "images": res.Images, "raw_preview": rawPreview}
		dbg, _ := json.Marshal(debug)
		log.Printf("[image-gen-debug] %s", string(dbg))
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned no image resource")
		return
	}
	images := res.Images
	if len(images) > b.N {
		images = images[:b.N]
	}

	var designerToken string
	// 下载阶段使用独立超时：生成可能耗尽大半总预算，若复用同一 ctx，
	// 慢生成后下载只剩几秒必然超时（对齐 chatui 实测：生成 135s + 下载 15s 失败）。
	dlTimeout := time.Duration(s.settings.get().ImageTimeoutSeconds) * time.Second
	if dlTimeout < 90*time.Second {
		dlTimeout = 90 * time.Second
	}
	downloadFailed := false
	data := make([]map[string]string, 0, len(images))
	for _, sourceURL := range images {
		if strings.HasPrefix(strings.ToLower(sourceURL), "data:image/") {
			if format == "b64_json" {
				parts := strings.SplitN(sourceURL, ",", 2)
				if len(parts) != 2 {
					writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "invalid upstream image data")
					return
				}
				data = append(data, map[string]string{"b64_json": parts[1]})
			} else {
				data = append(data, map[string]string{"url": sourceURL})
			}
			continue
		}
		if !isDesignerImageURL(sourceURL) {
			if format == "b64_json" {
				writeOpenAIError(w, http.StatusBadGateway, "unsupported_response_format", "upstream returned URL, not b64_json")
				return
			}
			data = append(data, map[string]string{"url": sourceURL})
			continue
		}
		if designerToken == "" {
			var derr error
			designerToken, derr = s.designerAccessToken(successAcc)
			if derr != nil {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", upstreamError(derr))
				return
			}
		}
		dlCtx, dlCancel := context.WithTimeout(r.Context(), dlTimeout)
		imageData, contentType, err := downloadDesignerImage(dlCtx, sourceURL, designerToken)
		dlCancel()
		if err != nil {
			// 图片已在上游生成成功，仅本机下载失败（设备可能无代理直连图片 CDN）。
			// 不再整体失败：回退返回上游 URL，由前端提示用户自行下载。
			log.Printf("[image-gen-download] err=%v (fallback to upstream URL)", err)
			downloadFailed = true
			data = append(data, map[string]string{"url": sourceURL})
			continue
		}
		if format == "b64_json" {
			data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(imageData)})
			continue
		}
		id, err := s.storeGeneratedImage(imageData, contentType)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "image_storage_error", "generated image could not be saved")
			return
		}
		data = append(data, map[string]string{"url": s.generatedImageURL(r, id)})
	}

	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		AccountEmail: successAcc.Email,
		Model:        firstNonEmpty(b.Model, "gpt-image-2"),
		Endpoint:     endpoint,
		InputTokens:  EstimateTokens(prompt),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
	out := map[string]any{"created": time.Now().Unix(), "data": data, "m365": map[string]any{"conversationId": res.ConversationID, "sessionId": res.SessionID, "images": images}}
	if downloadFailed {
		out["warning"] = "image(s) generated upstream, but server-side download failed; the device may have no direct access to the image CDN — open the URL(s) in data to download manually"
	}
	jsonOut(w, out)
}

func (s *Server) imageEdits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageEditRequestBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid multipart image edit request")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		file, header, err = r.FormFile("image[]")
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "image is required")
		return
	}
	defer file.Close()
	imageData, err := io.ReadAll(io.LimitReader(file, maxGeneratedImageBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "could not read image")
		return
	}
	if len(imageData) > maxGeneratedImageBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "image exceeds 20 MiB")
		return
	}
	contentType := http.DetectContentType(imageData)
	ext := ""
	switch contentType {
	case "image/png":
		ext = "png"
	case "image/jpeg":
		ext = "jpg"
	case "image/webp":
		ext = "webp"
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "image must be PNG, JPEG, or WebP")
		return
	}
	name := strings.TrimSpace(header.Filename)
	if name == "" {
		name = "image." + ext
	}
	n := 1
	if rawN := strings.TrimSpace(r.FormValue("n")); rawN != "" {
		n, err = strconv.Atoi(rawN)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "n must be an integer")
			return
		}
	}
	body := imageGenerationRequest{
		Prompt:         prompt,
		N:              n,
		Size:           strings.TrimSpace(r.FormValue("size")),
		ResponseFormat: strings.TrimSpace(r.FormValue("response_format")),
		Model:          strings.TrimSpace(r.FormValue("model")),
		AccountID:      firstNonEmpty(strings.TrimSpace(r.FormValue("accountId")), strings.TrimSpace(r.FormValue("account_id"))),
		User:           strings.TrimSpace(r.FormValue("user")),
		Operation:      "edit",
		Attachments: []chathub.Attachment{{
			Type:     "image",
			URL:      "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(imageData),
			Name:     name,
			MimeType: contentType,
		}},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "could not encode image edit request")
		return
	}
	next := r.Clone(r.Context())
	next.Body = io.NopCloser(bytes.NewReader(encoded))
	next.ContentLength = int64(len(encoded))
	next.Header = r.Header.Clone()
	next.Header.Set("Content-Type", "application/json")
	next.Form = nil
	next.PostForm = nil
	next.MultipartForm = nil
	s.imageGenerations(w, next)
}

func (s *Server) designerAccessToken(acc auth.AccountToken) (string, error) {
	if strings.TrimSpace(acc.RefreshToken) == "" {
		return "", fmt.Errorf("account has no refresh token for Designer image download")
	}
	clientID := firstNonEmpty(acc.ClientID, auth.ClientID())
	set, err := auth.RefreshWithScope(acc.RefreshToken, clientID, designerAppServiceScope)
	if err != nil {
		return "", fmt.Errorf("obtain Designer image token: %w", err)
	}
	if set.RefreshToken != "" && set.RefreshToken != acc.RefreshToken {
		if err := s.tokens.UpdateRefreshToken(acc.ID, set.RefreshToken); err != nil {
			log.Printf("[image-gen] rotated refresh token could not be persisted account=%s err=%v", acc.ID, err)
		}
	}
	return set.AccessToken, nil
}

func isDesignerImageURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "designerapp.officeapps.live.com")
}

func downloadDesignerImage(ctx context.Context, rawURL, accessToken string) ([]byte, string, error) {
	if !isDesignerImageURL(rawURL) {
		return nil, "", fmt.Errorf("unsupported generated image host")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "image/*")
	client := *outbound.HTTPClient()
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !isDesignerImageURL(next.URL.String()) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Designer image download HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxGeneratedImageBytes {
		return nil, "", fmt.Errorf("generated image exceeds %d bytes", maxGeneratedImageBytes)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		contentType = http.DetectContentType(body)
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return nil, "", fmt.Errorf("Designer returned non-image content")
	}
	return body, contentType, nil
}

// generatedImageURL builds the public link to a hosted image. The base is
// resolved through publicBaseURL so the link stays reachable behind a reverse
// proxy; a relative path is returned only if no host could be determined at
// all, which still works for same-origin callers.
func (s *Server) generatedImageURL(r *http.Request, id string) string {
	path := "/v1/images/files/" + id
	if base := s.publicBaseURL(r); base != "" {
		return base + path
	}
	return path
}

func (s *Server) generatedImageFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/images/files/")
	if _, err := uuid.Parse(id); err != nil {
		http.NotFound(w, r)
		return
	}
	item, err := s.loadGeneratedImage(id)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		if os.IsNotExist(err) {
			http.Error(w, "图片不存在或已清理；旧版本的内存图片无法恢复，请重新生成。", http.StatusNotFound)
		} else {
			http.Error(w, "图片读取失败，请稍后重试。", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", item.ContentType)
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Length", fmt.Sprint(len(item.Data)))
	_, _ = w.Write(item.Data)
}

func isImageQuotaRefusal(text string) bool {
	low := strings.ToLower(strings.TrimSpace(text))
	for _, phrase := range []string{
		"generate any more images",
		"image generation quota",
		"daily image limit",
		"try again tomorrow",
		"无法再生成图片",
		"请明天再试",
	} {
		if strings.Contains(low, phrase) {
			return true
		}
	}
	return false
}

// isImageQuotaError covers both the typed error and the human-readable
// structured metering errors returned by different ChatHub deployments. The
// latter used to be classified as UPSTREAM_STRUCTURED, which caused the same
// exhausted account to be selected repeatedly until the five-minute request
// timeout expired.
func isImageQuotaError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, chathub.ErrImageLimit) {
		return true
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "image generation daily limit") ||
		strings.Contains(low, "image generation quota") ||
		strings.Contains(low, "daily image limit") ||
		strings.Contains(low, "generate any more images")
}

// extractImageURLs finds image URLs in a raw JSON string by searching for URL patterns.
func extractImageURLs(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				lk := strings.ToLower(k)
				if s, ok := e.(string); ok && (lk == "url" || lk == "imageurl" || lk == "thumbnailurl" || lk == "downloadurl" || lk == "src" || lk == "value" || lk == "data") {
					if strings.HasPrefix(s, "https://") && !seen[s] {
						if strings.Contains(strings.ToLower(s), "image") || strings.HasSuffix(strings.ToLower(s), ".png") || strings.HasSuffix(strings.ToLower(s), ".jpg") || strings.HasSuffix(strings.ToLower(s), ".jpeg") || strings.HasSuffix(strings.ToLower(s), ".webp") || strings.HasSuffix(strings.ToLower(s), ".gif") {
							seen[s] = true
							out = append(out, s)
						}
					}
				} else {
					walk(e)
				}
			}
		}
	}
	walk(v)
	return out
}

func downloadImageAsBase64(url string) (b64, contentType string, err error) {
	return downloadImageAsBase64WithToken(url, "")
}

func downloadImageAsBase64WithToken(url, token string) (b64, contentType string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return downloadImageWithContext(ctx, url, token)
}

// imageDownloadClient builds the client used to fetch a remote image that a
// model response pointed at.
//
// A plain http.DefaultClient is wrong here on three counts: it follows up to ten
// redirects with no re-validation, it ignores the configured outbound proxy, and
// it applies no restriction on the target address. The chat UI caches whatever
// this returns and serves it back from /api/chatui/file/, so an unrestricted
// fetch is a read primitive against the local network.
func imageDownloadClient() *http.Client {
	c := outbound.UntrustedHTTPClient(30 * time.Second)
	if outbound.UsingProxy() {
		// The guard lives on the dialer, and a configured proxy owns the dial.
		log.Printf("[image-download] an outbound proxy is configured; downloads are not " +
			"address-restricted because the proxy performs the connection")
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return fmt.Errorf("image download: too many redirects")
		}
		// Re-validate every hop: the first URL passing the check says nothing
		// about where a 302 leads.
		return chathub.ValidateRemoteDownloadURL(req.URL.String())
	}
	return c
}

func downloadImageWithContext(ctx context.Context, url, token string) (b64, contentType string, err error) {
	// Fail early with a clear message; the dial-time guard in
	// outbound.DialControl is what actually makes this unforgeable.
	if err := chathub.ValidateRemoteDownloadURL(url); err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := imageDownloadClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("download returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedImageBytes+1))
	if err != nil {
		return "", "", err
	}
	if len(body) > maxGeneratedImageBytes {
		return "", "", fmt.Errorf("generated image exceeds size limit")
	}
	ct := normalizeImageContentType(resp.Header.Get("Content-Type"), body)
	if ct == "" {
		return "", "", fmt.Errorf("image download: response is not an image (content-type %q)", resp.Header.Get("Content-Type"))
	}
	enc := base64.StdEncoding.EncodeToString(body)
	return enc, ct, nil
}

// normalizeImageContentType returns the content type when it really is an image
// and "" otherwise. The stored type decides the file extension, and only known
// image extensions can be read back out, so accepting a non-image type here
// would file arbitrary bytes under a misleading name.
func normalizeImageContentType(raw string, body []byte) string {
	ct := strings.TrimSpace(raw)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch strings.ToLower(ct) {
	case "", "application/octet-stream", "binary/octet-stream":
		// Some CDNs serve images with no useful type; fall back to sniffing.
		ct = http.DetectContentType(body)
	}
	if !strings.HasPrefix(strings.ToLower(ct), "image/") {
		return ""
	}
	return ct
}

// hostedChatImage is one generated image in both of the forms a caller needs:
// the bytes the gateway downloaded and re-encoded, carried inline as a data URL,
// and the URL of the gateway's own stored copy.
//
// The inline form is what clients are given. A client handed only a URL has to
// fetch the image back from the gateway, which fails whenever the derived public
// address is wrong (a reverse proxy that rewrites Host, a dropped public port) or
// when the client simply cannot reach the NAS address - and the user then sees a
// link instead of a picture. The URL is kept alongside it so the stored copy can
// still be opened, downloaded at full size, or re-sent.
type hostedChatImage struct {
	DataURL string
	URL     string
}

// chatImageBlocks renders generated images for a client: each picture inline,
// followed by a link to the gateway's stored copy.
//
// This is the one place the client-facing shape is decided, so the chat
// completion route and the text-model route cannot drift apart. The other
// client-facing surface, the web chat page, keeps its own transport on purpose:
// it renders from the same origin through /api/chatui/file/<id>, and inlining
// the bytes there would multiply the size of every stored conversation.
func chatImageBlocks(altText string, imgs []hostedChatImage) string {
	var b strings.Builder
	alt := sanitizeImageAlt(altText)
	for _, im := range imgs {
		b.WriteString("![" + alt + "](" + im.DataURL + ")\n")
		b.WriteString("[下载图片](" + im.URL + ")\n\n")
	}
	return b.String()
}

// upstreamImagesToMarkdown delivers images in the standard content field.
// External clients cannot consume chat UI session-protected URLs.
func (s *Server) upstreamImagesToMarkdown(r *http.Request, sources []string, acc auth.AccountToken) string {
	if len(sources) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	imgs, err := s.hostChatImages(ctx, r, sources, acc)
	if err != nil {
		log.Printf("[chat-image-download] failed: %v", err)
		return "\n\n图片下载失败，未能提供可查看的图片。请稍后重试。"
	}
	return "\n\n" + chatImageBlocks("生成图片", imgs)
}

// hostChatImages verifies bytes, stores a copy, and returns each image both
// inline and as a URL.
// Never forward an account token to arbitrary URLs returned by the model.
func (s *Server) hostChatImages(ctx context.Context, r *http.Request, sources []string, acc auth.AccountToken) ([]hostedChatImage, error) {
	var out []hostedChatImage
	seen := map[[32]byte]bool{}
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source = strings.TrimSpace(source)
		var data []byte
		var err error
		if strings.HasPrefix(strings.ToLower(source), "data:image/") {
			meta, payload, ok := strings.Cut(source, ",")
			if !ok || !strings.Contains(strings.ToLower(meta), ";base64") || len(payload) > base64.StdEncoding.EncodedLen(maxGeneratedImageBytes) {
				return nil, fmt.Errorf("invalid inline image")
			}
			data, err = base64.StdEncoding.DecodeString(payload)
		} else {
			if !isDesignerImageURL(source) {
				return nil, fmt.Errorf("unsupported generated image host")
			}
			data, _, err = downloadDesignerImage(ctx, source, acc.AccessToken)
			if err != nil && ctx.Err() == nil && acc.RefreshToken != "" {
				var token string
				token, err = s.designerAccessToken(acc)
				if err == nil {
					data, _, err = downloadDesignerImage(ctx, source, token)
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("image download failed: %w", err)
		}
		ct := http.DetectContentType(data)
		if len(data) == 0 || len(data) > maxGeneratedImageBytes || !strings.HasPrefix(ct, "image/") {
			return nil, fmt.Errorf("upstream returned invalid image bytes")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, ct = stripImageMetadata(data, ct)
		hash := sha256.Sum256(data)
		if seen[hash] {
			continue
		}
		seen[hash] = true
		id, err := s.storeGeneratedImage(data, ct)
		if err != nil {
			return nil, fmt.Errorf("generated image could not be saved: %w", err)
		}
		// Both forms come from the same bytes, so the inline image and the hosted
		// copy can never disagree.
		out = append(out, hostedChatImage{
			DataURL: "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data),
			URL:     s.generatedImageURL(r, id),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("upstream returned no image resource")
	}
	return out, nil
}

// generateChatImages runs the image-generation pipeline for the chat-endpoint
// image router. It supports the full option set (size / count / style / negative
// / quality) and loops once per requested image so callers get exactly `count`
// results. Structured image_options win; the natural-language prompt is parsed as
// a fallback by the caller before reaching here.
func (s *Server) generateChatImages(r *http.Request, userPrompt string, opts imageGenOptions, attachments []chathub.Attachment, accountID string) ([]hostedChatImage, string, error) {
	size, count := opts.normalize()
	opts.Size = size
	prompt := opts.buildPrompt(userPrompt)
	// ImageTimeoutSeconds is one request-level budget shared by account
	// failover, multiple requested images and local hosting. Applying it once per
	// account can turn a five-minute setting into an hour-long stuck request.
	totalTimeout, attemptTimeout := chatImageRouteTimeouts(s.settings.get().ImageTimeoutSeconds)
	totalCtx, totalCancel := context.WithTimeout(r.Context(), totalTimeout)
	defer totalCancel()
	explicit := strings.TrimSpace(accountID) != ""
	var all []hostedChatImage
	var lastConv string
	var lastErr error
	for produced := 0; produced < count; produced++ {
		imgs, convID, err := s.generateOneImage(totalCtx, totalTimeout, attemptTimeout, r, prompt, attachments, explicit, accountID)
		if err != nil {
			lastErr = err
			break
		}
		all = append(all, imgs...)
		lastConv = convID
	}
	// Cap to the requested count: a single upstream call can return more than one
	// image, so without this guard a count=1 request could return several.
	if len(all) > count {
		all = all[:count]
	}
	if len(all) == 0 {
		if lastErr != nil {
			if errors.Is(lastErr, chathub.ErrImageLimit) || IsRateLimited(lastErr) {
				return nil, "", fmt.Errorf("image generation daily limit reached; try again tomorrow")
			}
			return nil, "", lastErr
		}
		return nil, "", fmt.Errorf("no image returned")
	}
	return all, lastConv, nil
}

// generateOneImage runs the upstream GPT Image 2 pipeline once and returns the
// served image URLs for that single generation.
func (s *Server) generateOneImage(totalCtx context.Context, totalTimeout, attemptTimeout time.Duration, r *http.Request, prompt string, attachments []chathub.Attachment, explicit bool, accountID string) ([]hostedChatImage, string, error) {
	var res chathub.Result
	found := false
	var successAcc auth.AccountToken
	var lastErr error
	tried := map[string]bool{}
	for attempt := 0; attempt < maxAccountProbe; attempt++ {
		if err := totalCtx.Err(); err != nil {
			return nil, "", chatImageContextError(err, totalTimeout)
		}
		var acc auth.AccountToken
		var err error
		if attempt == 0 {
			acc, err = s.resolveAccount(accountID)
			if err == nil && !explicit && !s.accountPool.ImageGenAvailable(acc.ID) {
				if next, nerr := s.nextImageGenAccount(acc.ID); nerr == nil {
					acc = next
				}
			}
		} else {
			acc, err = s.nextUntriedImageGenAccount(tried)
		}
		if err != nil {
			if attempt == 0 {
				return nil, "", err
			}
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		tried[acc.ID] = true
		if acc.OID == "" || acc.TID == "" {
			acc.OID, acc.TID = extractOIDTID(acc.AccessToken)
		}
		if acc.OID == "" || acc.TID == "" {
			if attempt == 0 {
				return nil, "", fmt.Errorf("account missing oid/tid — re-login with PKCE")
			}
			continue
		}
		remaining := time.Until(time.Now().Add(totalTimeout))
		if deadline, ok := totalCtx.Deadline(); ok {
			remaining = time.Until(deadline)
		}
		thisAttempt := attemptTimeout
		if remaining < thisAttempt {
			thisAttempt = remaining
		}
		ctx, cancel := context.WithTimeout(totalCtx, thisAttempt)
		res, err = s.chatWithAccount(ctx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{Text: prompt, Tone: "magic", Attachments: attachments, LicenseType: s.settings.get().LicenseType, Scenario: s.settings.get().Scenario, FeatureFlags: s.featureFlags()})
		attemptErr := ctx.Err()
		cancel()
		if err != nil {
			lastErr = err
			if totalCtx.Err() != nil {
				return nil, "", chatImageContextError(totalCtx.Err(), totalTimeout)
			}
			if errors.Is(attemptErr, context.DeadlineExceeded) {
				lastErr = fmt.Errorf("image generation account attempt timed out after %s: %w", thisAttempt, attemptErr)
				if explicit {
					return nil, "", lastErr
				}
				log.Printf("[image-route] account=%s timed out after %s; rotating", acc.ID, thisAttempt)
				continue
			}
			if isImageQuotaError(err) {
				s.accountPool.MarkImageGenTokensThrottled(acc.ID)
			} else if errors.Is(err, chathub.ErrMeteringThrottled) {
				s.accountPool.MarkImageGenSystemThrottled(acc.ID)
			}
			retryable := isImageQuotaError(err) || IsEmptyCompletion(err) || IsRateLimited(err) || upstreamStatus(err) == http.StatusTooManyRequests || IsRetryable(err)
			if explicit || !retryable {
				return nil, "", err
			}
			log.Printf("[image-route] account=%s retryable failure=%s; rotating", acc.ID, ClassifyError(err))
			continue
		}
		if len(res.Images) == 0 {
			if urls := extractImageURLs(res.RawResult); len(urls) > 0 {
				res.Images = urls
			}
		}
		if len(res.Images) == 0 {
			if urls := extractImageURLs(res.Text); len(urls) > 0 {
				res.Images = urls
			}
		}
		if len(res.Images) == 0 && !explicit && isImageQuotaRefusal(strings.Join([]string{res.Text, res.RawResult}, "\n")) {
			s.accountPool.MarkImageGenTokensThrottled(acc.ID)
			continue
		}
		found = true
		successAcc = acc
		break
	}
	if !found {
		if lastErr != nil && (isImageQuotaError(lastErr) || IsRateLimited(lastErr)) {
			return nil, "", fmt.Errorf("image generation daily limit reached; try again tomorrow")
		}
		if lastErr != nil {
			return nil, "", lastErr
		}
		return nil, "", fmt.Errorf("no image returned")
	}
	images := res.Images
	hosted, err := s.hostChatImages(totalCtx, r, images, successAcc)
	if err != nil {
		return nil, "", err
	}
	return hosted, res.ConversationID, nil
}

func chatImageRouteTimeouts(configuredSeconds int) (total, attempt time.Duration) {
	total = time.Duration(configuredSeconds) * time.Second
	if total < 5*time.Second {
		total = 5 * time.Second
	}
	if total > 10*time.Minute {
		total = 10 * time.Minute
	}
	attempt = 3 * time.Minute
	if total < attempt {
		attempt = total
	}
	return total, attempt
}

func chatImageContextError(err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("image generation timed out after %s: %w", timeout, err)
	}
	return fmt.Errorf("image generation canceled: %w", err)
}

// ----------------------------------------------------------------------------
// Per-API-key daily image-generation quota
//
// The chat-endpoint image router (used by OpenAI-compatible clients such as
// WorkBuddy) is not bound to a chatui user account, so its quota is tracked by
// the caller's API key prefix. A daily counter file keyed by date records how
// many images each key has generated today; the ceiling comes from the
// DailyImageLimit runtime setting (0 = unlimited).
// ----------------------------------------------------------------------------

var imageQuotaMu sync.Mutex

func imageQuotaPath(t time.Time) string {
	return filepath.Join(chatDataDir(), "image-quota-"+t.Format("2006-01-02")+".json")
}

func readImageQuota(t time.Time) map[string]int {
	m := map[string]int{}
	if b, err := os.ReadFile(imageQuotaPath(t)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// getAPIKeyImageQuota returns today's used image count and the configured
// (per-day) limit for the given API key prefix.
func getAPIKeyImageQuota(keyPrefix string) (used, limit int) {
	limit = currentSettings().DailyImageLimit
	if limit < 0 {
		limit = 0
	}
	if keyPrefix == "" {
		return 0, limit
	}
	return readImageQuota(time.Now())[keyPrefix], limit
}

// bumpAPIKeyImageQuota increments the daily counter by n and returns the updated
// used/total values. The counter is persisted atomically so concurrent requests
// from the same key stay accurate.
func bumpAPIKeyImageQuota(keyPrefix string, n int) (used, limit int) {
	limit = currentSettings().DailyImageLimit
	if limit < 0 {
		limit = 0
	}
	if keyPrefix == "" {
		return 0, limit
	}
	imageQuotaMu.Lock()
	defer imageQuotaMu.Unlock()
	m := readImageQuota(time.Now())
	m[keyPrefix] += n
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = writeFileAtomic(imageQuotaPath(time.Now()), b, 0600)
	}
	return m[keyPrefix], limit
}

// imageQuotaLine renders the "今日图片已用 X / 总额度 Y" line shown after each
// generation. When the limit is 0 (unlimited) it displays "不限".
func imageQuotaLine(used, limit int) string {
	if limit <= 0 {
		return fmt.Sprintf("📊 今日图片额度：已用 %d / 总额度 不限", used)
	}
	return fmt.Sprintf("📊 今日图片额度：已用 %d / 总额度 %d", used, limit)
}
