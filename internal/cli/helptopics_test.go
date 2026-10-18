package cli

import (
	"strings"
	"testing"

	helptopics "github.com/S95F/gameVPSmanager/docs/help"
)

// TestHelpTopicsPrintOffline: every embedded topic prints with `gvm help
// <topic>`, exits 0, and touches no host — the whole point is prose an
// operator reads on the box with nothing running. The DoD names exit-codes,
// concepts and faq, which are covered by the walk over all topics.
func TestHelpTopicsPrintOffline(t *testing.T) {
	names := helptopics.Names()
	if len(names) < 4 {
		t.Fatalf("only %d topics embedded; expected concepts, exit-codes, faq, …", len(names))
	}
	for _, want := range []string{"concepts", "exit-codes", "faq"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the %q topic the DoD names is missing", want)
		}
	}
	for _, topic := range names {
		// `firewall` is also a command, so `gvm help firewall` shows the
		// command, not the topic; read the topic directly for that one.
		if topic == "firewall" {
			if text, ok := helptopics.Topic(topic); !ok || strings.TrimSpace(text) == "" {
				t.Errorf("topic %q is empty", topic)
			}
			continue
		}
		h := newTestHost(t)
		code, out, errb := runWith(t, h, "", "help", topic)
		if code != 0 {
			t.Errorf("`gvm help %s` exited %d:\n%s", topic, code, out+errb)
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("`gvm help %s` printed nothing", topic)
		}
		if n := len(h.FS.writes); n != 0 {
			t.Errorf("`gvm help %s` wrote %d file(s); a topic must touch no host", topic, n)
		}
	}
}

// TestHelpListsTopics: the no-argument help names the topics, so they are
// discoverable without being told they exist.
func TestHelpListsTopics(t *testing.T) {
	h := newTestHost(t)
	code, out, _ := runWith(t, h, "", "help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "Help topics") || !strings.Contains(out, "faq") {
		t.Errorf("help did not list its topics:\n%s", out)
	}
}

// TestUnknownHelpTopicSuggests: a near-miss topic name suggests the real one
// rather than printing an empty page.
func TestUnknownHelpTopicSuggests(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "", "help", "exitcodes")
	combined := out + errb
	if code == 0 {
		t.Errorf("`gvm help exitcodes` exited 0; a bad topic should not succeed:\n%s", combined)
	}
	if !strings.Contains(combined, "exit-codes") {
		t.Errorf("no suggestion for a mistyped topic:\n%s", combined)
	}
}
