package firewall

// StepKind classifies a reconcile step.
type StepKind string

const (
	StepAdd    StepKind = "add"
	StepDelete StepKind = "delete"
	StepAdopt  StepKind = "adopt"  // foreign rule in the exact slot → bring under management
	StepShadow StepKind = "shadow" // warning: foreign deny/reject earlier than a desired allow
)

// Step is one action in the reconcile plan.
type Step struct {
	Kind   StepKind
	Rule   Rule
	Reason string
}

// Plan is the output of Reconcile.
type Plan struct {
	Steps          []Step
	ForeignRules   []Rule // counted and reported, never in Steps
	AlreadyCorrect []Rule
}

// Reconcile computes the steps to move from live to desired.
// The guard runs FIRST and returns an error if SSH would not be admitted.
//
// Invariants enforced:
//   - SSH rules are never deleted, even if tagged by gvm.
//   - Deletions are suppressed entirely when !g.LiveParseTrusted (D-42).
//   - Adds come before deletes so a re-tag never opens a gap.
//   - Within adds, SSH rules come first.
//   - A foreign rule in the exact desired slot becomes an Adoption, not add+delete.
func Reconcile(live, desired RuleSet, g Guard) (*Plan, error) {
	// 0. THE INVARIANT, FIRST. There is no path around this.
	if err := g.Check(desired, true); err != nil {
		return nil, err
	}

	plan := &Plan{}
	desiredKeyed := desired.Keyed()

	isSSHPort := func(port int) bool {
		for _, p := range g.SSHPorts {
			if p == port {
				return true
			}
		}
		return port == g.LiveSessionPort && g.LiveSessionPort > 0
	}

	// Separate live rules into gvm-tagged and foreign.
	liveTagged := map[Key]Rule{}
	liveForeign := map[Key]Rule{}
	for _, r := range live {
		if _, ok := ParseTag(r.Comment); ok {
			liveTagged[r.Key()] = r
		} else {
			liveForeign[r.Key()] = r
		}
	}

	// 1. ADDS (and adoptions): desired rules not present in live gvm-tagged.
	var sshAdds, otherAdds []Step
	for _, want := range desired {
		k := want.Key()

		if _, ok := liveTagged[k]; ok {
			// Already present with a gvm tag — check if it's the right one.
			existing := liveTagged[k]
			if existing.Comment == want.Comment {
				plan.AlreadyCorrect = append(plan.AlreadyCorrect, existing)
			} else {
				// Re-tag: add the new comment then delete the old one.
				otherAdds = append(otherAdds, Step{Kind: StepAdd, Rule: want, Reason: "re-tag"})
			}
			continue
		}
		if _, ok := liveForeign[k]; ok {
			// A foreign rule occupies the exact slot — adopt, not add+delete.
			plan.Steps = append(plan.Steps, Step{Kind: StepAdopt, Rule: want, Reason: "foreign rule in slot"})
			continue
		}
		// New rule.
		s := Step{Kind: StepAdd, Rule: want}
		if want.Proto == ProtoTCP && isSSHPort(want.Port) {
			sshAdds = append(sshAdds, s)
		} else {
			otherAdds = append(otherAdds, s)
		}
	}

	// SSH adds first, then everything else.
	plan.Steps = append(plan.Steps, sshAdds...)
	plan.Steps = append(plan.Steps, otherAdds...)

	// 2. SHADOW CHECK: a foreign deny/reject earlier than a desired allow.
	// (fail2ban per-host bans are excluded by shape — they have a `from` IP.)
	for _, want := range desired {
		if want.Verb != VerbAllow && want.Verb != VerbLimit {
			continue
		}
		for _, lr := range live {
			if lr.From != "" {
				continue // fail2ban shape
			}
			if lr.Verb != VerbDeny && lr.Verb != VerbReject {
				continue
			}
			if lr.Port == want.Port && (lr.Proto == want.Proto || lr.Proto == ProtoBoth) {
				if _, foreign := liveForeign[lr.Key()]; foreign {
					plan.Steps = append(plan.Steps, Step{Kind: StepShadow, Rule: lr,
						Reason: "foreign deny/reject shadows desired allow for port " + itoa(want.Port)})
				}
			}
		}
	}

	// 3. DELETES: only gvm-tagged rules no longer in desired.
	if g.LiveParseTrusted {
		for k, r := range liveTagged {
			if _, wanted := desiredKeyed[k]; wanted {
				continue
			}
			// Never delete an SSH rule, even if tagged.
			if r.Proto == ProtoTCP && isSSHPort(r.Port) {
				continue
			}
			plan.Steps = append(plan.Steps, Step{Kind: StepDelete, Rule: r, Reason: "no longer in desired set"})
		}
	}

	// 4. FOREIGN: count and report, never act.
	for _, r := range live {
		if _, ok := ParseTag(r.Comment); !ok {
			plan.ForeignRules = append(plan.ForeignRules, r)
		}
	}

	return plan, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	b := make([]byte, 0, 8)
	if n < 0 {
		b = append(b, '-')
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
