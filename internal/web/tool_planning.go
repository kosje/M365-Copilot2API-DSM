package web

import "strings"

// toolPlanningMode resolves the tool-planning strategy. The default is "native"
// (pass-through): tools are handed straight to the upstream model exactly like
// the upstream author's original project, so the model calls Write/Edit
// directly. "router" is an OPT-IN refinement that adds a separate tool-selection
// pre-pass; it must never be the default because the pre-pass is a tool-less
// turn in which the model can decide to emit a downloadable artifact instead of
// calling a local file tool — which is exactly the "online document" regression.
func toolPlanningMode(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "router") {
		return "router"
	}
	return "native"
}
