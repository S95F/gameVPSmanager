package firewall

import (
	"slices"
	"strings"
	"testing"
)

// TestUFWArgsAreAProperArgvWithTheTagUnquoted is Phase 40 item 1's safety fix.
//
// The old backend built argv with strings.Fields(UFWSpec()), which turned a
// comment of "gvm:v1:game" into the single element "'gvm:v1:game'" — quotes and
// all — so ufw stored the quotes as part of the comment, and tag matching (the
// thing orphan detection and tag-owned deletes depend on) silently stopped
// working. UFWArgs returns the tag as a bare element, in the canonical
// `<verb> [proto P] [from X] to any [port N] comment TAG` form.
func TestUFWArgsAreAProperArgvWithTheTagUnquoted(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want []string
	}{
		{
			"a game udp port range",
			Rule{Verb: VerbAllow, Proto: ProtoUDP, Port: 2456, PortEnd: 2457, Comment: "gvm:v1:game"},
			[]string{"allow", "proto", "udp", "to", "any", "port", "2456:2457", "comment", "gvm:v1:game"},
		},
		{
			"a host tcp port",
			Rule{Verb: VerbAllow, Proto: ProtoTCP, Port: 80, Comment: "gvm:host:http"},
			[]string{"allow", "proto", "tcp", "to", "any", "port", "80", "comment", "gvm:host:http"},
		},
		{
			"an admin cidr with a source",
			Rule{Verb: VerbAllow, Proto: ProtoTCP, Port: 22, From: "203.0.113.0/24", Comment: "gvm:host:ssh"},
			[]string{"allow", "proto", "tcp", "from", "203.0.113.0/24", "to", "any", "port", "22", "comment", "gvm:host:ssh"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.UFWArgs(true); !slices.Equal(got, tc.want) {
				t.Errorf("UFWArgs(true) = %v, want %v", got, tc.want)
			}
		})
	}

	// The tag is one bare element: never shell-quoted, never split by a space.
	got := Rule{Verb: VerbAllow, Proto: ProtoUDP, Port: 2456, Comment: "gvm:v1:game"}.UFWArgs(true)
	last := got[len(got)-1]
	if strings.ContainsAny(last, "' ") {
		t.Errorf("the tag argv element is quoted or split: %q (full: %v)", last, got)
	}

	// A delete matches by spec, so it carries no comment.
	del := Rule{Verb: VerbAllow, Proto: ProtoUDP, Port: 2456, Comment: "gvm:v1:game"}.UFWArgs(false)
	if slices.Contains(del, "comment") || slices.Contains(del, "gvm:v1:game") {
		t.Errorf("a delete spec carries the comment, which would make the delete miss: %v", del)
	}
}
