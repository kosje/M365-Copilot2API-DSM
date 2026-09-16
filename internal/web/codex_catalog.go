// Codex model catalog compatibility lives here. It is intentionally kept in
// package web because route handlers share unexported request and settings types.
package web

import (
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type modelLimits struct{ ContextWindow, MaxInputTokens, MaxOutputTokens int }
type reasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type modelSpec struct {
	ID, Owner, DisplayName, DefaultReasoningLevel string
	Tools                                         bool
}

type reasoningEffortPreset struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

var advertisedReasoningEfforts = []reasoningEffortPreset{
	{Effort: "none", Description: "Disable additional reasoning."},
	{Effort: "minimal", Description: "Fast responses with minimal reasoning."},
	{Effort: "low", Description: "Fast responses with lighter reasoning."},
	{Effort: "medium", Description: "Balances speed and reasoning depth for everyday tasks."},
	{Effort: "high", Description: "Greater reasoning depth for complex problems."},
	{Effort: "xhigh", Description: "Extra high reasoning depth for complex problems."},
}

// gatewayCodexBaseInstructions is returned only in the Codex model catalog.
// Codex uses it to build its own request instructions; it is not interpreted
// or forwarded directly by the gateway's ChatHub adapter.
const gatewayCodexBaseInstructions = `You are a helpful AI assistant. When asked to write code, always provide the complete implementation — never truncate, abbreviate, or return only a fragment. Write full, working code with all logic included.`

func codexModelMessages() map[string]any {
	return map[string]any{
		"instructions_template": gatewayCodexBaseInstructions,
		"instructions_variables": map[string]string{
			"personality_default":   "",
			"personality_friendly":  "",
			"personality_pragmatic": "",
		},
		"approvals":   nil,
		"auto_review": nil,
	}
}

var gatewayModels = []modelSpec{
	// "auto" is smart routing. With a per-key auto pool configured it resolves
	// to the pool's highest-priority entry (requestAutoModel); without one it
	// falls through modelTone's default to the upstream "magic" tone. Listing
	// it here is what lets ordinary OpenAI clients pick it from their model
	// dropdown — the routing itself already worked, it was just undiscoverable
	// outside the built-in /chat page.
	{ID: "auto", Owner: "microsoft-365", DisplayName: "智能路由", Tools: true},
	{ID: "gpt-5.2", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.2-reasoning", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.3", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.4", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.4-reasoning", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.5", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.5-reasoning", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.6-reasoning", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-image-2", Owner: "microsoft-365", DisplayName: "GPT Image 2"},
	{ID: "claude-sonnet", Owner: "anthropic-via-microsoft-365", Tools: true},
	{ID: "claude-sonnet-reasoning", Owner: "anthropic-via-microsoft-365", Tools: true},
}

func validUpstreamTone(tone string) bool {
	for _, known := range liveUpstreamTones() {
		if tone == known {
			return true
		}
	}
	return false
}

func knownUpstreamTones() []string {
	return []string{
		"Magic", "Gpt_5_2_Auto", "Gpt_5_2_Chat", "Gpt_5_2_Reasoning",
		"Gpt_5_3_Chat", "Gpt_5_3_Reasoning",
		"Gpt_5_4_Chat", "Gpt_5_4_Reasoning",
		"Gpt_5_5_Chat", "Gpt_5_5_Reasoning",
		"Gpt_5_6_Reasoning",
		"Claude_Sonnet", "Claude_Sonnet_Reasoning",
	}
}

var (
	dynamicTones []string
	dynamicMu    sync.RWMutex
	dynamicAt    time.Time
)

func liveUpstreamTones() []string {
	dynamicMu.RLock()
	if dynamicAt.IsZero() || time.Since(dynamicAt) > 24*time.Hour {
		dynamicMu.RUnlock()
		go syncUpstreamTones()
		dynamicMu.RLock()
	}
	t := dynamicTones
	dynamicMu.RUnlock()
	if len(t) > 0 {
		return t
	}
	return knownUpstreamTones()
}

func syncUpstreamTones() {
	tones := fetchUpstreamTones()
	if len(tones) == 0 {
		return
	}
	dynamicMu.Lock()
	dynamicTones = tones
	dynamicAt = time.Now()
	dynamicMu.Unlock()
	log.Printf("synced %d upstream tones from CDN bundle", len(tones))
}

func fetchUpstreamTones() []string {
	client := &http.Client{Timeout: 30 * time.Second}
	pageURL := "https://m365.cloud.microsoft/"
	resp, err := client.Get(pageURL)
	if err != nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	re := regexp.MustCompile(`main\.[a-f0-9]{8}\.js`)
	m := re.FindString(string(body))
	if m == "" {
		return nil
	}
	bundleURL := "https://res.public.onecdn.static.microsoft/midgard/versionless-v2/" + m
	resp2, err := client.Get(bundleURL)
	if err != nil {
		return nil
	}
	bundle, _ := io.ReadAll(io.LimitReader(resp2.Body, 4<<20))
	resp2.Body.Close()
	toneRe := regexp.MustCompile(`(?:Gpt_[0-9]_[0-9]_[A-Za-z_]+|Claude_[A-Za-z0-9_]+|Magic)`)
	matches := toneRe.FindAllString(string(bundle), -1)
	seen := map[string]bool{}
	for _, t := range matches {
		seen[t] = true
	}
	result := make([]string, 0, len(seen))
	for t := range seen {
		result = append(result, t)
	}
	sort.Strings(result)
	return result
}

func configuredModelMapping(model string, mappings []modelMapping) (modelMapping, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, mapping := range mappings {
		if strings.EqualFold(strings.TrimSpace(mapping.PublicModel), model) {
			return mapping, true
		}
	}
	return modelMapping{}, false
}

func configuredModelTone(model string, mappings []modelMapping) (string, bool) {
	mapping, ok := configuredModelMapping(model, mappings)
	if !ok {
		return "", false
	}
	return mapping.UpstreamTone, true
}

func configuredModelSpecs(mappings []modelMapping) []modelSpec {
	models := append([]modelSpec(nil), gatewayModels...)
	for _, mapping := range mappings {
		spec := modelSpec{
			ID: strings.TrimSpace(mapping.PublicModel), Owner: "microsoft-365", Tools: true,
			DisplayName: strings.TrimSpace(mapping.DisplayName), DefaultReasoningLevel: strings.TrimSpace(mapping.DefaultReasoningLevel),
		}
		replaced := false
		for i := range models {
			if strings.EqualFold(models[i].ID, spec.ID) {
				models[i] = spec
				replaced = true
				break
			}
		}
		if !replaced {
			models = append(models, spec)
		}
	}
	return models
}

func positiveEnvInt(name string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err == nil && v > 0 {
		return v
	}
	return fallback
}
func configuredModelLimits() modelLimits {
	cfg := currentSettings()
	contextWindow := cfg.ContextWindow
	maxOutput := cfg.MaxOutputTokens
	if maxOutput >= contextWindow {
		maxOutput = contextWindow / 8
		if maxOutput < 1 {
			maxOutput = 1
		}
	}
	return modelLimits{ContextWindow: contextWindow, MaxInputTokens: contextWindow - maxOutput, MaxOutputTokens: maxOutput}
}
func normalizeReasoningEffort(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	if e == "" {
		return "", nil
	}
	switch e {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return e, nil
	}
	return "", fmt.Errorf("unsupported reasoning effort %q; use none, minimal, low, medium, high, or xhigh", e)
}
func reasoningTone(model, effort string) (string, error) {
	e, err := normalizeReasoningEffort(effort)
	if err != nil {
		return "", err
	}
	// Client did not request an explicit effort: fall back to the global knob,
	// so operators can dial reasoning depth for every model at once.
	if e == "" {
		if cfg := currentSettings(); cfg.ReasoningEffort != "" {
			if se, serr := normalizeReasoningEffort(cfg.ReasoningEffort); serr == nil && se != "" {
				e = se
			}
		}
	}
	if tone, ok := configuredModelTone(model, currentSettings().ModelMappings); ok {
		return tone, nil
	}
	base := modelTone(model)
	// Explicit reasoning aliases are never silently downgraded by a generic client default.
	if strings.Contains(strings.ToLower(model), "reasoning") {
		return base, nil
	}
	if e == "" || e == "none" || e == "minimal" || e == "low" {
		return base, nil
	}
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "claude", "claude-sonnet":
		return "Claude_Sonnet_Reasoning", nil
	case "gpt-5.2":
		return "Gpt_5_2_Reasoning", nil
	case "gpt-5.3":
		return "Gpt_5_3_Reasoning", nil
	case "gpt-5.4":
		return "Gpt_5_4_Reasoning", nil
	case "gpt-5.5":
		return "Gpt_5_5_Reasoning", nil
	case "gpt-5.6":
		return "Gpt_5_6_Reasoning", nil
	default:
		return "Gpt_5_5_Reasoning", nil
	}
}

// catalogEntry builds the OpenAI-compatible model descriptor for a single
// model spec. Capability fields are duplicated at both the top level and under
// "capabilities" because different OpenAI-compatible clients inspect different
// locations.
func catalogEntry(m modelSpec, l modelLimits) map[string]any {
	features := []string{"tools", "function_calling", "streaming", "reasoning", "vision"}
	modalities := []string{"text", "image"}
	caps := map[string]any{
		"chat_completions": true, "responses": true, "streaming": true,
		"tools": true, "reasoning": true,
		"reasoning_efforts": advertisedReasoningEfforts, "supported_reasoning_levels": advertisedReasoningEfforts,
		"reasoning_mode": "gateway_tone_routing", "supports_tools": true, "tool_calls": true,
		"function_calling": true, "supports_function_calling": true, "supports_vision": true,
		"vision": true, "modalities": modalities, "input_modalities": modalities,
		"output_modalities": []string{"text"}, "supported_features": features,
	}
	displayName := m.DisplayName
	if displayName == "" {
		displayName = m.ID
	}
	defaultReasoningLevel := m.DefaultReasoningLevel
	if defaultReasoningLevel == "" {
		defaultReasoningLevel = "medium"
	}
	return map[string]any{
		"id": m.ID, "slug": m.ID, "display_name": displayName, "description": "Public model endpoint.",
		"base_instructions": gatewayCodexBaseInstructions, "model_messages": codexModelMessages(),
		"default_reasoning_level": defaultReasoningLevel, "object": "model", "owned_by": "gateway",
		"shell_type": "shell_command", "visibility": "list", "supported_in_api": true, "priority": 1,
		"additional_speed_tiers": []string{}, "service_tiers": []any{},
		"availability_nux": nil, "upgrade": nil, "include_skills_usage_instructions": false,
		"supports_reasoning_summaries": true, "default_reasoning_summary": "none",
		"support_verbosity": true, "default_verbosity": "low", "apply_patch_tool_type": "freeform",
		"web_search_tool_type": "text_and_image", "truncation_policy": map[string]any{"mode": "tokens", "limit": 10000},
		"supports_parallel_tool_calls": true, "supports_image_detail_original": true,
		"max_context_window": l.ContextWindow, "effective_context_window_percent": 95,
		"experimental_supported_tools": []any{}, "supports_search_tool": true, "use_responses_lite": false,
		"tool_mode": "code_mode_only", "multi_agent_version": "v2",
		"context_window": l.ContextWindow, "max_input_tokens": l.MaxInputTokens, "max_output_tokens": l.MaxOutputTokens,
		"capabilities": caps, "supports_tools": true, "tool_calls": true,
		"supported_reasoning_levels": advertisedReasoningEfforts,
		"function_calling":           true, "supports_function_calling": true, "supports_vision": true,
		"vision": true, "modalities": modalities, "input_modalities": modalities,
		"output_modalities": []string{"text"}, "supported_features": features,
	}
}

// autoSmartModels are advertised as first-class, tool-capable models so that
// OpenAI-compatible clients (WorkBuddy/Trae/…) recognize "auto" / "auto-2" as
// valid models and attach their file tools. Without this, a client configured
// with a custom model name like "auto-2" never sees it in /v1/models and may
// withhold tools, leaving the gateway unable to create or modify files.
var autoSmartModels = []modelSpec{
	{ID: "auto", Owner: "microsoft-365", Tools: true, DisplayName: "智能路由（推荐）", DefaultReasoningLevel: "medium"},
	{ID: "auto-2", Owner: "microsoft-365", Tools: true, DisplayName: "智能路由 2", DefaultReasoningLevel: "medium"},
}

func modelCatalog() []map[string]any {
	l := configuredModelLimits()
	models := configuredModelSpecs(currentSettings().ModelMappings)
	out := make([]map[string]any, 0, len(models)+len(autoSmartModels))
	for _, m := range models {
		out = append(out, catalogEntry(m, l))
	}
	return out
}

// openaiModelsCatalog returns the full catalog including the smart-routing
// "auto"/"auto-2" aliases advertised only on the OpenAI-compatible endpoint.
func openaiModelsCatalog() []map[string]any {
	l := configuredModelLimits()
	out := modelCatalog()
	for _, m := range autoSmartModels {
		out = append(out, catalogEntry(m, l))
	}
	return out
}

// ---- auto routing: random selection from the live model catalog ----
// "auto"/"auto-2"/"intelligent"/… requests are pinned to a random concrete
// model from the current catalog. Unknown model names are rejected outright so
// they can never silently fall back to the conservative "magic" tone.

var (
	autoMu         sync.RWMutex
	autoChatPool   []string
	autoKnownSet   map[string]bool
	autoListAt     time.Time
	autoListTTL    = 5 * time.Minute
	autoRandSource = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// refreshAutoModels rebuilds the cached model lists from the current settings
// and static catalog. Safe for concurrent use.
func refreshAutoModels() {
	specs := configuredModelSpecs(currentSettings().ModelMappings)
	chat := make([]string, 0, len(specs))
	known := make(map[string]bool, len(specs)+len(autoSmartModels))
	for _, s := range specs {
		known[strings.ToLower(s.ID)] = true
		if s.Tools {
			chat = append(chat, s.ID)
		}
	}
	for _, s := range autoSmartModels {
		known[strings.ToLower(s.ID)] = true
	}
	autoMu.Lock()
	autoChatPool = chat
	autoKnownSet = known
	autoListAt = time.Now()
	autoMu.Unlock()
}

// getAutoModels returns the cached chat pool and known-model set, refreshing
// them if the cache is stale or empty.
func getAutoModels() ([]string, map[string]bool) {
	autoMu.RLock()
	stale := autoListAt.IsZero() || time.Since(autoListAt) > autoListTTL || len(autoChatPool) == 0
	autoMu.RUnlock()
	if stale {
		refreshAutoModels()
	}
	autoMu.RLock()
	defer autoMu.RUnlock()
	return autoChatPool, autoKnownSet
}

// isAutoModel reports whether the name is an "auto" / smart-routing alias.
func isAutoModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	switch m {
	case "auto", "auto-1", "auto-2", "auto-3", "automatic", "intelligent", "smart":
		return true
	}
	return strings.HasPrefix(m, "auto-")
}

// pickAutoModelRandom returns a random chat-capable model for an "auto"
// request, constrained to the provided allow-list (per-key whitelist) when
// non-empty. Returns "" when no eligible model exists.
func pickAutoModelRandom(allowed []string) string {
	pool, _ := getAutoModels()
	if len(pool) == 0 {
		return ""
	}
	if len(allowed) > 0 {
		set := make(map[string]bool, len(allowed))
		for _, a := range allowed {
			if v := strings.TrimSpace(a); v != "" {
				set[strings.ToLower(v)] = true
			}
		}
		filtered := make([]string, 0, len(pool))
		for _, m := range pool {
			if set[strings.ToLower(m)] {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) == 0 {
			return ""
		}
		pool = filtered
	}
	return pool[autoRandSource.Intn(len(pool))]
}

// isKnownModel reports whether the gateway accepts this model name: catalog
// models, custom mappings, and the advertised auto aliases. Anything else is
// rejected by the caller instead of being downgraded to "magic".
func isKnownModel(model string) bool {
	_, known := getAutoModels()
	return known[strings.ToLower(strings.TrimSpace(model))]
}

// StartAutoModelsRefresh launches a background ticker that periodically
// re-reads the model catalog so the auto pool stays current.
func (s *Server) StartAutoModelsRefresh() {
	refreshAutoModels()
	go func() {
		ticker := time.NewTicker(autoListTTL)
		defer ticker.Stop()
		for range ticker.C {
			refreshAutoModels()
		}
	}()
}
