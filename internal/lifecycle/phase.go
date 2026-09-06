// Package lifecycle answers one question -- what is this instance doing right
// now -- and keeps it structurally separate from the question of what to do
// about it.
//
// Three things live here and the boundaries between them are the design.
//
// Observation is what the host says: the instance YAML, the manifest pin, the
// generated drop-in's own header, systemd's unit state, the runner's
// status.json, the /run markers and the job lease. Gathering it reads and
// never writes, and every source is behind a seam, so the whole of it can be
// constructed by hand in a test.
//
// Derive turns an Observation into a Phase. It is PURE: no clock of its own,
// no filesystem, no systemd, no database. That is not tidiness. The phase
// table is the piece an operator argues with at 3am ("why does it say
// failed"), and a function that can be called with a literal struct and
// compared against a literal answer is a function that can be argued with. It
// is also the piece most likely to be wrong, and a table test over every
// Observation shape is only possible because nothing in it can fail for an
// unrelated reason.
//
// The Reconciler observes and REPORTS. It does not start, stop or restart
// anything, ever, and it cannot: it holds a UnitReader, which has no start
// method on it (testdata/reconciler-actuates is the compile-fail fixture that
// keeps it that way). A reconciler that healed by itself would eventually
// fight an operator who is mid-repair -- restarting the instance they stopped
// on purpose, halfway through a manual world fix. Healing has an owner and it
// is not this: `gvm-guard.timer` runs a job that decides deliberately, from
// spec.enabled and the hold file, and records what it did (section 13.4).
// Actuation on an operator's behalf is Manager's, and it is a different type
// on purpose.
package lifecycle

// Phase is one instance's coarse condition.
//
// The values are exactly `instance_phase_t` from the schema (section 14.4) and
// the order below is the declaration order there. They are the same strings in
// the YAML, in the JSON `gvm status --json` prints, in journald and in the
// database, so an operator greps one word across all four.
type Phase string

const (
	// PhaseAbsent means no /etc/gvm/instances/<name>.yaml. The registry may
	// still hold a row -- section 14.4 sets absent_since rather than deleting,
	// because grants hang off the id -- so "gvm has never heard of it" and
	// "the file is gone" are the same phase and a different reason.
	PhaseAbsent Phase = "absent"
	// PhaseDeclared is a spec on disk that no apply has converged. Nothing
	// has been written to the host on its behalf.
	PhaseDeclared Phase = "declared"
	// PhaseProvisioning is applied -- account, directories and unit exist --
	// with no game files yet.
	PhaseProvisioning Phase = "provisioning"
	// PhaseInstalling is a provider writing the install tree right now.
	PhaseInstalling Phase = "installing"
	// PhaseAwaitingManualFiles is content that cannot be fetched without a
	// human: a CurseForge project whose author blocked third-party download
	// (section 12.8). The server may still be running; the phase says the
	// content intent cannot be satisfied unattended.
	PhaseAwaitingManualFiles Phase = "awaiting_manual_files"
	// PhaseStopped is not running and that is not a surprise -- disabled, or
	// held by an operator, or cleanly exited.
	PhaseStopped Phase = "stopped"
	// PhaseStarting covers everything between exec and READY=1.
	PhaseStarting Phase = "starting"
	// PhaseRunning is up and ready.
	PhaseRunning Phase = "running"
	// PhaseDegraded is running, or claiming to be, with something gvm needs
	// in order to say so not answering. It is a phase here and a flag in the
	// runner's own status.json, and that is deliberate: the runner knows
	// whether ITS shutdown was degraded, while this is a statement about the
	// whole instance including the parts the runner cannot see.
	PhaseDegraded Phase = "degraded"
	// PhaseStopping is the shutdown ladder, in progress.
	PhaseStopping Phase = "stopping"
	// PhaseUpdating is a job that is rewriting the install tree or the world.
	PhaseUpdating Phase = "updating"
	// PhaseFailed is a condition that will not resolve without a decision.
	// Every rule that produces it carries a hint naming the command that
	// makes the decision, because "failed" with no next move is where an
	// operator starts guessing.
	PhaseFailed Phase = "failed"
)

// phaseOrder is the enum's declaration order, which the schema calls
// load-bearing. Keeping the Go and SQL orders identical means an ordinal
// stored by one can be read by the other, and a phase added to one and not the
// other is caught by TestPhasesMatchTheSchemaEnum rather than by a query that
// silently returns the wrong name.
var phaseOrder = []Phase{
	PhaseAbsent, PhaseDeclared, PhaseProvisioning, PhaseInstalling,
	PhaseAwaitingManualFiles, PhaseStopped, PhaseStarting, PhaseRunning,
	PhaseDegraded, PhaseStopping, PhaseUpdating, PhaseFailed,
}

// Phases returns every declared phase in schema order.
func Phases() []Phase { return append([]Phase(nil), phaseOrder...) }

// Valid reports whether p is one of the declared phases. A phase that came
// from a file or a socket is a string until this says otherwise.
func (p Phase) Valid() bool {
	for _, q := range phaseOrder {
		if p == q {
			return true
		}
	}
	return false
}

func (p Phase) String() string { return string(p) }

// Severity is how a phase should be counted in a summary.
//
// It exists so that `gvm list` and `gvm doctor` agree on what "3 problems"
// means. Section 20's own note applies: informational things are notes, so the
// problem count stays meaningful and an operator is not trained to ignore a
// permanently non-zero number.
type Severity uint8

const (
	// SevOK is where the instance is meant to be.
	SevOK Severity = iota + 1
	// SevNotice is in flight or deliberately not running. Nothing to do.
	SevNotice
	// SevProblem needs a person.
	SevProblem
)

func (s Severity) String() string {
	switch s {
	case SevOK:
		return "ok"
	case SevNotice:
		return "notice"
	case SevProblem:
		return "problem"
	}
	return "invalid"
}

// Severity classifies a phase.
//
// PhaseStopped is a NOTICE and not a problem: an instance an operator stopped,
// or disabled, is doing exactly what it was told. The guard timer is what
// notices a stop nobody asked for (section 13.4), and it decides that from the
// hold file rather than from a severity.
func (p Phase) Severity() Severity {
	switch p {
	case PhaseRunning:
		return SevOK
	case PhaseAbsent, PhaseDeclared, PhaseProvisioning, PhaseInstalling,
		PhaseStopped, PhaseStarting, PhaseStopping, PhaseUpdating:
		return SevNotice
	case PhaseAwaitingManualFiles, PhaseDegraded, PhaseFailed:
		return SevProblem
	}
	return SevProblem
}

// Transitional reports whether the phase is expected to change on its own.
//
// Watch polls these more often, and `gvm start --wait` waits on them. A phase
// that is not transitional will still be there in an hour, so a caller that
// polls it is burning CPU to learn nothing.
func (p Phase) Transitional() bool {
	switch p {
	case PhaseProvisioning, PhaseInstalling, PhaseStarting, PhaseStopping, PhaseUpdating:
		return true
	}
	return false
}

// Serving reports whether players can be on right now.
//
// PhaseDegraded is true here on purpose. Degraded means gvm cannot fully
// account for the instance, not that it is down, and a dashboard that hid a
// degraded server from the "up" count would hide the one that most needs
// looking at.
func (p Phase) Serving() bool {
	return p == PhaseRunning || p == PhaseDegraded || p == PhaseStopping
}
