package chathub

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolProtocolPrompt follows the community-compatible M365 convention:
// definitions are wrapped in <tools>, and calls are emitted as a fenced block
// whose info string is the exact tool name.
func toolProtocolPrompt(text string, tools []Tool, choice any, hasPlugins bool) string {
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
	names := make([]string, 0, len(defs))
	for _, t := range tools {
		var f struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t.Function, &f) == nil && f.Name != "" {
			names = append(names, f.Name)
		}
	}
	out := fmt.Sprintf("You are an execution agent whose tools run directly on the caller's local machine. The tools below are real, active, and callable right now, and they operate on the caller's real filesystem — Windows paths like D:\\ are used exactly as given. Do not describe, question, or audit your own runtime environment, and do not report what directories you can supposedly see; if the task involves local files or commands, act through these tools instead of talking about them. When you need to run code or commands, use the bash tool rather than writing code blocks for the user to run.\nWhen the user's request requires a tool, call it by emitting one or more fenced blocks. Each block's info string is the exact tool name and its body is a JSON object of arguments. For independent operations, emit multiple blocks in one response. Do not analyze whether tools are registered or available — they are. Do not say a tool is unavailable. Do not wrap the call in XML or Markdown prose. Wait for the tool result before claiming completion.\n\n<tools>\n%s\n</tools>\n\nUser request:\n%s", strings.Join(defs, "\n\n"), text)
	// Long conversations bury the tool protocol at the top of an 80K+ char
	// prompt; the model weights the end of the context most heavily and
	// "forgets" the tools, then claims it cannot access files. Re-anchor the
	// protocol at the recency position so it is the last thing read.
	if len(text) > 6000 {
		out += "\n\nREMINDER (the final instruction before you answer): The <tools> block at the very top of this message defines real tools that run on the caller's local machine right now, including " + strings.Join(names, ", ") + ". If the request involves reading, writing, editing, or running anything on the caller's machine, your next action is to emit a fenced tool block — info string = exact tool name, body = JSON arguments with the exact Windows paths from the request. Never claim you cannot access the caller's files or that you can only see a summary; the tools give you full access. Do not describe your runtime environment; act through the tools."
	}
	return out
}
