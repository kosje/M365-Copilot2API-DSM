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
