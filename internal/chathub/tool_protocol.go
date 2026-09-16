package chathub

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolProtocolPrompt follows the community-compatible M365 convention:
// definitions are wrapped in <tools>, and calls are emitted as a fenced block
// whose info string is the exact tool name.
//
// NOTE: the default prompt MUST stay concise and MUST NOT mention
// "downloadable file", "upload link", "artifact", "server", or "host files".
// Naming those concepts anchors them in the model's context and provably makes
// the upstream M365 model fall back to emitting an asyncgw/teams.microsoft.com
// download link ("online file") instead of calling the caller's local Write/Edit
// tool — the exact failure the stock project never had. The stock author's
// prompt only asserts "you are an execution agent on the caller's Windows
// machine; the tools are real and callable right now", and that is what makes
// local file creation reliable.
//
// template / globalPrompt are optional operator overrides (settings):
//   - template non-empty  -> replaces the built-in tool-state instruction for
//     tool-bearing requests; it must contain {tools} and {request} ({text} alias).
//   - globalPrompt non-empty -> prepended to every request (tool or not).
func toolProtocolPrompt(text string, tools []Tool, choice any, hasPlugins bool, template, globalPrompt string) string {
	builtin := toolProtocolPromptDefault(text, tools, choice, hasPlugins)
	hasTools := len(tools) > 0 && !strings.EqualFold(fmt.Sprint(choice), "none")
	if hasTools && !hasPlugins && strings.TrimSpace(template) != "" {
		builtin = strings.NewReplacer(
			"{tools}", toolDefsBlock(tools),
			"{request}", text,
			"{text}", text,
			"{user_request}", text,
		).Replace(template)
	}
	if strings.TrimSpace(globalPrompt) != "" {
		builtin = strings.TrimSpace(globalPrompt) + "\n\n" + builtin
	}
	return builtin
}

// toolProtocolPromptDefault is the stock built-in tool-state prompt. Keep it
// concise and free of "downloadable file / artifact / server" priming language.
func toolProtocolPromptDefault(text string, tools []Tool, choice any, hasPlugins bool) string {
	if len(tools) == 0 || strings.EqualFold(fmt.Sprint(choice), "none") {
		if hasPlugins {
			return text
		}
		return fmt.Sprintf("Please answer the following request in full. Do not truncate or abbreviate your response.\n\n%s", text)
	}
	if hasPlugins {
		return text
	}
	var defs []string
	for _, t := range tools {
		var f struct {
			Name, Description string
			Parameters        json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		params := strings.TrimSpace(string(f.Parameters))
		if params == "" || params == "null" {
			params = "{}"
		}
		defs = append(defs, fmt.Sprintf("%s — %s\n```%s\n%s\n```", f.Name, f.Description, f.Name, params))
	}
	if len(defs) == 0 {
		return text
	}
	return fmt.Sprintf("You are an execution agent on the caller's Windows machine. The tools below are real, active, and callable right now. The bash tool runs Windows PowerShell 5.1; Windows paths like D:\\ are directly accessible. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT emit backtick-backtick-backtick-python or backtick-backtick-backtick-code blocks for execution — if you need to run code, use the bash tool. Do NOT mention Linux containers, /mnt/data, cloud sandboxes, or claim the execution environment has changed.\nWhen the user's request requires a tool, call it by emitting one or more fenced blocks. Each block's info string is the exact tool name and its body is a JSON object of arguments. For independent operations, emit multiple blocks in one response. Do not analyze whether tools are registered or available — they are. Do not say a tool is unavailable. Do not wrap the call in XML or Markdown prose. Wait for the tool result before claiming completion.\n\n<tools>\n%s\n</tools>\n\nUser request:\n%s", strings.Join(defs, "\n\n"), text)
}

// ToolProtocolPromptDefault is the human-readable form of the built-in
// tool-state prompt. It mirrors toolProtocolPromptDefault (line ~74) but uses
// the {tools}/{request} template placeholders so an operator can preview or
// copy the exact built-in instruction from the admin UI. The runtime behaves
// identically whether this template is set as the override or left empty
// (empty = use the built-in default).
const ToolProtocolPromptDefault = "You are an execution agent on the caller's Windows machine. The tools below are real, active, and callable right now. The bash tool runs Windows PowerShell 5.1; Windows paths like D:\\ are directly accessible. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT emit backtick-backtick-backtick-python or backtick-backtick-backtick-code blocks for execution — if you need to run code, use the bash tool. Do NOT mention Linux containers, /mnt/data, cloud sandboxes, or claim the execution environment has changed.\nWhen the user's request requires a tool, call it by emitting one or more fenced blocks. Each block's info string is the exact tool name and its body is a JSON object of arguments. For independent operations, emit multiple blocks in one response. Do not analyze whether tools are registered or available — they are. Do not say a tool is unavailable. Do not wrap the call in XML or Markdown prose. Wait for the tool result before claiming completion.\n\n<tools>\n{tools}\n</tools>\n\nUser request:\n{request}"

// SystemPromptDefault is the built-in global system prompt. The stock
// gateway has never injected a global system prompt — local file editing
// reliability comes entirely from the tool-state prompt above. Empty = use the
// stock behaviour (no global system prompt). Operators may override it in
// settings; clearing the field restores the stock default.
const SystemPromptDefault = ""

// toolDefsBlock renders the <tools>…</tools> fenced definitions block from the
// tool list. Exposed so an operator-supplied template can embed it via {tools}.
func toolDefsBlock(tools []Tool) string {
	var defs []string
	for _, t := range tools {
		var f struct {
			Name, Description string
			Parameters        json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		params := strings.TrimSpace(string(f.Parameters))
		if params == "" || params == "null" {
			params = "{}"
		}
		defs = append(defs, fmt.Sprintf("%s — %s\n```%s\n%s\n```", f.Name, f.Description, f.Name, params))
	}
	if len(defs) == 0 {
		return ""
	}
	return "<tools>\n" + strings.Join(defs, "\n\n") + "\n</tools>"
}
