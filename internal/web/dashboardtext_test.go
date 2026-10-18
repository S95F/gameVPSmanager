package web

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// TestEveryRuleHasDashboardText is the completeness property: a moderator must
// get a plain-language answer for every state a server can be in, so a rule
// added to internal/lifecycle without dashboard text fails here rather than
// showing a signed-in friend a blank or the operator's shell hint.
func TestEveryRuleHasDashboardText(t *testing.T) {
	for _, r := range lifecycle.Rules() {
		txt, ok := dashboardRuleText[r]
		if !ok || strings.TrimSpace(txt) == "" {
			t.Errorf("lifecycle rule %q has no dashboard text", r)
		}
	}
	// And nothing stale: every entry names a rule that still exists.
	live := map[lifecycle.Rule]bool{}
	for _, r := range lifecycle.Rules() {
		live[r] = true
	}
	for r := range dashboardRuleText {
		if !live[r] {
			t.Errorf("dashboard text names rule %q, which lifecycle no longer declares", r)
		}
	}
}

// TestDashboardTextShowsNoShellHints: a dashboard role has no shell on the box,
// so their status text must never tell them to run one.
func TestDashboardTextShowsNoShellHints(t *testing.T) {
	banned := []string{"sudo", "gvm ", "systemctl", "journalctl", "--"}
	for r, txt := range dashboardRuleText {
		low := strings.ToLower(txt)
		for _, b := range banned {
			if strings.Contains(low, b) {
				t.Errorf("dashboard text for %q contains a shell hint (%q): %q", r, b, txt)
			}
		}
	}
	// The fallback obeys the same rule.
	if low := strings.ToLower(dashboardFallback); strings.Contains(low, "sudo") || strings.Contains(low, "gvm ") {
		t.Errorf("the fallback text contains a shell hint: %q", dashboardFallback)
	}
}

func TestDashboardStatusTextFallsBackSafely(t *testing.T) {
	if got := DashboardStatusText(lifecycle.Rule("a-rule-that-does-not-exist")); got != dashboardFallback {
		t.Errorf("DashboardStatusText(unknown) = %q, want the fallback", got)
	}
	if got := DashboardStatusText(lifecycle.RuleReady); got != dashboardRuleText[lifecycle.RuleReady] {
		t.Errorf("DashboardStatusText(ready) = %q, want the mapped text", got)
	}
}
