package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"m365-copilot2api/internal/outbound"
)

type modelMapping struct {
	PublicModel           string `json:"publicModel"`
	UpstreamTone          string `json:"upstreamTone"`
	DisplayName           string `json:"displayName"`
	DefaultReasoningLevel string `json:"defaultReasoningLevel"`
}

var defaultModelMappings = []modelMapping{
	{PublicModel: "gpt-5.6-sol", UpstreamTone: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Sol", DefaultReasoningLevel: "low"},
	{PublicModel: "gpt-5.6-terra", UpstreamTone: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Terra", DefaultReasoningLevel: "medium"},
	{PublicModel: "gpt-5.6-luna", UpstreamTone: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Luna", DefaultReasoningLevel: "medium"},
}

var publicModelID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

var configurableCodexModels = []string{
	"gpt-5.2",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.5",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"codex-auto-review",
}

type runtimeSettings struct {
	MaxToolCallsPerTurn        int            `json:"maxToolCallsPerTurn"`
	MaxToolRounds              int            `json:"maxToolRounds"`
	ContextWindow              int            `json:"contextWindow"`
	MaxOutputTokens            int            `json:"maxOutputTokens"`
	ChatTimeoutSeconds         int            `json:"chatTimeoutSeconds"`
	ImageTimeoutSeconds        int            `json:"imageTimeoutSeconds"`
	LogLevel                   string         `json:"logLevel"`
	DebugLogPath               string         `json:"debugLogPath"`
	ListenAddress              string         `json:"listenAddress"`
	ConfigPath                 string         `json:"configPath"`
	TokenCachePath             string         `json:"tokenCachePath"`
	SessionCachePath           string         `json:"sessionCachePath"`
	OutboundProxy              string         `json:"outboundProxy"`
	ProxyPool                  []string       `json:"proxyPool,omitempty"`
	ClientID                   string         `json:"clientId"`
	Authority                  string         `json:"authority"`
	RedirectURI                string         `json:"redirectUri"`
	Scope                      string         `json:"scope"`
	ModelMappings              []modelMapping `json:"modelMappings"`
	ToolPlanningMode           string         `json:"toolPlanningMode"`
	RateLimitCooldownSeconds   int            `json:"rateLimitCooldownSeconds"`
	Scenario                   string         `json:"scenario"`
	MaxConversationMessages    int            `json:"maxConversationMessages"`
	LicenseType                string         `json:"licenseType"`
	AccountConcurrencyLimit    int            `json:"accountConcurrencyLimit"`
	EnableMemoryV2             bool           `json:"enableMemoryV2"`
	EnableDeepWork             bool           `json:"enableDeepWork"`
	EnableComputerUse          bool           `json:"enableComputerUse"`
	EnableRealtimeVoice        bool           `json:"enableRealtimeVoice"`
	EnableSystemPromptOverride bool           `json:"enableSystemPromptOverride"`
	EnableDesignerImageGen4o   bool           `json:"enableDesignerImageGen4o"`
	EnableCodeCanvas           bool           `json:"enableCodeCanvas"`
	EnableSydneyReconnect      bool           `json:"enableSydneyReconnect"`
	QuotaRefreshIntervalSeconds int          `json:"quotaRefreshIntervalSeconds"`
	// SessionTTLMinutes controls how long a session binding stays alive before
	// eviction from sessions.json (and from in-memory lookup).
	SessionTTLMinutes int `json:"sessionTtlMinutes"`
	// ContextTTLMinutes controls how long a resolved conversation context remains
	// eligible for prefix/suffix/fuzzy reuse.
	ContextTTLMinutes int `json:"contextTtlMinutes"`
	// ContextSimilarity is the Jaccard similarity threshold (0-1) used to fuzzy
	// match a request against stored ContextHistory when strict prefix/suffix
	// matching fails.
	ContextSimilarity float64 `json:"contextSimilarity"`
	// PublicIdentityPolicy enables the public-facing identity rewrite policy
	// (neutralising Microsoft-brand references in answers and replacing them
	// with a generic GPT-5-series identity).
	PublicIdentityPolicy bool `json:"publicIdentityPolicy"`
	// ModelAliases maps arbitrary client-requested model names (e.g. "gpt-4o",
	// "claude-sonnet-4") to configured public models. Keys are matched
	// case-insensitively; values must be existing public model IDs.
	ModelAliases map[string]string `json:"modelAliases,omitempty"`
	// Webhook alerts. URL empty = disabled. Type: auto|feishu|telegram|bark|generic.
	AlertWebhookURL         string   `json:"alertWebhookUrl,omitempty"`
	AlertWebhookType        string   `json:"alertWebhookType,omitempty"`
	AlertTelegramChatID     string   `json:"alertTelegramChatId,omitempty"`
	AlertEvents             []string `json:"alertEvents,omitempty"`             // empty = all
	AlertErrorRatePercent   int      `json:"alertErrorRatePercent,omitempty"`   // 0 = disabled, 1-100
	// Predictive token refresh: refresh tokens nearing expiry every N seconds
	// (0 = disabled, default 6h).
	TokenRefreshIntervalSeconds int `json:"tokenRefreshIntervalSeconds,omitempty"`
	// Metrics endpoint token; empty allows loopback scrapers only.
	MetricsToken string `json:"metricsToken,omitempty"`
	// AutoCompact: when the context budget overflows, summarize dropped
	// history via an upstream call instead of silently truncating.
	EnableAutoCompact       bool `json:"enableAutoCompact"`
	AutoCompactMinTokens    int  `json:"autoCompactMinTokens,omitempty"`    // only compact when dropped history >= this many tokens (default 4000)
	// Agent loop detection thresholds: how many identical tool calls / identical
	// failures before the agent ledger flags a stuck loop (hard stop) or a
	// repeated failure (hard stop). Raised from the old hardcoded 2/3 so that
	// long multi-step agent tasks are not aborted prematurely.
	LoopSameLimit   int `json:"loopSameLimit,omitempty"`   // consecutive identical calls (same result) before StuckLoop stop (default 6)
	LoopRepeatLimit int `json:"loopRepeatLimit,omitempty"` // consecutive identical failures before RepeatedFailure stop (default 5)
	// HideReasoning suppresses the reasoning_content / thinking stream from the
	// OpenAI/Anthropic response so clients do not render a tall, fragmented
	// reasoning panel. The upstream call still runs; only the client-visible
	// reasoning is dropped. Default false (reasoning is forwarded as before).
	HideReasoning bool `json:"hideReasoning,omitempty"`
}

type settingsStore struct {
	mu   sync.RWMutex
	path string
	v    runtimeSettings
}

func envInt(name string, fallback int) int {
	n, e := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if e == nil && n > 0 {
		return n
	}
	return fallback
}
func envFloat(name string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	if f, e := strconv.ParseFloat(raw, 64); e == nil && f >= 0 && f <= 1 {
		return f
	}
	return fallback
}
func defaultRuntimeSettings() runtimeSettings {
	return runtimeSettings{
		MaxToolCallsPerTurn: envInt("M365_MAX_TOOL_CALLS_PER_TURN", 32), MaxToolRounds: envInt("M365_MAX_TOOL_ROUNDS", 512),
		ContextWindow: envInt("M365_CONTEXT_WINDOW", 128000), MaxOutputTokens: envInt("M365_MAX_OUTPUT_TOKENS", 16384),
		ChatTimeoutSeconds: envInt("M365_CHAT_TIMEOUT_SECONDS", 120), ImageTimeoutSeconds: envInt("M365_IMAGE_TIMEOUT_SECONDS", 300), LogLevel: firstNonEmptySetting(os.Getenv("M365_LOG_LEVEL"), "info"),
		DebugLogPath: os.Getenv("M365_DEBUG_LOG"), ListenAddress: os.Getenv("M365_LISTEN"), ConfigPath: os.Getenv("M365_CONFIG"),
		TokenCachePath: os.Getenv("M365_TOKEN_CACHE"), SessionCachePath: os.Getenv("M365_SESSION_CACHE"), OutboundProxy: os.Getenv(outbound.EnvProxy), ClientID: os.Getenv("M365_CLIENT_ID"),
		Authority: os.Getenv("M365_AUTHORITY"), RedirectURI: os.Getenv("M365_REDIRECT_URI"), Scope: os.Getenv("M365_SCOPE"),
		ModelMappings:              append([]modelMapping(nil), defaultModelMappings...),
		ToolPlanningMode:           toolPlanningMode(os.Getenv("M365_TOOL_PLANNING_MODE")),
		RateLimitCooldownSeconds:   envInt("M365_RATE_LIMIT_COOLDOWN_SECONDS", 30),
		Scenario:                   firstNonEmptySetting(os.Getenv("M365_SCENARIO"), "OfficeWebIncludedCopilot"),
		MaxConversationMessages:    envInt("M365_MAX_CONVERSATION_MESSAGES", 600),
		LicenseType:                firstNonEmptySetting(os.Getenv("M365_LICENSE_TYPE"), "Starter"),
		AccountConcurrencyLimit:    envInt("M365_ACCOUNT_CONCURRENCY_LIMIT", 8),
		EnableMemoryV2:             os.Getenv("M365_ENABLE_MEMORY_V2") == "true",
		EnableDeepWork:             os.Getenv("M365_ENABLE_DEEP_WORK") == "true",
		EnableComputerUse:          os.Getenv("M365_ENABLE_COMPUTER_USE") == "true",
		EnableRealtimeVoice:        os.Getenv("M365_ENABLE_REALTIME_VOICE") == "true",
		EnableSystemPromptOverride: os.Getenv("M365_ENABLE_SYSTEM_PROMPT_OVERRIDE") == "true",
		EnableDesignerImageGen4o:   os.Getenv("M365_ENABLE_DESIGNER_IMAGE_GEN_4O") == "true",
		EnableCodeCanvas:           os.Getenv("M365_ENABLE_CODE_CANVAS") == "true",
		EnableSydneyReconnect:      os.Getenv("M365_ENABLE_SYDNEY_RECONNECT") == "true",
		QuotaRefreshIntervalSeconds: envInt("M365_QUOTA_REFRESH_INTERVAL_SECONDS", 300),
		SessionTTLMinutes:          envInt("M365_SESSION_TTL_MINUTES", 120),
		ContextTTLMinutes:          envInt("M365_CONTEXT_TTL_MINUTES", 120),
		ContextSimilarity:          envFloat("M365_CONTEXT_SIMILARITY", 0.6),
		PublicIdentityPolicy:       os.Getenv("M365_PUBLIC_IDENTITY_POLICY") == "true",
		ModelAliases:               map[string]string{},
		TokenRefreshIntervalSeconds: envInt("M365_TOKEN_REFRESH_INTERVAL_SECONDS", 21600),
		EnableAutoCompact:          os.Getenv("M365_ENABLE_AUTO_COMPACT") != "false",
		AutoCompactMinTokens:       envInt("M365_AUTO_COMPACT_MIN_TOKENS", 4000),
		LoopSameLimit:              envInt("M365_LOOP_SAME_LIMIT", 6),
		LoopRepeatLimit:            envInt("M365_LOOP_REPEAT_LIMIT", 5),
		HideReasoning:              os.Getenv("M365_HIDE_REASONING") == "true",
	}
}
func settingsPath() string {
	if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "settings.json")
	}
	if p := strings.TrimSpace(os.Getenv("M365_SETTINGS_FILE")); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "m365-copilot2api", "settings.json")
}

var openSettingsStore = sync.OnceValue(func() *settingsStore {
	s := &settingsStore{path: settingsPath(), v: defaultRuntimeSettings()}
	if b, e := os.ReadFile(s.path); e == nil {
		_ = json.Unmarshal(b, &s.v)
	}
	if e := validateSettings(s.v); e != nil {
		log.Printf("[settings] invalid persisted settings: %v", e)
	}
	return s
})

func firstNonEmptySetting(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func validateSettings(v runtimeSettings) error {
	if v.MaxToolCallsPerTurn < 1 || v.MaxToolCallsPerTurn > 64 {
		return fmt.Errorf("每轮工具调用数必须为 1-64")
	}
	if v.MaxToolRounds < 1 || v.MaxToolRounds > 512 {
		return fmt.Errorf("最大工具轮次必须为 1-512")
	}
	if v.ContextWindow < 1024 {
		return fmt.Errorf("上下文窗口不能小于 1024")
	}
	if v.MaxOutputTokens < 1 || v.MaxOutputTokens >= v.ContextWindow {
		return fmt.Errorf("最大输出必须大于 0 且小于上下文窗口")
	}
	if v.ChatTimeoutSeconds < 5 || v.ChatTimeoutSeconds > 3600 {
		return fmt.Errorf("聊天超时必须为 5-3600 秒")
	}
	if v.ImageTimeoutSeconds < 5 || v.ImageTimeoutSeconds > 3600 {
		return fmt.Errorf("图片超时必须为 5-3600 秒")
	}
	if v.LogLevel != "silent" && v.LogLevel != "error" && v.LogLevel != "warn" && v.LogLevel != "info" && v.LogLevel != "debug" {
		return fmt.Errorf("日志等级必须为 silent、error、warn、info 或 debug")
	}
	if err := outbound.ValidateProxyURL(v.OutboundProxy); err != nil {
		return err
	}
	for _, proxyURL := range v.ProxyPool {
		if err := outbound.ValidateProxyURL(strings.TrimSpace(proxyURL)); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(v.ModelMappings))
	for _, mapping := range v.ModelMappings {
		model := strings.TrimSpace(mapping.PublicModel)
		if !publicModelID.MatchString(model) {
			return fmt.Errorf("公开模型 ID 只能包含字母、数字、点、下划线或连字符，且长度为 1-128")
		}
		key := strings.ToLower(model)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("公开模型 ID %q 重复", model)
		}
		seen[key] = struct{}{}
		if !validUpstreamTone(strings.TrimSpace(mapping.UpstreamTone)) {
			return fmt.Errorf("上游 tone %q 不受支持", mapping.UpstreamTone)
		}
		if strings.TrimSpace(mapping.DisplayName) == "" {
			return fmt.Errorf("公开模型 %q 缺少显示名称", model)
		}
		if _, err := normalizeReasoningEffort(mapping.DefaultReasoningLevel); err != nil || strings.TrimSpace(mapping.DefaultReasoningLevel) == "" {
			return fmt.Errorf("公开模型 %q 的默认推理级别无效", model)
		}
	}
	if v.RateLimitCooldownSeconds < 5 || v.RateLimitCooldownSeconds > 3600 {
		return fmt.Errorf("限流冷却时间必须为 5-3600 秒")
	}
	if v.MaxConversationMessages < 1 || v.MaxConversationMessages > 10000 {
		return fmt.Errorf("对话消息上限必须为 1-10000")
	}
	validLicenses := map[string]bool{"Starter": true, "Premium": true, "Free": true, "BCAIS": true, "BCSWW": true, "BCWAF": true, "BCWBF": true}
	if !validLicenses[v.LicenseType] {
		return fmt.Errorf("licenseType 必须为 Starter、Premium、Free、BCAIS、BCSWW、BCWAF 或 BCWBF")
	}
	validScenarios := map[string]bool{"OfficeWebIncludedCopilot": true, "Bizchat": true, "CopilotConsumer": true, "Chathub": true}
	if !validScenarios[v.Scenario] {
		return fmt.Errorf("scenario 必须为 OfficeWebIncludedCopilot、Bizchat、CopilotConsumer 或 Chathub")
	}
	if v.AccountConcurrencyLimit < 1 || v.AccountConcurrencyLimit > 64 {
		return fmt.Errorf("账号并发上限必须为 1-64")
	}
	// 0 disables the periodic quota refresh; otherwise 30-86400 seconds.
	if v.QuotaRefreshIntervalSeconds != 0 && (v.QuotaRefreshIntervalSeconds < 30 || v.QuotaRefreshIntervalSeconds > 86400) {
		return fmt.Errorf("额度刷新间隔必须为 0（关闭）或 30-86400 秒")
	}
	if v.AlertErrorRatePercent < 0 || v.AlertErrorRatePercent > 100 {
		return fmt.Errorf("错误率告警阈值必须为 0（关闭）或 1-100")
	}
	switch v.AlertWebhookType {
	case "", "auto", "feishu", "telegram", "bark", "generic":
	default:
		return fmt.Errorf("告警 webhook 类型必须为 auto、feishu、telegram、bark 或 generic")
	}
	if v.TokenRefreshIntervalSeconds != 0 && (v.TokenRefreshIntervalSeconds < 60 || v.TokenRefreshIntervalSeconds > 86400) {
		return fmt.Errorf("Token 主动刷新间隔必须为 0（关闭）或 60-86400 秒")
	}
	publicModels := map[string]bool{}
	for _, m := range v.ModelMappings {
		publicModels[strings.ToLower(m.PublicModel)] = true
	}
	if len(v.ModelAliases) > 32 {
		return fmt.Errorf("模型别名数量不能超过 32")
	}
	for alias, target := range v.ModelAliases {
		if !publicModelID.MatchString(alias) {
			return fmt.Errorf("模型别名 %q 无效", alias)
		}
		if !publicModels[strings.ToLower(strings.TrimSpace(target))] {
			return fmt.Errorf("模型别名 %q 的目标 %q 不是已配置的公开模型", alias, target)
		}
	}
	if v.AutoCompactMinTokens < 0 || v.AutoCompactMinTokens > 100000 {
		return fmt.Errorf("自动压缩最小 token 数必须为 0-100000")
	}
	if v.LoopSameLimit != 0 && (v.LoopSameLimit < 1 || v.LoopSameLimit > 64) {
		return fmt.Errorf("同一调用循环上限(loopSameLimit)必须为 1-64")
	}
	if v.LoopRepeatLimit != 0 && (v.LoopRepeatLimit < 1 || v.LoopRepeatLimit > 64) {
		return fmt.Errorf("同一失败循环上限(loopRepeatLimit)必须为 1-64")
	}
	if v.SessionTTLMinutes < 1 || v.SessionTTLMinutes > 10080 {
		return fmt.Errorf("会话绑定 TTL(sessionTtlMinutes)必须为 1-10080 分钟")
	}
	if v.ContextTTLMinutes < 1 || v.ContextTTLMinutes > 10080 {
		return fmt.Errorf("上下文复用 TTL(contextTtlMinutes)必须为 1-10080 分钟")
	}
	if v.ContextSimilarity < 0 || v.ContextSimilarity > 1 {
		return fmt.Errorf("上下文相似度阈值(contextSimilarity)必须为 0-1")
	}
	if strings.TrimSpace(v.Scenario) == "" {
		return fmt.Errorf("场景标识不能为空")
	}
	return nil
}
func (s *settingsStore) get() runtimeSettings { s.mu.RLock(); defer s.mu.RUnlock(); return s.v }
func (s *settingsStore) save(v runtimeSettings) error {
	if e := validateSettings(v); e != nil {
		return e
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	if e := os.MkdirAll(filepath.Dir(s.path), 0700); e != nil {
		return e
	}
	if e := writeFileAtomic(s.path, b, 0600); e != nil {
		return e
	}
	s.mu.Lock()
	s.v = v
	s.mu.Unlock()
	return nil
}
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"settings": s.settings.get(), "codexModels": configurableCodexModels, "upstreamTones": knownUpstreamTones(), "restartRequiredFields": []string{"listenAddress", "configPath", "tokenCachePath", "sessionCachePath", "outboundProxy", "proxyPool", "clientId", "authority", "redirectUri", "scope", "debugLogPath"}})
	case http.MethodPut:
		// 前端可能只修改一个字段（如监听地址），其余字段以零值提交。
		// 逐字段合并到当前设置再校验，避免"改一个字段弄丢其他配置"。
		cur := s.settings.get()
		base, _ := json.Marshal(cur)
		var merged map[string]any
		if json.Unmarshal(base, &merged) != nil {
			writeOpenAIError(w, 500, "internal_error", "marshal settings")
			return
		}
		var patch map[string]any
		if json.NewDecoder(r.Body).Decode(&patch) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		for k, v := range patch {
			merged[k] = v
		}
		mergedJSON, _ := json.Marshal(merged)
		var v runtimeSettings
		if json.Unmarshal(mergedJSON, &v) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		if e := s.settings.save(v); e != nil {
			writeOpenAIError(w, 400, "invalid_request_error", e.Error())
			return
		}
		auditLog(r, "settings_update", "")
		if e := outbound.ConfigurePool(v.ProxyPool); e != nil {
			writeOpenAIError(w, 400, "invalid_request_error", e.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true, "settings": v})
	default:
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
	}
}
func configuredToolCallLimit(s *settingsStore) int {
	if raw, ok := os.LookupEnv("M365_MAX_TOOL_CALLS_PER_TURN"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n >= 1 && n <= 64 {
			return n
		}
		return 1
	}
	return s.get().MaxToolCallsPerTurn
}

// adaptiveToolCallLimit permits parallel calls only when every call is a
// read-only, independently addressable operation. Any write, execution,
// mutation, or ambiguous tool is serialized conservatively.
func adaptiveToolCallLimit(c []detectedToolCall, configured int) int {
	if len(c) < 2 || configured < 2 {
		return 1
	}
	for _, call := range c {
		name := strings.ToLower(strings.TrimSpace(call.Name))
		if name == "" || toolLooksMutating(name) || !toolLooksReadOnly(name) {
			return 1
		}
	}
	return configured
}

func toolLooksMutating(name string) bool {
	for _, word := range []string{"exec", "shell", "command", "write", "edit", "update", "delete", "remove", "move", "rename", "create", "patch", "apply", "install", "run"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func toolLooksReadOnly(name string) bool {
	for _, word := range []string{"read", "list", "search", "find", "get", "fetch", "browser", "lookup", "inspect", "stat", "status", "describe", "info"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func limitToolCalls(c []detectedToolCall, n int) []detectedToolCall {
	if n < 1 {
		n = 1
	}
	if len(c) > n {
		return c[:n]
	}
	return c
}

func currentSettings() runtimeSettings { return openSettingsStore().get() }

// currentSettingsSafe returns the persisted settings without panicking when a
// test-constructed Server has no settings store.
func currentSettingsSafe() runtimeSettings { return openSettingsStore().get() }

func sessionTTLMinutes() int {
	if raw, ok := os.LookupEnv("M365_SESSION_TTL_MINUTES"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 && n <= 10080 {
			return n
		}
		return 120
	}
	if n := currentSettings().SessionTTLMinutes; n > 0 && n <= 10080 {
		return n
	}
	return 120
}
func contextTTLMinutes() int {
	if raw, ok := os.LookupEnv("M365_CONTEXT_TTL_MINUTES"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 && n <= 10080 {
			return n
		}
		return 120
	}
	if n := currentSettings().ContextTTLMinutes; n > 0 && n <= 10080 {
		return n
	}
	return 120
}
func contextSimilarityThreshold() float64 {
	if raw, ok := os.LookupEnv("M365_CONTEXT_SIMILARITY"); ok {
		if f, e := strconv.ParseFloat(strings.TrimSpace(raw), 64); e == nil && f >= 0 && f <= 1 {
			return f
		}
		return 0.6
	}
	if f := currentSettings().ContextSimilarity; f >= 0 && f <= 1 {
		return f
	}
	return 0.6
}
func accountDefaultConcurrency() int {
	if raw, ok := os.LookupEnv("M365_ACCOUNT_DEFAULT_CONCURRENCY"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 && n <= 64 {
			return n
		}
		return 8
	}
	// Compatibility: the older env name M365_ACCOUNT_CONCURRENCY_LIMIT is
	// exposed in settings.json as accountConcurrencyLimit.
	if raw, ok := os.LookupEnv("M365_ACCOUNT_CONCURRENCY_LIMIT"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 && n <= 64 {
			return n
		}
		return 8
	}
	if n := currentSettings().AccountConcurrencyLimit; n > 0 && n <= 64 {
		return n
	}
	return 8
}

// resolveModelAlias maps a client-requested model name through the configured
// alias table (case-insensitive). Unmatched models are returned unchanged, so
// existing behaviour for known public models and legacy tones is preserved.
// Nil-safe: a Server without a settings store resolves nothing.
func (s *settingsStore) resolveModelAlias(model string) string {
	if s == nil || model == "" {
		return model
	}
	cfg := s.get()
	if len(cfg.ModelAliases) == 0 {
		return model
	}
	if target, ok := cfg.ModelAliases[strings.ToLower(strings.TrimSpace(model))]; ok {
		return strings.TrimSpace(target)
	}
	return model
}

// ApplyStartupSettingsEnv loads persisted restart-required fields before the
// rest of the application initializes. Explicit process environment variables
// always win over values saved from the web console.
func ApplyStartupSettingsEnv() {
	s := openSettingsStore().get()
	values := map[string]string{"M365_LISTEN": s.ListenAddress, "M365_CONFIG": s.ConfigPath, "M365_TOKEN_CACHE": s.TokenCachePath, "M365_SESSION_CACHE": s.SessionCachePath, outbound.EnvProxy: s.OutboundProxy, "M365_PROXY_POOL": strings.Join(s.ProxyPool, "\n"), "M365_CLIENT_ID": s.ClientID, "M365_AUTHORITY": s.Authority, "M365_REDIRECT_URI": s.RedirectURI, "M365_SCOPE": s.Scope, "M365_DEBUG_LOG": s.DebugLogPath}
	for k, v := range values {
		if _, exists := os.LookupEnv(k); !exists && strings.TrimSpace(v) != "" {
			_ = os.Setenv(k, v)
		}
	}
}
