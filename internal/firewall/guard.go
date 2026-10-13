package firewall

import (
	"errors"
	"fmt"
	"strings"
)

// Guard carries the SSH invariant. It is a separate type so the invariant is a
// value that must be constructed and passed, rather than an `if` someone can
// forget. There is no bypass flag and no code path around Check.
type Guard struct {
	// SSHPorts is the union of all detected SSH ports.
	SSHPorts []int
	// LiveSessionPort is the port of the session running this command, if known.
	LiveSessionPort int
	// BackendEnabled is true when the firewall backend is currently active.
	BackendEnabled bool
	// AllowEnable is only set by `gvm firewall enable` — all other callers have
	// this false, so an inactive firewall stays staged-only.
	AllowEnable bool
	// LiveParseTrusted is false when the ufw status parser could not account
	// for every rule it saw. Deletions are suppressed entirely (D-42).
	LiveParseTrusted bool
	// Confident mirrors Detection.Confident: false means we could not observe
	// sshd at all and should refuse to enable a deny-by-default firewall.
	Confident bool
}

var (
	// ErrWouldLockOut is returned when the computed rule set would not admit SSH.
	// There is no bypass flag; the operator must fix the desired set.
	ErrWouldLockOut = errors.New("firewall: the computed rule set would not admit SSH")
	// ErrSSHUnknown is returned by enable when detection is not confident.
	ErrSSHUnknown = errors.New("firewall: could not determine any SSH port; refusing to enable")
	// ErrAmbiguousRule is returned when more than one live rule matches a delete spec.
	ErrAmbiguousRule = errors.New("firewall: more than one live rule matches; refusing to delete")
)

// Check asserts that the desired rule set admits every detected SSH port.
// If defaultIncomingDeny is true (the only sensible value for `gvm firewall enable`)
// then EVERY detected port must have an explicit allow/limit rule in desired.
func (g Guard) Check(desired RuleSet, defaultIncomingDeny bool) error {
	if len(g.SSHPorts) == 0 {
		// No SSH ports known — not an error here; ErrSSHUnknown is for enable.
		return nil
	}
	if !defaultIncomingDeny {
		// An allow-by-default firewall cannot lock you out.
		return nil
	}

	// Build a quick admittance check.
	admitted := map[int]bool{}
	for _, r := range desired {
		if r.Verb != VerbAllow && r.Verb != VerbLimit {
			continue
		}
		if r.Port == 0 {
			// A blanket allow admits everything.
			for _, p := range g.SSHPorts {
				admitted[p] = true
			}
			break
		}
		for _, p := range g.SSHPorts {
			if r.Port == p && (r.Proto == ProtoTCP || r.Proto == ProtoBoth || r.Proto == "") {
				admitted[p] = true
			}
		}
	}

	var missing []string
	for _, p := range g.SSHPorts {
		if !admitted[p] {
			missing = append(missing, fmt.Sprintf("%d/tcp", p))
		}
	}
	// The live session port must be admitted even if it appears in no config file.
	if g.LiveSessionPort > 0 && !admitted[g.LiveSessionPort] {
		alreadyListed := false
		for _, s := range missing {
			if s == fmt.Sprintf("%d/tcp", g.LiveSessionPort) {
				alreadyListed = true
				break
			}
		}
		if !alreadyListed {
			missing = append(missing, fmt.Sprintf("%d/tcp (live session)", g.LiveSessionPort))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: not admitted: %s", ErrWouldLockOut, strings.Join(missing, ", "))
	}
	return nil
}
