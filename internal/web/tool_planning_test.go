package web

import "testing"

func TestToolPlanningModeIsPinnedToNative(t *testing.T) {
	for _, raw := range []string{"", "router", "ROUTER", "unexpected", " native "} {
		if got := toolPlanningMode(raw); got != "native" {
			t.Fatalf("toolPlanningMode(%q)=%q, want native", raw, got)
		}
	}
}
