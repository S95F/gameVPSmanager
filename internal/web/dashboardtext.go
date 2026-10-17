package web

import "github.com/S95F/gameVPSmanager/internal/lifecycle"

// Plain-language status for dashboard roles (§failure-recovery-13).
//
// A verdict from internal/lifecycle carries an operator-facing Hint, and that
// hint is usually a shell command — `sudo gvm apply`, `journalctl -u …`. A
// moderator signed in to the dashboard has no shell on the box, so showing them
// that hint is at best noise and at worst a dead end they cannot follow. This
// table maps every lifecycle rule to a sentence written for them: what is true,
// and — when they cannot fix it themselves — that "the server operator" (the
// person with SSH) is who can. TestEveryRuleHasDashboardText keeps it complete
// as the rule set grows, and TestDashboardTextShowsNoShellHints keeps a shell
// command from ever creeping into it.
var dashboardRuleText = map[lifecycle.Rule]string{
	lifecycle.RuleNoSpec:         "This server isn't set up on this host yet. The server operator needs to create it.",
	lifecycle.RulePinMismatch:    "This server's settings changed but haven't been re-applied yet. The server operator needs to apply them.",
	lifecycle.RuleSpecUnreadable: "This server's definition can't be read. The server operator needs to fix it.",
	lifecycle.RuleLeaseStale:     "A background job stopped without finishing and left the server locked. The server operator needs to clear it.",
	lifecycle.RuleLeaseUnknown:   "It isn't clear whether a background job is still running. The server operator needs to check.",
	lifecycle.RuleLeaseInstall:   "The game files are being installed. This can take a while.",
	lifecycle.RuleLeaseUpdate:    "An update is running. This can take a while.",
	lifecycle.RuleBlockedContent: "Some mods have to be downloaded by hand before this server can start. The server operator needs to fetch them.",
	lifecycle.RuleNeverApplied:   "This server is defined but hasn't been set up yet. The server operator needs to apply it.",
	lifecycle.RulePinMissing:     "This server's applied-settings record is missing. The server operator needs to re-apply it.",
	lifecycle.RuleForeignDropIn:  "This server's startup was changed outside gvm. The server operator needs to review it.",
	lifecycle.RuleUnitUnknown:    "The system doesn't know this server's service yet. The server operator needs to apply it.",
	lifecycle.RuleUnitMasked:     "This server is masked and can't start. The server operator needs to unmask it.",
	lifecycle.RuleUnitNotFound:   "This server's service is missing. The server operator needs to apply it.",
	lifecycle.RuleActivating:     "Starting…",
	lifecycle.RuleCrashLooping:   "This server keeps crashing as it starts. The server operator needs to check the logs.",
	lifecycle.RuleDeactivating:   "Stopping…",
	lifecycle.RuleUnitFailed:     "This server failed to run. The server operator needs to check the logs.",
	lifecycle.RuleNoStatus:       "The server is up but isn't reporting its status yet.",
	lifecycle.RuleStatusStale:    "The server's status is stale; it may be busy or stuck. The server operator may need to look.",
	lifecycle.RuleShuttingDown:   "Stopping and saving the world…",
	lifecycle.RuleBooting:        "Starting up and loading the world…",
	lifecycle.RuleRunnerFailed:   "This server didn't start. The server operator needs to check the logs.",
	lifecycle.RuleRunnerExited:   "This server exited on its own. The server operator needs to check the logs.",
	lifecycle.RuleReady:          "Running.",
	lifecycle.RuleNotInstalled:   "This server's game files aren't installed yet. The server operator needs to install them.",
	lifecycle.RuleHeld:           "Stopped by an operator and held down until the next restart.",
	lifecycle.RuleUncleanExit:    "This server stopped unexpectedly last time. The server operator may want to check the logs.",
	lifecycle.RuleDisabled:       "Turned off. It will stay off until an operator turns it back on.",
	lifecycle.RuleInactive:       "Stopped.",
	lifecycle.RuleUnmodelled:     "This server is in a state the dashboard doesn't recognise. The server operator needs to look at it.",
}

// dashboardFallback is shown for a rule with no entry. The completeness test
// makes this unreachable in practice; it exists so a future rule added without
// its text degrades to a safe sentence rather than a blank.
const dashboardFallback = "Something on this server needs attention. The server operator can look into it."

// DashboardStatusText returns the moderator-facing sentence for a lifecycle
// rule, never the operator's shell hint.
func DashboardStatusText(r lifecycle.Rule) string {
	if t, ok := dashboardRuleText[r]; ok {
		return t
	}
	return dashboardFallback
}
