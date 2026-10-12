package jobs

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The destructive-action contract (section 16.4): rebuild.sh's prompt, ported.
//
//	read -r -p "Type the server name to continue: " ANSWER
//	[ "$ANSWER" = "$SERVER_NAME" ] || die "Aborted."
//
// That discipline has to survive into an interface where a click is one pixel
// from a mistake. Section 16.4's answer is that the check is SERVER-SIDE and
// identical for the CLI, the API and the dashboard, and this file is where
// "server-side" is a place rather than an intention: the check runs inside
// Runner.Execute's validate, before the lock, before the guard, before the
// step table is built -- so a caller cannot reach a destroy by not calling it.
//
// Three decisions are worth stating, because each one is a thing a later edit
// would reasonably get wrong:
//
// THE RISK SENTENCE IS REQUIRED AND IS NOT THE CALLER'S. riskOf below is the
// only place that says what a kind destroys, and Destructive() is derived from
// it rather than being a second list. A prompt that could not say what it was
// about to delete would be a prompt people learn to answer by reflex, which is
// the failure mode rebuild.sh's own prompt exists to defeat.
//
// `--yes` IS NOT A CONFIRMATION OVER HTTP. Automated satisfies the contract
// from a timer or a root shell on the box, where the operator is the person who
// typed it. Section 16.4 requires the execute call to carry `confirm` EXACTLY
// equal to the instance name, and a boolean in a JSON body is one forged form
// away from being somebody else's click -- so an automated flag arriving with a
// web or API trigger is refused by name. That asymmetry is the whole reason the
// check lives here and not in internal/cli.
//
// CASE IS NOT FOLDED AND WHITESPACE IS ONLY TRIMMED. "PZ-MAIN" is not an
// acceptable answer to "type pz-main": the point of typing the name is that it
// cannot be produced by muscle memory, and every relaxation of the comparison
// moves it back towards "y".

// ErrNotConfirmed is a destructive action nobody authorised.
var ErrNotConfirmed = errors.New("jobs: not confirmed")

// riskOf is what each destructive kind does that cannot be undone, in the
// sentence the prompt and the refusal both use.
//
// One table, and Destructive reads it, so a kind cannot become destructive
// without acquiring a sentence that says why -- and cannot acquire a sentence
// without becoming destructive.
var riskOf = map[lifecycle.JobKind]string{
	lifecycle.JobRebuild: "a rebuild archives the world and then DELETES it, and everyone " +
		"starts over",
	lifecycle.JobRestore: "a restore overwrites the live world with the contents of an archive, " +
		"and everything since that archive was taken is gone",
}

// Destructive reports whether this kind destroys something an operator cares
// about and therefore needs section 16.4's typed confirmation.
func Destructive(k lifecycle.JobKind) bool {
	_, ok := riskOf[k]
	return ok
}

// DestructiveKinds returns every kind that needs a confirmation, in schema
// order, so a CLI or an API can enumerate them rather than hard-coding two.
func DestructiveKinds() []lifecycle.JobKind {
	out := make([]lifecycle.JobKind, 0, len(riskOf))
	for _, k := range lifecycle.JobKinds() {
		if Destructive(k) {
			out = append(out, k)
		}
	}
	return out
}

// RiskOf is the sentence a destructive kind's prompt must state, or "" for a
// kind that destroys nothing.
func RiskOf(k lifecycle.JobKind) string { return riskOf[k] }

// Confirmation is what a caller supplies to authorise a destructive job.
//
// It carries no instance name and no kind. Those come from the Env the job is
// about to run with, so a confirmation cannot be checked against a different
// instance from the one it will act on -- which is exactly the mistake a
// dashboard with two tabs open makes, and the one a plan_id (section 16.4
// step 1) exists to prevent at the HTTP edge.
type Confirmation struct {
	// Typed is what the operator typed at the prompt, or the API body's
	// `confirm` field. It must equal the instance name.
	Typed string
	// Automated is `--yes`: a script asserting that a human already decided.
	// A separate field from Typed so that "they typed it" and "automation
	// asserted it" are different facts in the audit row, and so that the
	// refusal below can treat them differently by trigger.
	Automated bool
	// ArchiveAck is section 16.4's `archive_ack`. It is recorded rather than
	// required: the type system already refuses a destroy without a verified
	// Ref (internal/archive.Destroy, instance.Destroy, rebuild's step 7), so
	// demanding the acknowledgement as well would be a second gate on the one
	// thing that is already impossible to route around. What it buys is an
	// audit row that says the operator was shown the archive path.
	ArchiveAck bool
	// Risk overrides riskOf's sentence, for a route whose consequence is
	// narrower or wider than the kind's default -- `gvm restore --to <dir>`
	// touches no live world and a caller may say so. Empty uses the kind's.
	Risk string
}

// Prompt is the line an interactive caller prints before reading an answer.
//
// Rendered here rather than in the CLI so that the sentence an operator is
// asked to agree to and the sentence a refusal quotes back at them are the
// same string. Two spellings of the same warning read as two different
// warnings.
func (c *Confirmation) Prompt(instanceName string, kind lifecycle.JobKind) string {
	return fmt.Sprintf("%s: %s\nType the instance name (%s) to continue: ",
		instanceName, c.risk(kind), instanceName)
}

func (c *Confirmation) risk(kind lifecycle.JobKind) string {
	if c != nil && strings.TrimSpace(c.Risk) != "" {
		return strings.TrimSpace(c.Risk)
	}
	return riskOf[kind]
}

// Check is the contract. It is pure, so every branch is a table test on the
// development machine.
//
// A nil receiver is the case that matters most and is handled first: it is
// what an API handler that forgot the middleware, or a CLI path that grew a new
// flag, actually produces. Treating it as "no confirmation" rather than as a
// programming error means the refusal is the same sentence an operator who
// typed nothing would get, which is the sentence that tells them what to do.
func (c *Confirmation) Check(instanceName string, kind lifecycle.JobKind, trig Trigger) error {
	if !Destructive(kind) {
		// Nothing to confirm. Checked rather than assumed, because a caller
		// that routes every job through here must not be able to invent a
		// confirmation requirement for a nightly backup.
		return nil
	}
	if err := instance.ValidateName(instanceName); err != nil {
		return fmt.Errorf("%w: a %s with no instance to act on: %w", ErrNotConfirmed, kind, err)
	}
	risk := c.risk(kind)
	if risk == "" {
		// Unreachable through riskOf, which is why it is checked: the only way
		// here is a caller that set Risk to whitespace, and a prompt that
		// cannot state the consequence is not a prompt.
		return fmt.Errorf("%w: %s on %s carries no statement of what it destroys, and a "+
			"confirmation that cannot say what it is confirming is not one",
			ErrNotConfirmed, kind, instanceName)
	}

	if c == nil {
		return fmt.Errorf("%w: %s on %s was not confirmed. %s. Type the instance name to "+
			"continue (--confirm %s), or --yes from automation",
			ErrNotConfirmed, kind, instanceName, risk, instanceName)
	}

	if typed := strings.TrimSpace(c.Typed); typed != "" {
		if typed == instanceName {
			return nil
		}
		return fmt.Errorf("%w: %q is not %q, so this %s is refused. %s",
			ErrNotConfirmed, typed, instanceName, kind, risk)
	}

	if c.Automated {
		if remoteTrigger(trig) {
			// Section 16.4, as a refusal rather than as a paragraph: the
			// execute call must carry `confirm` equal to the instance name.
			// A boolean in a request body proves only that something sent a
			// boolean, and the attacker in that section's threat model can
			// send one.
			return fmt.Errorf("%w: %s on %s arrived over %s with --yes and no typed name. "+
				"Section 16.4 requires the confirmation to be the instance name itself over "+
				"this transport: an automation flag is one forged request away from being "+
				"somebody else's click. %s",
				ErrNotConfirmed, kind, instanceName, trig, risk)
		}
		return nil
	}

	return fmt.Errorf("%w: type the instance name (%s) to continue; %s",
		ErrNotConfirmed, instanceName, risk)
}

// remoteTrigger is whether this request crossed a network boundary.
//
// Enumerated rather than defaulted: an unknown trigger reads as remote,
// because the safe direction for a check that authorises deleting a world is
// to demand more than the caller expected rather than less.
func remoteTrigger(t Trigger) bool {
	local := []Trigger{TriggerCLI, TriggerTimer, TriggerGuard, TriggerChained}
	return !slices.Contains(local, t)
}
