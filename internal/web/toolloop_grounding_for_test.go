package web

import (
	"strings"
	"testing"
)

func TestWorkspaceGroundingForWithoutTools(t *testing.T) {
	prompt := "D:\\work\\GitHub\\Insurtool 测试修改文件"
	withTools := workspaceGroundingFor(prompt, true)
	without := workspaceGroundingFor(prompt, false)
	if withTools == "" || without == "" {
		t.Fatalf("grounding empty: with=%q without=%q", withTools, without)
	}
	if !strings.Contains(withTools, "D:\\work\\GitHub\\Insurtool") {
		t.Fatalf("root path missing in withTools: %q", withTools)
	}
	if !strings.Contains(without, "No tools are attached") || strings.Contains(without, "Your tools operate") {
		t.Fatalf("tool-less variant wrong: %q", without)
	}
	if !strings.Contains(withTools, "Your tools operate") {
		t.Fatalf("tools variant wrong: %q", withTools)
	}
	if workspaceGroundingFor("no paths here", true) != "" {
		t.Fatalf("expected empty grounding without paths")
	}
}
