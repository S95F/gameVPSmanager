// Package firewall computes and reconciles the host firewall rule set.
//
// The two load-bearing invariants, enforced structurally:
//
//  1. No computed rule set may fail to admit SSH. Guard.Check runs first in
//     Reconcile; there is no bypass flag and no code path around it.
//
//  2. gvm deletes only rules it tagged. A foreign (untagged) rule survives
//     every gvm operation forever. Deletions are suppressed entirely when the
//     live-state parse is not trusted (D-42).
package firewall

import (
	"fmt"
	"strconv"
	"strings"
)

// Proto is a network protocol.
type Proto string

const (
	ProtoTCP  Proto = "tcp"
	ProtoUDP  Proto = "udp"
	ProtoBoth Proto = "any"
)

// Verb is allow/deny/reject/limit.
type Verb string

const (
	VerbAllow  Verb = "allow"
	VerbDeny   Verb = "deny"
	VerbReject Verb = "reject"
	VerbLimit  Verb = "limit"
)

// Rule is a single firewall rule as gvm understands it.
type Rule struct {
	Verb    Verb
	Proto   Proto
	Port    int    // 0 means any
	PortEnd int    // 0 means single port; > Port means range
	From    string // "" means any; CIDR or IP
	Comment string
	Family  string // "v4", "v6", or "" for both
}

// Key is the identity for diffing. Comment, Family, and Index are deliberately
// excluded: a rule whose comment changed is the same rule, and the v4/v6 pair
// ufw creates is ONE logical rule.
type Key struct {
	Verb    Verb
	Proto   Proto
	Port    int
	PortEnd int
	From    string
}

// Key returns the comparison key for this rule.
func (r Rule) Key() Key {
	return Key{r.Verb, r.Proto, r.Port, r.PortEnd, r.From}
}

// String renders the rule as a ufw argument string (without the verb).
func (r Rule) String() string {
	var b strings.Builder
	if r.From != "" {
		fmt.Fprintf(&b, "from %s to any ", r.From)
	}
	if r.Port > 0 {
		fmt.Fprintf(&b, "port %d", r.Port)
		if r.PortEnd > r.Port {
			fmt.Fprintf(&b, ":%d", r.PortEnd)
		}
		if r.Proto != "" && r.Proto != ProtoBoth {
			fmt.Fprintf(&b, " proto %s", r.Proto)
		}
	}
	return strings.TrimSpace(b.String())
}

// UFWSpec returns the ufw command-line spec for adding this rule.
func (r Rule) UFWSpec() string {
	var parts []string
	switch r.Verb {
	case VerbLimit:
		parts = append(parts, "limit")
	case VerbDeny:
		parts = append(parts, "deny")
	case VerbReject:
		parts = append(parts, "reject")
	default:
		parts = append(parts, "allow")
	}

	if r.From != "" {
		parts = append(parts, "from", r.From, "to", "any")
	}
	if r.Port > 0 {
		portStr := strconv.Itoa(r.Port)
		if r.PortEnd > r.Port {
			portStr += ":" + strconv.Itoa(r.PortEnd)
		}
		parts = append(parts, "port", portStr)
		if r.Proto != "" && r.Proto != ProtoBoth {
			parts = append(parts, "proto", string(r.Proto))
		}
	}
	if r.Comment != "" {
		parts = append(parts, "comment", "'"+r.Comment+"'")
	}
	return strings.Join(parts, " ")
}

// RuleSet is an ordered list of rules.
type RuleSet []Rule

// Keyed returns a map from Key to the first Rule with that key.
func (rs RuleSet) Keyed() map[Key]Rule {
	m := map[Key]Rule{}
	for _, r := range rs {
		k := r.Key()
		if _, dup := m[k]; !dup {
			m[k] = r
		}
	}
	return m
}
