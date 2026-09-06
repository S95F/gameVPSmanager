// Package hostops is the one place gvm changes the host, and the closed list
// of ways it can.
//
// Section 3.2 splits the control plane in two: an internet-reachable web tier
// that holds no master key and cannot touch the host, and a root daemon that
// can. The split costs one unix socket and this file. It is explicitly NOT a
// microservice boundary -- gvmd and gvm-web are always the same build, there
// is no versioning between them -- so the only thing the boundary actually
// buys is that the privileged surface is enumerable. A root daemon whose
// surface is "run this command" would have cost the same socket and bought
// nothing.
//
// Two properties make that real, and both are asserted by verbs_test.go:
//
//   - The verb set is CLOSED. Every handler has a declared verb and every
//     declared verb has a handler. A handler reachable over the socket that
//     nobody declared is an undeclared privileged operation, which is the
//     precise failure this package exists to make impossible.
//   - Verbs name INTENT, never paths, unit names or accounts. `unit.start`
//     takes an instance name and derives `gvm@<name>.service`; there is no
//     verb that starts an arbitrary unit, writes an arbitrary path or chowns
//     an arbitrary file. A single `fs.write(path, bytes)` verb would make the
//     enum decorative: gvm-web could write /etc/sudoers and the audit row
//     would say `file.write`.
//
// The implementation surface is deliberately WIDER than the verb surface.
// user_linux.go implements all of platform.Users, including AddToGroup, which
// the packaging needs and which is not a verb -- "add any account to any
// group" hands out root by way of the sudo group. Where those two surfaces
// differ is the line the split draws, and it is drawn here rather than in the
// socket server, so the in-process root CLI is bounded by exactly the same
// list as the socket.
//
// There is exactly one implementation (ops.go). The root CLI calls Do
// in-process because it is already root and a socket hop would add nothing;
// gvm-web reaches the same Do over /run/gvm/admin.sock (phase 15). Neither
// path has its own copy of any decision.
package hostops

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/audit"
)

// Verb is one privileged operation. The set below is the complete list.
type Verb string

// The verbs, grouped by what they touch.
//
// The names are `<subject>.<verb>` and travel over the socket verbatim, so an
// operator greps the same string in a journald line, in an audit_log row and
// in this file.
const (
	// Supervision. Each acts on ONE unit belonging to ONE instance, derived
	// from the instance name by internal/instance. `systemctl start caddy` is
	// not expressible here and never will be by accident: the edge layer gets
	// its own declared verbs when it lands.
	VerbUnitStart   Verb = "unit.start"
	VerbUnitStop    Verb = "unit.stop"
	VerbUnitRestart Verb = "unit.restart"
	VerbUnitEnable  Verb = "unit.enable"
	VerbUnitDisable Verb = "unit.disable"

	// VerbDaemonReload makes the manager re-read unit files. It is separate
	// from the writes rather than folded into them because one apply writes
	// several files and reloads once, and a reload per file would make a
	// three-file apply three times as slow for no gain.
	VerbDaemonReload Verb = "daemon.reload"

	// Observation. These change nothing, and per the table below they carry no
	// audit action -- see Decl.Audit.
	VerbUnitState Verb = "unit.state"
	VerbUnitList  Verb = "unit.list"
	VerbDiskUsage Verb = "disk.usage"

	// The control plane's own files and directories (section 5). The verb
	// names a KIND; hostops derives the path, the mode and the owner.
	VerbFileWrite  Verb = "file.write"
	VerbFileRemove Verb = "file.remove"
	VerbDirEnsure  Verb = "dir.ensure"

	// VerbUserEnsure creates the instance's system account if it is absent.
	// It never modifies one that exists: an adopted instance's account
	// predates gvm, may be shared with things gvm cannot see, and "ensure"
	// meaning "make it match my idea of it" is how an adoption breaks a
	// server that was working.
	VerbUserEnsure Verb = "user.ensure"
)

// Effect says whether a verb changes the host.
//
// It exists because the audit requirement differs. Every MUTATING verb carries
// an audit action with a parameter allowlist, so nothing gvm does to a host is
// unrecorded. Observing verbs carry none, deliberately: audit_log is
// append-only by grant (the application roles hold SELECT and INSERT and
// nothing else), and a dashboard polling unit state every two seconds would
// bury the rows that matter under millions that do not. They are still
// declared here, because an undeclared read is still an undeclared capability.
type Effect uint8

const (
	// Observes reads host state and writes nothing.
	Observes Effect = iota + 1
	// Mutates changes the host.
	Mutates
)

func (e Effect) String() string {
	switch e {
	case Observes:
		return "observes"
	case Mutates:
		return "mutates"
	}
	return "invalid"
}

// Args is the set of Request fields a verb reads.
//
// It is declared rather than inferred so that a field set on a verb that
// ignores it is REFUSED. A `dir.ensure` carrying a Body is a caller that
// believes it wrote a file and did not; a `daemon.reload` carrying a file kind
// is a caller that has confused two steps of an apply. Both would otherwise
// succeed, silently, having done half of what the caller meant.
type Args uint16

const (
	ArgUnit Args = 1 << iota
	ArgScope
	ArgFile
	ArgDir
	ArgBody
	ArgJob
	ArgLegacy
)

// argNames is what a refusal prints. It is also what verbs_test.go enumerates
// to prove no declaration carries a bit nobody defined.
var argNames = map[Args]string{
	ArgUnit:   "unit",
	ArgScope:  "scope",
	ArgFile:   "file",
	ArgDir:    "dir",
	ArgBody:   "body",
	ArgJob:    "job",
	ArgLegacy: "legacy_unit",
}

// Has reports whether the set contains a. It is a method so the check reads
// the same way at every call site.
func (a Args) Has(x Args) bool { return a&x != 0 }

// Decl is one verb's declaration: everything the dispatcher needs to know
// about a verb before it reaches the verb's own code.
type Decl struct {
	Verb   Verb
	Effect Effect

	// Args is which addressing fields this verb reads. Anything else set on
	// the request is refused.
	Args Args

	// Audit is the action the record is written under. Required for Mutates,
	// forbidden for Observes.
	//
	// Several verbs share audit.InstanceApply, and that is correct rather than
	// lazy: writing a drop-in, ensuring a directory and creating the instance
	// account are steps of one `gvm apply`, and section 14.8 audits the apply.
	// The verb itself lands in params.actions, so the row still says which
	// step it was.
	Audit audit.Action

	// Instance says the request is meaningless without an instance name. It
	// is true for everything except unit.list, which asks about the host.
	Instance bool

	// Summary is one line for `gvm doctor` and for the socket's capability
	// listing, so the enumerable surface is also a readable one.
	Summary string
}

// verbs is the declaration table, and it is the definition of the closed set.
//
// ops.go's dispatch table must have exactly these keys. verbs_test.go asserts
// that in both directions and also against the constants above, read out of
// this file's source: a handler keyed by a string literal that is not one of
// the constants would satisfy a map-to-map comparison and is caught by the
// third leg.
var verbs = map[Verb]Decl{
	VerbUnitStart: {
		Verb: VerbUnitStart, Effect: Mutates, Audit: audit.InstanceStart, Instance: true,
		Args:    ArgUnit | ArgJob,
		Summary: "start an instance's service unit",
	},
	VerbUnitStop: {
		Verb: VerbUnitStop, Effect: Mutates, Audit: audit.InstanceStop, Instance: true,
		Args:    ArgUnit | ArgJob,
		Summary: "stop an instance's service unit and wait for the job to finish",
	},
	VerbUnitRestart: {
		Verb: VerbUnitRestart, Effect: Mutates, Audit: audit.InstanceStart, Instance: true,
		Args:    ArgUnit | ArgJob,
		Summary: "restart an instance's service unit",
	},
	VerbUnitEnable: {
		Verb: VerbUnitEnable, Effect: Mutates, Audit: audit.InstanceApply, Instance: true,
		Args:    ArgUnit | ArgJob,
		Summary: "enable an instance's unit or timer at boot",
	},
	VerbUnitDisable: {
		Verb: VerbUnitDisable, Effect: Mutates, Audit: audit.InstanceApply, Instance: true,
		Args:    ArgUnit | ArgJob,
		Summary: "disable an instance's unit or timer at boot",
	},
	VerbDaemonReload: {
		Verb: VerbDaemonReload, Effect: Mutates, Audit: audit.InstanceApply, Instance: true,
		Summary: "make systemd re-read unit files after an apply wrote them",
	},
	VerbUnitState: {
		Verb: VerbUnitState, Effect: Observes, Instance: true,
		Args:    ArgUnit | ArgJob,
		Summary: "read one unit's load/active/sub state and last result",
	},
	VerbUnitList: {
		Verb: VerbUnitList, Effect: Observes, Args: ArgScope,
		Summary: "list the gvm units systemd knows about, by scope",
	},
	VerbDiskUsage: {
		Verb: VerbDiskUsage, Effect: Observes, Instance: true, Args: ArgDir,
		Summary: "statfs the filesystem holding one of an instance's directories",
	},
	VerbFileWrite: {
		Verb: VerbFileWrite, Effect: Mutates, Audit: audit.InstanceApply, Instance: true,
		Args:    ArgFile | ArgJob | ArgLegacy | ArgBody,
		Summary: "atomically write one of the control-plane files gvm owns",
	},
	VerbFileRemove: {
		Verb: VerbFileRemove, Effect: Mutates, Audit: audit.InstanceApply, Instance: true,
		Args:    ArgFile | ArgJob | ArgLegacy,
		Summary: "remove one of the control-plane files gvm owns",
	},
	VerbDirEnsure: {
		Verb: VerbDirEnsure, Effect: Mutates, Audit: audit.InstanceApply, Instance: true,
		Args:    ArgDir,
		Summary: "create one of an instance's directories with its declared mode and owner",
	},
	VerbUserEnsure: {
		Verb: VerbUserEnsure, Effect: Mutates, Audit: audit.InstanceApply, Instance: true,
		Summary: "create the instance's system account and group if absent; never modify one that exists",
	},
}

// ErrUnknownVerb is returned for a verb that is not in the table.
//
// It is a sentinel rather than a formatted error because the socket server
// answers it differently from every other failure: an undeclared verb arriving
// from gvm-web is a security event, not an operating condition, and Do records
// it as a denial before returning.
var ErrUnknownVerb = errors.New("hostops: verb is not declared")

// Lookup returns a verb's declaration.
func Lookup(v Verb) (Decl, bool) { d, ok := verbs[v]; return d, ok }

// Verbs returns every declared verb, sorted. It is what the capability
// listing and `gvm doctor` print.
func Verbs() []Verb {
	out := make([]Verb, 0, len(verbs))
	for v := range verbs {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// Declarations returns every declaration, sorted by verb.
func Declarations() []Decl {
	out := make([]Decl, 0, len(verbs))
	for _, v := range Verbs() {
		out = append(out, verbs[v])
	}
	return out
}

// validVerbName is the shape every verb string must have: two lowercase
// dot-separated segments.
//
// It is enforced by a test rather than at runtime because verbs are constants;
// the value of checking is that "unitStart" or "unit.start " cannot be added
// by hand and go unnoticed in a table this long.
func validVerbName(v Verb) error {
	subject, action, ok := strings.Cut(string(v), ".")
	if !ok {
		return fmt.Errorf("%q has no '.' separating subject from action", v)
	}
	for _, part := range []string{subject, action} {
		if part == "" {
			return fmt.Errorf("%q has an empty segment", v)
		}
		for _, r := range part {
			if (r < 'a' || r > 'z') && r != '_' {
				return fmt.Errorf("%q contains %q; verbs are lowercase with '_' inside a segment", v, r)
			}
		}
	}
	if strings.Contains(action, ".") {
		return fmt.Errorf("%q has more than two segments", v)
	}
	return nil
}
