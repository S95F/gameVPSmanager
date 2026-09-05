// Package instance holds the primitives that turn an instance's name into
// everything the host derives from it: a systemd unit, a unix account, a path
// set, cgroup limits, and -- the reason this package exists -- a path that has
// been proven safe to delete.
package instance

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// ErrName is the sentinel every naming failure wraps.
var ErrName = errors.New("instance: invalid name")

// nameRE is D-45 verbatim. Each clause is load-bearing:
//
//	[a-z0-9]      first character: a leading '-' is a flag to useradd and to
//	              half the CLI tools that will ever see this name.
//	[a-z0-9-]     lowercase only, because SteamCMD's force_install_dir fails on
//	              paths containing capitals on some distributions; and no '_',
//	              which stays free as the job-slug separator (D-13).
//	{0,26}        with the two anchor characters, 2..28 total: useradd caps at
//	              32 and the "gvm-" prefix costs 4.
//	[a-z0-9]      last character: a trailing '-' makes "gvm-<name>-" style
//	              derived names ambiguous to read.
//
// The charset is also exactly what makes systemd's %i and %I byte-identical,
// so unit-name escaping never enters the picture anywhere in gvm.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,26}[a-z0-9]$`)

// MaxNameLen is D-45's ceiling, and the reason for it.
const MaxNameLen = 28

// MinNameLen is the floor the regex's two anchors impose.
const MinNameLen = 2

// reserved maps a name gvm refuses to the collision it would cause. The value
// is not decoration: an operator who is told "reserved" and not why will try
// the next name on the list, which may be reserved for the same reason.
//
// The first block is D-45's list. The second is section 6.4's: an instance's
// unix account is gvm-<name>, so an instance called "console" would ask for
// the account gvm-console -- the name of the group that gates every console
// socket on the box (D-10). Those three are not in D-45's list because D-45
// enumerated units and verbs, not accounts; the account collision is worse
// than any of them, so they are here.
var reserved = map[string]string{
	"all":     "`gvm stop --all` and `--all` everywhere else would become ambiguous",
	"none":    "`expose: none`, `console: none` and `control.kind: none` all use it as a keyword",
	"gvmd":    "gvmd.service is the root daemon's own unit",
	"run":     "`gvm run <instance>` is the runner's own verb",
	"job":     "gvm-job@ is the job unit template (D-13)",
	"jobs":    "gvm-jobs.slice is the slice every job runs in",
	"host":    "host-level jobs are spelled with an empty instance part, not with this name (D-13)",
	"default": "systemd resolves default.target; a unit named for it reads as systemd's, not gvm's",
	"system":  "reads as gvm's own system account rather than one instance's",
	"guard":   "_guard is a host-level job slug (D-13)",
	"verify":  "_verify is a host-level job slug (D-13)",

	"web":     "the unix account gvm-web belongs to the web tier (section 6.4)",
	"fetch":   "the unix account gvm-fetch downloads third-party content (D-16)",
	"console": "the group gvm-console gates every instance's console socket (D-10)",
	"content": "the group gvm-content owns the shared blob store (section 6.4)",
}

// ReservedNames returns the reserved words with the reason each is reserved.
func ReservedNames() map[string]string {
	out := make(map[string]string, len(reserved))
	for k, v := range reserved {
		out[k] = v
	}
	return out
}

// ValidateName applies D-45. The rules are the intersection of what systemd
// accepts in a unit-instance name and what useradd accepts in an account name,
// because one gvm name becomes both.
//
// Renaming an instance is not supported (D-45), so this runs once per instance
// and the diagnostic it produces is the only chance the operator gets to pick a
// better name.
func ValidateName(s string) error {
	if err := validateNameSyntax(s); err != nil {
		return err
	}
	if why, ok := reserved[s]; ok {
		return fmt.Errorf("%w: %q is reserved: %s", ErrName, s, why)
	}
	return nil
}

// ValidateJobName applies the same charset to a job name, which is what makes
// '_' an unambiguous separator in the slug (D-13): neither half can contain one.
//
// It deliberately does NOT apply the reserved list. Those words are reserved
// because of what an instance name becomes -- a unit, an account, a group --
// and a job name becomes none of those; it becomes half of a slug. Two of them,
// "guard" and "verify", are the host-level job names D-13 itself specifies, so
// sharing the list would forbid the very slugs the register requires.
func ValidateJobName(s string) error {
	if err := validateNameSyntax(s); err != nil {
		return fmt.Errorf("job name: %w", err)
	}
	return nil
}

// validateNameSyntax is the charset, length and readability half of D-45 --
// everything an instance name and a job name have in common.
func validateNameSyntax(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%w: a name is required", ErrName)
	case len(s) < MinNameLen:
		return fmt.Errorf("%w: %q is %d characters; names are %d-%d",
			ErrName, s, len(s), MinNameLen, MaxNameLen)
	case len(s) > MaxNameLen:
		return fmt.Errorf("%w: %q is %d characters; the limit is %d, because useradd caps "+
			"account names at 32 and the gvm- prefix costs 4 (D-45)", ErrName, s, len(s), MaxNameLen)
	}
	if !nameRE.MatchString(s) {
		return fmt.Errorf("%w: %q; names are lowercase %s and may not start or end with '-' (D-45)",
			ErrName, s, "[a-z0-9-]")
	}
	// Rejected separately from the regex because "--" is legal in both systemd
	// and useradd: it is refused because "pz--main" and "pz-main" are one
	// glance apart in a list of units the operator is about to stop.
	if strings.Contains(s, "--") {
		return fmt.Errorf("%w: %q contains \"--\", which is too easy to misread against the same "+
			"name with one dash (D-45)", ErrName, s)
	}
	return nil
}

// MustValidName panics with the reason the name is bad.
//
// Every derived name in this file goes through it. A name reaches here from a
// spec file that was validated at load, so a bad one is a programming error --
// and the failure mode of carrying on is a systemd unit name, a unix account
// name or a filesystem path that gvm did not intend, addressed to something
// that may well exist. Callers taking a name from an operator (the CLI, the
// web tier) call ValidateName and report the error instead.
func MustValidName(name string) {
	if err := ValidateName(name); err != nil {
		panic(fmt.Sprintf("instance: %v; call ValidateName before deriving names from operator input", err))
	}
}

// UnitName is the instance's systemd service unit.
func UnitName(name string) string {
	MustValidName(name)
	return "gvm@" + name + ".service"
}

// SliceName is the slice every instance unit runs in.
const SliceName = "gvm.slice"

// JobSliceName is the slice every job unit runs in.
const JobSliceName = "gvm-jobs.slice"

// JobSlug is the "<instance>_<job>" template parameter of a job unit (D-13).
// A host-level job has an empty instance part: JobSlug("", "guard") is
// "_guard".
func JobSlug(inst, job string) string {
	if inst != "" {
		MustValidName(inst)
	}
	if err := ValidateJobName(job); err != nil {
		panic(fmt.Sprintf("instance: %v", err))
	}
	return inst + "_" + job
}

// JobUnit is the job's systemd service unit (D-13).
func JobUnit(inst, job string) string { return "gvm-job@" + JobSlug(inst, job) + ".service" }

// JobTimer is the timer that triggers JobUnit. Separate from JobUnit because a
// job can exist without a schedule -- `gvm job run` is a verb, not only a
// timer target.
func JobTimer(inst, job string) string { return "gvm-job@" + JobSlug(inst, job) + ".timer" }

// SplitJobSlug is JobSlug's inverse, and the reason the charset excludes '_'.
//
// It is what `gvm job run %i` uses to find out which instance and which job
// systemd just handed it, so it validates both halves: a slug systemd will
// accept is not automatically a slug gvm wrote.
func SplitJobSlug(slug string) (inst, job string, err error) {
	i := strings.IndexByte(slug, '_')
	if i < 0 {
		return "", "", fmt.Errorf("%w: job slug %q has no '_' separating instance from job (D-13)",
			ErrName, slug)
	}
	inst, job = slug[:i], slug[i+1:]
	if strings.IndexByte(job, '_') >= 0 {
		return "", "", fmt.Errorf("%w: job slug %q has more than one '_'; neither an instance "+
			"name nor a job name may contain one (D-13)", ErrName, slug)
	}
	// An empty instance part is how a host-level job is spelled, so it is
	// checked for emptiness rather than validity.
	if inst != "" {
		if err := ValidateName(inst); err != nil {
			return "", "", fmt.Errorf("job slug %q: %w", slug, err)
		}
	}
	if err := ValidateJobName(job); err != nil {
		return "", "", fmt.Errorf("job slug %q: %w", slug, err)
	}
	return inst, job, nil
}

// UserName is the unix account an instance runs as (section 6.4).
//
// Managed instances get this account created for them; an adopted instance
// keeps its own (spec.user), and gvm creates nothing.
func UserName(name string) string {
	MustValidName(name)
	return "gvm-" + name
}

// GroupName is the instance's own group, of which it is the only member. An
// instance user in any shared group is what `gvm doctor` exists to catch: one
// compromised workshop item would reach another world (section 6.4, D-10).
func GroupName(name string) string { return UserName(name) }

// DropInPath is the generated drop-in gvm owns for an instance unit. gvm writes
// exactly this file and never the shipped template; an operator's own
// 90-local.conf beside it is reported by doctor and never touched (section 5.3).
func DropInPath(name string) string {
	MustValidName(name)
	return path.Join(SystemdUnitDir, "gvm@"+name+".service.d", "10-gvm.conf")
}

// JobTimerPath is the generated timer for a job (section 7.5).
func JobTimerPath(inst, job string) string {
	return path.Join(SystemdUnitDir, JobTimer(inst, job))
}
