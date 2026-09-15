package web

import (
	"encoding/json"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestWorkspaceGroundingDetectsWindowsPath(t *testing.T) {
	got := workspaceGrounding("请修改 D:\\work\\GitHub\\Insurtool\\src\\main.ts 这个文件")
	if !strings.Contains(got, "WORKSPACE GROUNDING") {
		t.Fatalf("expected grounding paragraph, got %q", got)
	}
	if !strings.Contains(got, "D:\\work\\GitHub\\Insurtool") {
		t.Fatalf("expected workspace root in grounding, got %q", got)
	}
}

func TestWorkspaceGroundingNoPath(t *testing.T) {
	if g := workspaceGrounding("写一篇关于月亮的散文"); g != "" {
		t.Fatalf("expected empty grounding for chat without Windows path, got %q", g)
	}
}

func TestWorkspaceRootCollapsesDeepPath(t *testing.T) {
	if got := workspaceRoot("C:\\Users\\pguoy\\proj\\a\\b\\c.ts"); got != "C:\\Users\\pguoy\\proj" {
		t.Fatalf("unexpected root %q", got)
	}
}

func TestToolNamesExtractsFunctionNames(t *testing.T) {
	tools := []chathub.Tool{
		{Function: json.RawMessage(`{"name":"Read","description":"read file"}`)},
		{Function: json.RawMessage(`{"name":"Write","description":"write file"}`)},
	}
	names := toolNames(tools)
	want := map[string]bool{"Read": true, "Write": true}
	if len(names) != 2 || !want[names[0]] || !want[names[1]] {
		t.Fatalf("unexpected tool names %v", names)
	}
}

func TestIsImageGenIntent(t *testing.T) {
	cases := map[string]bool{
		"帮我生成一张产品海报":                    true,
		"画一张猫的图片":                       true,
		"Generate an image of a sunset": true,
		"文生图：一只在月亮上的猫":                  true,
		"帮我修改 main.ts 文件":               false,
		"今天天气怎么样":                       false,
	}
	for text, want := range cases {
		if got := isImageGenIntent(text); got != want {
			t.Fatalf("isImageGenIntent(%q)=%v want %v", text, got, want)
		}
	}
}

func TestShouldRouteChatImageAvoidsFalsePositives(t *testing.T) {
	cases := map[string]bool{
		"帮我生成一张东方仙侠海报":                            true,
		"生成一张极简风格的蓝色圆形图标，纯白背景":                    true,
		"设计一个 logo，直接返回图片":                        true,
		"Generate an image of a sunset":           true,
		"这个接口是否支持生图？":                             false,
		"写一个适合生成海报的生图提示词，不要生成图片":                  false,
		"分析这张图并描述人物服装":                            false,
		"生成 logo 的 SVG 代码":                        false,
		"海报设计需要注意什么？":                             false,
		"How does the image generation API work?": false,
	}
	for text, want := range cases {
		if got := shouldRouteChatImage(text); got != want {
			t.Fatalf("shouldRouteChatImage(%q)=%v want %v", text, got, want)
		}
	}
}

func TestCodingIntentSuppressesImageRoute(t *testing.T) {
	text := "生成 logo 的 SVG 代码"
	if !isImageGenIntent(text) || !codingIntent(text) {
		t.Fatal("test fixture must contain both image and coding intent")
	}
	if shouldRouteChatImage(text) {
		t.Fatal("coding intent must suppress image routing")
	}
	if !codingIntent("帮我修改 main.ts 文件") {
		t.Fatalf("expected coding intent for edit request")
	}
}

func TestLastUserContentAndRole(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "画一只猫"},
		{Role: "assistant", Content: "好的"},
		{Role: "tool", Content: "done"},
	}
	if got := lastUserContent(msgs); got != "画一只猫" {
		t.Fatalf("lastUserContent=%q", got)
	}
	if got := lastMessageRole(msgs); got != "tool" {
		t.Fatalf("lastMessageRole=%q", got)
	}
}
