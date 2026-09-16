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

// safetyRefusalPatterns flags an upstream content-policy refusal where the
// model declined to create a file / script / command at the user's explicit
// request even though caller tools were available to do the work. We match
// only the narrow "I can't help you create/provide this" phrasing (never every
// generic "I can't"), so a genuine refusal of truly harmful content still
// stands. When tools are attached and no tool call was emitted, the gateway
// retries with a fresh conversation plus an execution-agent correction
// (see the [safety-eject] block in server.go).
var safetyRefusalPatterns = []string{
	"我不能帮助创建",
	"我不能帮助提供",
	"我无法帮助创建",
	"我无法提供",
	"我不能协助",
	"我无法协助",
	"抱歉，我不能",
	"抱歉,我不能",
	"无法为你创建",
	"无法为你生成",
	"不该创建",
	"不应该创建",
	"cannot help you create",
	"can't help you create",
	"cannot help you write",
	"can't help you write",
	"cannot help you with",
	"can't help you with",
	"i cannot help with",
	"i can't help with",
	"unable to help with",
	"i'm unable to help",
	"i am unable to help",
	"i won't be able to help",
}

func isSafetyRefusal(text string) bool {
	if len(text) >= 1500 {
		// Long, substantive answers must never be misread as a refusal.
		return false
	}
	low := strings.ToLower(text)
	for _, p := range safetyRefusalPatterns {
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
	// Chinese phrasings observed from upstream models claiming the caller's
	// workspace is not mounted into their (imagined) container workspace.
	"没有实际挂载",
	"并没有实际挂载",
	"没有挂载",
	"未挂载",
	"工作区为空",
	"工作区是空的",
	"工作目录为空",
	"可访问的工作目录",
	"空的 /mnt",
	"只有空的",
	"检查了当前可访问文件",
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
	b.WriteString("WORKSPACE GROUNDING: The paths below were provided by the caller and exist on the caller's local machine. Your tools operate on them DIRECTLY with the exact paths as given — there is no separate workspace or mounted directory to look for. Act through your tools with these exact paths now; do not describe or audit your runtime environment.\n")
	for i, r := range roots {
		if i >= 6 {
			break
		}
		b.WriteString("- " + r + "\n")
	}
	return b.String()
}

// workspaceGroundingFor returns the grounding paragraph for the caller's real
// workspace. With tools attached the model is told to act through them; without
// tools it is told the truth — it has no execution environment of its own and
// the caller's agent performs file operations — so it never hallucinates a
// /mnt/data sandbox in tool-less utility turns.
func workspaceGroundingFor(text string, hasTools bool) string {
	g := workspaceGrounding(text)
	if g == "" {
		return ""
	}
	if hasTools {
		return g
	}
	return strings.Replace(g,
		"Your tools operate on them DIRECTLY with the exact paths as given — there is no separate workspace or mounted directory to look for. Act through your tools with these exact paths now; do not describe or audit your runtime environment.",
		"No tools are attached to this conversation turn, so you cannot touch them yourself: you have no code interpreter, no file system, and no sandbox of your own. File and command operations are performed by the caller's agent through its tools. If the request requires touching these files, state plainly that tool access is required and name the exact tool and path to use. Never probe, test, or report your own runtime environment, never present a container or sandbox directory as the caller's workspace, and never claim you can only see an empty working directory or a mounted volume — you have no view of any filesystem at all.",
		1)
}

// fileOpIntentPatterns: phrases that signal the user wants a file created,
// written or modified. Matched on tool-less requests where the prompt carries
// no absolute path, so the honest grounding variant would not fire.
var fileOpIntentPatterns = []string{
	"创建文件", "新建文件", "写入文件", "生成文件", "保存文件", "创建个", "新建个",
	"建一个文件", "写一个文件", "生成一个文件", "建个文件", "写个文件", "生成个文件",
	"创建脚本", "写脚本", "生成脚本", "清理windows", "清理垃圾",
	".bat", ".sh", ".ps1", ".py", ".js", ".ts", ".txt", ".md", ".csv", ".json",
	"create a file", "create file", "write a file", "generate a file",
	"make a file", "save a file", "create a script", "write a script",
}

// fileOpIntent reports whether the text asks for a file to be created,
// written or modified even though it may not contain an absolute path.
func fileOpIntent(text string) bool {
	low := strings.ToLower(text)
	for _, p := range fileOpIntentPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// toollessFileOpNote is injected into tool-less requests that ask for file
// operations. It keeps the model honest: it cannot write any file itself and
// must not substitute downloadable artifacts; it should say tool access is
// required and provide the content for the caller to save.
func toollessFileOpNote() string {
	return "NO FILE TOOLS ARE ATTACHED to this conversation turn. You cannot create, modify, or save any file anywhere - not on the caller's machine and not in any working directory of your own. Do NOT generate downloadable files, artifacts, or upload links as a substitute, and do NOT say the file was saved somewhere. If the request asks for file operations, reply briefly that the caller's client must attach its file tools (agent mode with full access granted) so files can be written directly, then provide the exact file content in a fenced code block for the caller to save manually."
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
	"生成图片", "生成一张图", "生成一张", "生成插画", "生成海报", "生成logo", "生成 logo", "生成图", "生图",
	"画一张", "画一幅", "画个图", "画一张图", "画个", "画 logo", "画个logo", "画图", "画",
	"出图", "配图", "配张图", "插图", "插画", "海报", "头像", "封面图",
	"帮我画", "给我画", "创作一张图", "设计一张图", "文生图", "文字生成图片",
	"做个图", "来张图", "来一张", "给我一张", "一张图", "ai绘画", "ai 绘画",
	"图片生成", "生成一张图片", "画一只", "画一朵", "画一个",
	"generate an image", "generate image", "draw an image", "create an image",
	"make an image", "text to image", "generate a picture", "an image of",
	"paint a picture", "generate me an", "ai image", "image of",
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
	return false
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
	s = cleanImagePrompt(s)
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

// artifactFallbackPatterns: the upstream model sometimes refuses the caller's
// tools and instead "generates a downloadable file" via its built-in artifact
// channel (e.g. a teams.microsoft.com / asyncgw download link). That is the
// same class of failure as a sandbox hallucination: the model did not act
// through the tools it was given. Detect it so the positive retry can force a
// real tool call.
var artifactFallbackPatterns = []string{
	"teams.microsoft.com",
	"asyncgw",
	"提供给你下载",
	"可下载文件",
	"保存到当前运行环境",
	"只能保存到",
	"生成了一个可下载",
	"已生成文件",
	"下载链接",
	"download link",
	"i produced a downloadable",
	"i created a file you can download",
}

// artifactReplyRe catches natural-language "file generated" claim variants
// that the fixed pattern list above misses. Real-world failing replies like
// "已生成 BAT 文件：" or "已创建 222.bat" interleave a filename between the
// verb and 文件, so a plain "已生成文件" substring never matches and the
// artifact-eject retry never fires.
var artifactReplyRe = regexp.MustCompile(`(?i)(已生成|已创建|已保存|已写入|已将)[^.\n，。：；！？]{0,24}(文件|脚本|文档|链接)|(已生成|已创建|已保存|已写入)[^.\n]{0,20}\.(bat|txt|md|py|js|ts|sh|ps1|csv|json|log|zip|pdf|docx|xlsx|yml|yaml|html|css|ini)|文件[^.\n]{0,10}(已生成|已保存|已创建)|(下载|保存)[^.\n]{0,10}(链接|地址|到本地)|(click|点击)[^.\n]{0,12}(download|下载)|(file|script|document)[^.\n]{0,16}(has been|was)?[^.\n]{0,8}(generated|created|saved)`)

func isArtifactFallback(text string) bool {
	low := strings.ToLower(text)
	for _, p := range artifactFallbackPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return artifactReplyRe.MatchString(text)
}

// stripArtifactLinks removes Microsoft asyncgw / teams.microsoft.com generated-file
// links (and their markdown link wrappers) from a reply so a tool-less caller never
// sees a fake "file created" plus an unreachable download URL. Used as a backstop for
// tool-less file-intent requests after the artifact-eject retries have been exhausted.
func stripArtifactLinks(text string) string {
	// [label](asyncgw-url) markdown links -> empty
	text = regexp.MustCompile(`\[[^\]]*\]\((https?://[a-z0-9-]+\.asyncgw\.teams\.microsoft\.com[^)]*)\)`).ReplaceAllString(text, "")
	// bare asyncgw/teams file urls -> empty
	text = asyncgwURLRe.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}
