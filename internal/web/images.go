package web

import (
	"bytes"
	"context"
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
	for i := 0; i < maxAccountProbe; i++ {
		acc, ok := s.tokens.Next()
		if !ok {
			return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
		}
		if avoidID != "" && acc.ID == avoidID {
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
	prevID := ""
	var successAcc auth.AccountToken
	var lastErr error
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
			acc, err = s.nextImageGenAccount(prevID)
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
		prevID = acc.ID
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
			limited := errors.Is(err, chathub.ErrImageLimit) || IsRateLimited(err) || upstreamStatus(err) == http.StatusTooManyRequests
			if explicit || !limited {
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return "", "", err
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

// upstreamImagesToMarkdown 把聊天回复里上游生成的图片转成 markdown 追加到回复文本：
//   - 下载成功：落盘为本地缓存文件，返回 ![生成图片 N](baseURL/api/chatui/file/{id})
//   - 下载失败（设备可能无代理直连图片 CDN）：返回可点击的上游下载链接，由用户自行下载
//
// 与 /v1/images/generations 的兜底语义一致，但以 markdown 形式随聊天流返回。
func (s *Server) upstreamImagesToMarkdown(baseURL string, urls []string, acc auth.AccountToken) string {
	var b strings.Builder
	idx := 0
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		idx++
		// data URI：直接解码落盘
		if strings.HasPrefix(strings.ToLower(u), "data:image/") {
			if _, payload, ok := strings.Cut(u, ","); ok {
				if data, err := base64.StdEncoding.DecodeString(payload); err == nil && len(data) > 0 {
					if id, serr := s.chatUI.saveImage(data, "image/png"); serr == nil {
						b.WriteString(fmt.Sprintf("\n![生成图片 %d](%s/api/chatui/file/%s)", idx, baseURL, id))
						continue
					}
				}
			}
			continue
		}
		// 带上游 token 下载图片字节；失败再用 Designer 专用 token 重试
		b64, ct, err := downloadImageAsBase64WithToken(u, acc.AccessToken)
		if err != nil {
			if dt, dterr := s.designerAccessToken(acc); dterr == nil {
				b64, ct, err = downloadImageAsBase64WithToken(u, dt)
			}
		}
		if err == nil {
			if data, derr := base64.StdEncoding.DecodeString(b64); derr == nil && len(data) > 0 {
				if id, serr := s.chatUI.saveImage(data, ct); serr == nil {
					b.WriteString(fmt.Sprintf("\n![生成图片 %d](%s/api/chatui/file/%s)", idx, baseURL, id))
					continue
				}
			}
		}
		log.Printf("[chat-image-download] err=%v (fallback to upstream URL)", err)
		b.WriteString(fmt.Sprintf("\n⚠️ 图片 %d 已生成，但服务器下载图片失败（设备可能无法直连图片 CDN，需要代理）。请自行点击链接下载：[下载图片 %d](%s)", idx, idx, u))
	}
	return strings.TrimSpace(b.String())
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
	explicit := strings.TrimSpace(accountID) != ""
	var res chathub.Result
	found := false
	prevID := ""
	var successAcc auth.AccountToken
	var lastErr error
	for attempt := 0; attempt < maxAccountProbe; attempt++ {
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
			acc, err = s.nextImageGenAccount(prevID)
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
		prevID = acc.ID
		if acc.OID == "" || acc.TID == "" {
			acc.OID, acc.TID = extractOIDTID(acc.AccessToken)
		}
		if acc.OID == "" || acc.TID == "" {
			if attempt == 0 {
				return nil, "", fmt.Errorf("account missing oid/tid — re-login with PKCE")
			}
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ImageTimeoutSeconds)*time.Second)
		res, err = s.chatWithAccount(ctx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{Text: prompt, Tone: "magic", Attachments: attachments, LicenseType: s.settings.get().LicenseType, Scenario: s.settings.get().Scenario, FeatureFlags: s.featureFlags()})
		cancel()
		if err != nil {
			lastErr = err
			if errors.Is(err, chathub.ErrImageLimit) {
				s.accountPool.MarkImageGenTokensThrottled(acc.ID)
			}
			limited := errors.Is(err, chathub.ErrImageLimit) || IsRateLimited(err) || upstreamStatus(err) == http.StatusTooManyRequests
			if explicit || !limited {
				return nil, "", err
			}
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
	var urls []string
	dlTimeout := time.Duration(s.settings.get().ImageTimeoutSeconds) * time.Second
	if dlTimeout < 90*time.Second {
		dlTimeout = 90 * time.Second
	}
	for _, sourceURL := range images {
		if strings.HasPrefix(strings.ToLower(sourceURL), "data:image/") {
			meta, payload, ok := strings.Cut(sourceURL, ",")
			if !ok || !strings.Contains(strings.ToLower(meta), ";base64") {
				return nil, "", fmt.Errorf("upstream returned an invalid inline image")
			}
			imageData, derr := base64.StdEncoding.DecodeString(payload)
			if derr != nil || len(imageData) == 0 || len(imageData) > maxGeneratedImageBytes {
				return nil, "", fmt.Errorf("upstream returned an invalid inline image")
			}
			contentType := strings.TrimPrefix(strings.SplitN(meta, ";", 2)[0], "data:")
			if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
				contentType = http.DetectContentType(imageData)
			}
			id := s.storeGeneratedImage(imageData, contentType)
			urls = append(urls, generatedImageURL(r, id))
			continue
		}
		if !isDesignerImageURL(sourceURL) {
			urls = append(urls, sourceURL)
			continue
		}
		// The normal M365 access token works for some Designer URLs while the
		// dedicated Designer scope works for others. Try both, matching the
		// proven chat-image download path, so external clients receive a stable
		// local /v1/images/files URL instead of an upstream URL they cannot open.
		b64, contentType, derr := downloadImageAsBase64WithToken(sourceURL, successAcc.AccessToken)
		var imageData []byte
		if derr == nil {
			imageData, derr = base64.StdEncoding.DecodeString(b64)
		}
		if derr != nil || len(imageData) == 0 {
			designerToken, tokenErr := s.designerAccessToken(successAcc)
			if tokenErr == nil {
				dlCtx, dlCancel := context.WithTimeout(r.Context(), dlTimeout)
				imageData, contentType, derr = downloadDesignerImage(dlCtx, sourceURL, designerToken)
				dlCancel()
			} else {
				derr = tokenErr
			}
		}
		if derr != nil || len(imageData) == 0 {
			log.Printf("[image-route-download] err=%v (fallback to upstream URL)", derr)
			urls = append(urls, sourceURL)
			continue
		}
		id := s.storeGeneratedImage(imageData, contentType)
		urls = append(urls, generatedImageURL(r, id))
	}
	if len(urls) == 0 {
		return nil, "", fmt.Errorf("upstream returned no image resource")
	}
	return urls, res.ConversationID, nil
}
