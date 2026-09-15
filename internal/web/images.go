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
	"strconv"
	"strings"
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
			if errors.Is(err, chathub.ErrImageLimit) {
				// 标记该账号画图配额耗尽（冷却到 UTC 明日零点），
				// 后续画图请求选号时直接跳过它。
				s.accountPool.MarkImageGenTokensThrottled(acc.ID)
			}
			retryable := errors.Is(err, chathub.ErrImageLimit) || IsEmptyCompletion(err) || IsRateLimited(err) || upstreamStatus(err) == http.StatusTooManyRequests || IsRetryable(err)
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
		if errors.Is(lastErr, chathub.ErrImageLimit) || IsRateLimited(lastErr) {
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
		id := s.storeGeneratedImage(imageData, contentType)
		data = append(data, map[string]string{"url": generatedImageURL(r, id)})
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

func (s *Server) storeGeneratedImage(data []byte, contentType string) string {
	id := uuid.NewString()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generatedImages == nil {
		s.generatedImages = map[string]generatedImage{}
	}
	for key, item := range s.generatedImages {
		if now.After(item.ExpiresAt) {
			delete(s.generatedImages, key)
		}
	}
	if len(s.generatedImages) >= maxGeneratedImages {
		var oldestID string
		var oldest time.Time
		for key, item := range s.generatedImages {
			if oldestID == "" || item.ExpiresAt.Before(oldest) {
				oldestID, oldest = key, item.ExpiresAt
			}
		}
		if oldestID != "" {
			delete(s.generatedImages, oldestID)
		}
	}
	s.generatedImages[id] = generatedImage{Data: append([]byte(nil), data...), ContentType: contentType, ExpiresAt: now.Add(generatedImageTTL)}
	return id
}

func generatedImageURL(r *http.Request, id string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/v1/images/files/%s", scheme, r.Host, id)
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
	now := time.Now()
	s.mu.Lock()
	item, ok := s.generatedImages[id]
	if ok && now.After(item.ExpiresAt) {
		delete(s.generatedImages, id)
		ok = false
	}
	if ok {
		item.Data = append([]byte(nil), item.Data...)
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
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

func downloadImageWithContext(ctx context.Context, url, token string) (b64, contentType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
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
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(body)
	}
	enc := base64.StdEncoding.EncodeToString(body)
	return enc, ct, nil
}

func downloadImageAsDataURI(url string) (string, error) {
	b64, ct, err := downloadImageAsBase64(url)
	if err != nil {
		return url, nil
	}
	return "data:" + ct + ";base64," + b64, nil
}

func downloadImageAsDataURIWithToken(url, token string) (string, error) {
	b64, ct, err := downloadImageAsBase64WithToken(url, token)
	if err != nil {
		urlPreview := url
		if len(urlPreview) > 80 {
			urlPreview = urlPreview[:80]
		}
		log.Printf("[image-download] failed url=%s token_len=%d err=%v", urlPreview, len(token), err)
		return url, nil
	}
	urlPreview := url
	if len(urlPreview) > 80 {
		urlPreview = urlPreview[:80]
	}
	log.Printf("[image-download] ok url=%s ct=%s size=%d", urlPreview, ct, len(b64))
	return "data:" + ct + ";base64," + b64, nil
}

// upstreamImagesToMarkdown delivers images in the standard content field.
// External clients cannot consume chat UI session-protected URLs.
func (s *Server) upstreamImagesToMarkdown(r *http.Request, sources []string, acc auth.AccountToken) string {
	if len(sources) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	urls, err := s.hostChatImages(ctx, r, sources, acc)
	if err != nil {
		log.Printf("[chat-image-download] failed: %v", err)
		return "\n\n图片下载失败，未能提供可查看的图片。请稍后重试。"
	}
	var b strings.Builder
	for _, u := range urls {
		b.WriteString("\n\n![生成图片](" + u + ")\n[下载图片](" + u + ")")
	}
	return b.String()
}

// hostChatImages verifies bytes before publishing a public, short-lived URL.
// Never forward an account token to arbitrary URLs returned by the model.
func (s *Server) hostChatImages(ctx context.Context, r *http.Request, sources []string, acc auth.AccountToken) ([]string, error) {
	var urls []string
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
		hash := sha256.Sum256(data)
		if seen[hash] {
			continue
		}
		seen[hash] = true
		urls = append(urls, generatedImageURL(r, s.storeGeneratedImage(data, ct)))
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("upstream returned no image resource")
	}
	return urls, nil
}

// generateChatImages runs the image-generation pipeline (account rotation,
// upstream GPT Image 2 call, download/store) and returns served image URLs
// suitable for inline embedding in a chat completion. Used by the chat-endpoint
// image-intent router so callers do not need a separate image endpoint.
func (s *Server) generateChatImages(r *http.Request, userPrompt string, n int, size string, attachments []chathub.Attachment, accountID string) ([]string, string, error) {
	if n <= 0 {
		n = 1
	}
	if size == "" {
		size = "1024x1024"
	}
	prompt := fmt.Sprintf("Generate an image with GPT Image 2. Size: %s. Description: %s. Return the image URL directly.", size, userPrompt)
	// ImageTimeoutSeconds is a request-level budget, not a per-account budget.
	// Applying it independently inside the failover loop multiplies a 300s
	// setting by every account (12 accounts => up to an hour) while SSE
	// keepalives make the client appear stuck forever.
	totalTimeout, attemptTimeout := chatImageRouteTimeouts(s.settings.get().ImageTimeoutSeconds)
	totalCtx, totalCancel := context.WithTimeout(r.Context(), totalTimeout)
	defer totalCancel()
	explicit := strings.TrimSpace(accountID) != ""
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
			if errors.Is(err, chathub.ErrImageLimit) {
				s.accountPool.MarkImageGenTokensThrottled(acc.ID)
			}
			retryable := errors.Is(err, chathub.ErrImageLimit) || IsEmptyCompletion(err) || IsRateLimited(err) || upstreamStatus(err) == http.StatusTooManyRequests || IsRetryable(err)
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
		if lastErr != nil && (errors.Is(lastErr, chathub.ErrImageLimit) || IsRateLimited(lastErr)) {
			return nil, "", fmt.Errorf("image generation daily limit reached; try again tomorrow")
		}
		if lastErr != nil {
			return nil, "", lastErr
		}
		return nil, "", fmt.Errorf("no image returned")
	}
	images := res.Images
	if len(images) > n {
		images = images[:n]
	}
	urls, err := s.hostChatImages(totalCtx, r, images, successAcc)
	if err != nil {
		return nil, "", err
	}
	return urls, res.ConversationID, nil
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
