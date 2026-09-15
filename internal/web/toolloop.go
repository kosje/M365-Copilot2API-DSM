package web

import (
	"encoding/json"
	"fmt"
	"m365-copilot2api/internal/chathub"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

type detectedToolCall struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func toolType(name string, tools []map[string]any) string {
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		if n, _ := f["name"].(string); n == name {
			if typ, _ := t["type"].(string); typ != "" {
				return typ
			}
		}
	}
	return "function"
}

func allowedToolNames(tools []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		if f, ok := t["function"].(map[string]any); ok {
			if n, ok := f["name"].(string); ok && n != "" {
				out[n] = true
			}
		}
	}
	return out
}

type rejectedToolCall struct {
	Name   string
	Reason string
}

// validateDetectedToolCalls is the final trust boundary before a model-selected
// call is serialized to the client. ChatHub/native events and model-generated
// routing text are both untrusted: an undeclared name such as "unknown_tool"
// must never escape to Claude Code, Codex, or another local tool runner.
func validateDetectedToolCalls(calls []detectedToolCall, tools []map[string]any, choice any) ([]detectedToolCall, []rejectedToolCall) {
	valid := make([]detectedToolCall, 0, len(calls))
	rejected := make([]rejectedToolCall, 0)
	for _, call := range calls {
		fn := toolFunction(call.Name, tools)
		if fn == nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "tool was not declared by the client"})
			continue
		}
		if !toolChoiceAllows(choice, call.Name) {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "tool_choice does not allow this tool"})
			continue
		}
		args := map[string]any{}
		if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
			call.Arguments = json.RawMessage(`{}`)
		} else if err := json.Unmarshal(call.Arguments, &args); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "arguments are not a JSON object"})
			continue
		}
		if err := schemaValid(args, fn); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: err.Error()})
			continue
		}
		if call.ID == "" {
			call.ID = callID(call.Name, string(call.Arguments), len(valid))
		}
		if call.Type == "" {
			call.Type = toolType(call.Name, tools)
		}
		valid = append(valid, call)
	}
	return valid, rejected
}

func toolChoiceAllows(choice any, name string) bool {
	if choice == nil {
		return true
	}
	if s, ok := choice.(string); ok {
		return s != "none" && (s != "required" || name != "")
	}
	if m, ok := choice.(map[string]any); ok {
		if f, ok := m["function"].(map[string]any); ok {
			n, _ := f["name"].(string)
			return n == name
		}
		if n, ok := m["name"].(string); ok {
			return n == name
		}
	}
	return true
}

// callID returns a globally unique tool call id. Content hashes previously
// collided when the same tool+arguments was invoked again (duplicate tool call
// id errors from clients), so uniqueness must not depend on call content.
func callID(name, args string, index int) string {
	return "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func extractToolCalls(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	allowed := allowedToolNames(tools)
	var out []detectedToolCall
	remaining := text
	for {
		start := strings.Index(remaining, "<m365-tool-call>")
		if start < 0 {
			break
		}
		end := strings.Index(remaining[start:], "</m365-tool-call>")
		if end < 0 {
			break
		}
		end += start
		content := remaining[start+len("<m365-tool-call>") : end]
		remaining = remaining[end+len("</m365-tool-call>"):]
		var raw any
		if json.Unmarshal([]byte(content), &raw) != nil {
			continue
		}
		items := []any{raw}
		if arr, ok := raw.([]any); ok {
			items = arr
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			n, _ := m["name"].(string)
			if !allowed[n] || !toolChoiceAllows(choice, n) {
				continue
			}
			a, _ := json.Marshal(m["arguments"])
			out = append(out, detectedToolCall{ID: callID(n, string(a), len(out)), Type: toolType(n, tools), Name: n, Arguments: a})
		}
	}
	return out, len(out) > 0
}

func validateToolResult(messages []oaiMsg, known map[string]bool) error {
	for _, m := range messages {
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				return fmt.Errorf("tool_call_id required")
			}
			if len(known) > 0 && !known[m.ToolCallID] {
				return fmt.Errorf("unknown tool_call_id: %s", m.ToolCallID)
			}
		}
	}
	return nil
}

var toolRefusalPatterns = []string{
	"tools are not available",
	"tool is not available",
	"not actually registered",
	"not actually available",
	"not available in this session",
	"工具不可用",
	"工具未暴露",
}

func isToolRefusal(text string) bool {
	if len(text) >= 200 {
		return false
	}
	low := strings.ToLower(text)
	for _, p := range toolRefusalPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func isContentPolicyBlock(text string) bool {
	return chathub.IsContentPolicyBlock(text)
}

func isImageLimitNotice(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "无法生成更多图像") || strings.Contains(t, "unable to generate more images")
}

var sandboxHallucinationPatterns = []string{
	"I can run that for you",
	"I'll run that",
	"let me run that",
	"let me execute",
	"running in sandbox",
	"executing in sandbox",
	"code interpreter",
	"python sandbox",
	"sandbox environment",
	"sandbox 环境",
	"code interpreter 环境",
	"/mnt/data",
	"linux container",
	"linux sandbox",
	"cloud sandbox",
	"云沙箱",
	"云容器",
	"container directory",
	"容器内目录",
	"容器目录",
	"execution environment has changed",
	"执行环境已经切换",
	"cannot access the Windows path",
	"only provides Linux",
	"只提供 Linux 容器",
	"no Windows execution",
	"don't have a Windows",
	"cannot execute on Windows",
	"no execution channel",
	"没有 Windows 执行通道",
	"没有执行通道",
	"cannot run commands on",
	"don't have command execution",
	"无法执行命令",
	"当前运行环境里没有",
	"运行环境只有",
	"执行环境只有",
	"无法直接访问你的",
	"无法访问你的 D:",
	"I don't have SSH access tools",
	"I don't have any tools",
	"none of which can reach",
}

// windowsPathRe matches absolute Windows paths (drive letter + backslash tree).
var windowsPathRe = regexp.MustCompile(`[A-Za-z]:\\[^\s"<>|*?]+`)

// workspaceRoot collapses a Windows path to a stable up-to-3-level root so that
// "D:\work\GitHub\Insurtool\file.ts" becomes "D:\work\GitHub\Insurtool".
func workspaceRoot(p string) string {
	p = strings.TrimRight(p, `\`)
	parts := strings.Split(p, `\`)
	if len(parts) <= 1 {
		return p
	}
	if len(parts) > 4 {
		parts = parts[:4]
	}
	return strings.Join(parts, `\`)
}

// workspaceGrounding scans a flattened prompt for Windows absolute paths the
// caller supplied (e.g. a WorkBuddy workspace like D:\work\GitHub\Insurtool) and
// returns a grounding paragraph asserting those files live on the local machine
// and are directly usable by the model's tools. Returns "" when no path is found.
func workspaceGrounding(text string) string {
	seen := map[string]bool{}
	var roots []string
	for _, m := range windowsPathRe.FindAllString(text, -1) {
		root := workspaceRoot(m)
		if seen[root] {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("WORKSPACE GROUNDING: The paths below were provided by the caller and exist on the caller's local Windows machine. Your Read/Write/Edit/Bash tools operate on them DIRECTLY — there is no separate sandbox, no Linux container, and no /mnt/data. Do NOT claim you cannot access them or can only see container directories. Use the exact paths with your tools.\n")
	for i, r := range roots {
		if i >= 6 {
			break
		}
		b.WriteString("- " + r + "\n")
	}
	return b.String()
}

// toolNames returns the function names declared in the caller's tool list.
func toolNames(tools []chathub.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		var f struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t.Function, &f) == nil && f.Name != "" {
			names = append(names, f.Name)
		}
	}
	return names
}

// imageGenIntentPatterns: phrases that clearly ask the model to generate or
// draw an image. Used to auto-route a chat request to the image pipeline so the
// caller does not need a separate image endpoint.
var imageGenIntentPatterns = []string{
	"生成图片", "生成图像", "生成一张图", "生成一幅图", "生成插画", "生成海报", "生成头像", "生成封面",
	"生成logo", "生成 logo",
	"画一张", "画一幅", "画个图", "画一张图", "画个", "画 logo", "画个logo", "画图",
	"帮我画", "给我画", "请画", "帮我生图", "给我生图", "请生图", "生图：", "生图:", "直接生图", "开始生图",
	"帮我出图", "给我出图", "请出图", "直接出图", "帮我配图", "请配图", "配图：", "配图:", "配张图",
	"创作一张图", "设计一张图", "设计一个logo", "设计一个 logo", "文生图：", "文生图:", "文字生成图片",
	"做个图", "来张图", "ai绘画：", "ai 绘画：",
	"generate an image", "generate image", "draw an image", "create an image",
	"make an image", "text to image", "generate a picture", "draw a picture",
	"paint a picture", "generate me an image", "create me an image", "make me an image",
}

var imageGenActionPatterns = []string{
	"生成", "画", "生图", "出图", "配图", "设计", "制作", "创作", "做个", "做一张", "来张",
}

var imageGenObjectPatterns = []string{
	"图片", "图像", "一张图", "一幅图", "插画", "海报", "头像", "封面", "logo", "壁纸",
}

// imageGenNonActionPatterns identify discussion, analysis, prompt-writing and
// explicit opt-out requests. Nouns such as "海报" or "image" alone are not
// generation intent: routing those requests would unexpectedly spend image
// quota and hijack ordinary questions.
var imageGenNonActionPatterns = []string{
	"不要生成图片", "别生成图片", "无需生成图片", "不用生成图片", "不需要生成图片",
	"不要画图", "别画图", "只写提示词", "生图提示词", "绘图提示词",
	"生图接口", "图片生成接口", "图像生成接口", "生成图片接口", "生图api", "生图 api",
	"是否支持生图", "支不支持生图", "如何生图", "怎么生图", "生图原理",
	"怎么设计海报", "如何设计海报", "设计海报需要注意什么", "海报设计需要注意什么",
	"分析这张图", "分析图片", "描述这张图", "识别图片", "图片里有什么", "看一下这张图",
	"do not generate an image", "don't generate an image", "image generation api", "image generation endpoint",
	"write an image prompt", "image prompt only", "analyze this image", "describe this image",
}

// codingIntentPatterns: phrases that signal the user wants code/editing work
// rather than an image. When present we must NOT auto-route to image gen,
// otherwise a coding request containing the word "图" would be hijacked.
var codingIntentPatterns = []string{
	"修改", "编辑", "改一下", "读取", "读一下", "新建文件", "创建文件", "重构",
	"实现", "函数", "方法", "类 ", "代码", "code", "bug", "编译", "运行", "终端",
	"命令行", "bash", "修复", "调试", "测试", "pytest", "npm ", "go build", "git ",
	"脚本", "部署",
}

// isImageGenIntent reports whether text clearly asks to generate/draw an image.
func isImageGenIntent(text string) bool {
	t := strings.ToLower(text)
	for _, p := range imageGenIntentPatterns {
		if strings.Contains(t, strings.ToLower(p)) {
			return true
		}
	}
	hasAction := false
	for _, p := range imageGenActionPatterns {
		if strings.Contains(t, strings.ToLower(p)) {
			hasAction = true
			break
		}
	}
	if !hasAction {
		return false
	}
	for _, p := range imageGenObjectPatterns {
		if strings.Contains(t, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func shouldRouteChatImage(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" || codingIntent(t) {
		return false
	}
	for _, p := range imageGenNonActionPatterns {
		if strings.Contains(t, strings.ToLower(p)) {
			return false
		}
	}
	if strings.ContainsAny(t, "?？") {
		for _, p := range []string{"是否", "能否", "可否", "支持", "会不会", "可以", "can you", "do you support", "are you able"} {
			if strings.Contains(t, p) {
				return false
			}
		}
	}
	return isImageGenIntent(t)
}

// codingIntent reports whether text is about code/editing work.
func codingIntent(text string) bool {
	t := strings.ToLower(text)
	for _, p := range codingIntentPatterns {
		if strings.Contains(t, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// lastUserContent returns the content of the most recent user-role message.
func lastUserContent(messages []oaiMsg) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return contentToString(messages[i].Content)
		}
	}
	return ""
}

// lastMessageRole returns the role of the final message in the conversation.
func lastMessageRole(messages []oaiMsg) string {
	if len(messages) == 0 {
		return ""
	}
	return messages[len(messages)-1].Role
}

// sanitizeImageAlt makes text safe to embed inside a markdown image alt.
func sanitizeImageAlt(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "]", "")
	s = strings.ReplaceAll(s, "(", "")
	s = strings.ReplaceAll(s, ")", "")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func isSandboxHallucination(text string) bool {
	low := strings.ToLower(text)
	for _, p := range sandboxHallucinationPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
