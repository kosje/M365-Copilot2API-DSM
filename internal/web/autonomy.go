package web

// autonomyBoostDirective is prepended to the upstream prompt when AutonomyBoost
// is enabled. The gateway is a stateless proxy: the client (WorkBuddy, Trae,
// Claude Code, …) owns the agent loop and executes tools, so the gateway cannot
// itself "auto-continue" or "judge completion". The correct proxy-side lever is
// to instruct the upstream M365 model to keep driving the client's loop to
// completion — i.e. verify with build/test before stopping. This is the
// realization of auto-continue / completion-judge for OpenAI-compatible clients.
const autonomyBoostDirective = "[Autonomy mode] You are operating as an autonomous coding agent. For any coding task you MUST:\n" +
	"1. Implement ALL required changes, not just a partial first step.\n" +
	"2. VERIFY your work by running the build and the relevant tests yourself (e.g. build, lint, go test, npm test, pytest) — do not assume success.\n" +
	"3. Only finish when the task is fully complete AND verified.\n" +
	"Before ending, explicitly confirm: did the build pass? did tests pass? are all intended files written? If any check failed or was skipped, fix it NOW.\n" +
	"Do NOT end your turn by asking the user to continue, and do NOT finish with a summary of unfinished work. Keep going until the task is genuinely done."

// applyAutonomyBoost prepends the autonomy directive to the flattened prompt
// when the setting is on. It is a no-op otherwise.
func applyAutonomyBoost(prompt string, on bool) string {
	if !on {
		return prompt
	}
	return autonomyBoostDirective + "\n\n" + prompt
}
