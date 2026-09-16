package web

// toolPlanningMode is permanently "native" (pass-through to the upstream model,
// exactly like the original author's project) so the model calls Write/Edit
// directly on the client. The "router" variant added a tool-less pre-pass that
// was the root cause of the "online document instead of local file" regression;
// selection via UI/settings/env is intentionally removed and hardcoded here.
func toolPlanningMode(raw string) string {
	return "native"
}
