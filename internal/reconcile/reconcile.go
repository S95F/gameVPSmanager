// Package reconcile is `gvm apply`: the single write path from an instance
// spec to a host.
//
// Everything gvm changes about a box passes through here on its way to
// internal/hostops, and the shape of that journey is the point. A plan is
// built first and is pure -- it renders every byte, decides every ordering and
// can be printed -- and only then is it executed. That split is what makes
// `gvm apply --diff` honest: the diff is not a description of what apply
// intends to do, it is the plan apply will run, and there is no second code
// path that could do something else.
//
// Three properties are structural rather than documented:
//
//   - A destructive Action takes a *archive.Ref. It is a parameter of the
//     interface method, so an implementation that wants to remove anything
//     must thread it through to instance.Destroy, which will not compile
//     without one (testdata/action-no-ref and testdata/destroy-without-ref
//     are the fixtures that prove it).
//   - The write-probe runs BEFORE the drop-in is written (D-15). Plan.Validate
//     refuses a plan in which a drop-in write is not preceded by a probe of
//     the same instance, and Apply calls Validate before executing anything.
//     A ProtectHome/ReadWritePaths combination that would leave the game
//     unable to write its own save directory is therefore caught while
//     nothing has changed, rather than at 3am on a restart.
//   - One Sandbox value produces both the drop-in's confinement directives
//     and the transient unit's properties, so the probe cannot check a
//     sandbox other than the one that gets written.
//
// What this package does NOT do: it never calls systemd, never touches a file
// and never creates an account itself. Every effect is a hostops.Request. That
// is not layering for its own sake -- hostops is the closed privileged surface
// (its verb table is the enumerable list of everything gvm can do as root),
// and a reconciler with its own os.WriteFile would put a write outside it.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/hostops"
)

// The package's refusals. Each is a sentinel because the operator's next move
// differs: a plan-shape failure is a gvm bug, a probe failure is a sandbox the
// operator can widen, and an unsafe value is something they typed.
var (
	// ErrPlan means the plan itself is malformed -- most often a drop-in
	// write that no probe precedes. It is a programming error in the plan
	// builder and is reported as one rather than fixed up silently.
	ErrPlan = errors.New("reconcile: the plan does not hold together")

	// ErrUnsafeValue is a value that would not survive being written into a
	// unit file as a directive: a newline, a carriage return or any other
	// control character. Systemd directives are literal to end of line, so a
	// newline inside one is not a formatting problem, it is a second
	// directive the operator did not write.
	ErrUnsafeValue = errors.New("reconcile: value cannot be written into a unit file")

	// ErrNoArchive is a destructive action reached with nothing to authorise
	// it. It is the runtime half of archive-before-destroy; the compile-time
	// half is the *archive.Ref parameter on Action.Do.
	ErrNoArchive = errors.New("reconcile: a destructive step needs a verified archive")
)

// Host is the privileged surface reconcile drives, and it is exactly
// hostops.Do.
//
// It is an interface with one method so that the plan can be executed against
// a recorder in a test and against the root daemon in production, and so that
// nothing in this package can reach a privileged operation by another route:
// there is no second method to add one to.
type Host interface {
	Do(ctx context.Context, req hostops.Request) (hostops.Result, error)
}

// maxUnitValue bounds a single directive value.
//
// systemd's own line limit is generous but finite, and a value long enough to
// approach it came from somewhere other than a person typing a path. Refusing
// early names the field; letting it through produces a unit file the manager
// truncates with a message nobody reads.
const maxUnitValue = 4096

// checkUnitValue refuses anything that cannot appear after `Directive=`.
//
// This is the one validator standing between an operator-typed string in
// /etc/gvm/instances/<name>.yaml and a systemd unit file. `OnCalendar` is the
// clearest case: a schedule containing "\nExecStart=/bin/sh -c ..." would
// otherwise render as two directives, and the second one runs as root.
func checkUnitValue(field, v string) error {
	if len(v) > maxUnitValue {
		return fmt.Errorf("%w: %s is %d bytes, over the %d-byte ceiling",
			ErrUnsafeValue, field, len(v), maxUnitValue)
	}
	for i, r := range v {
		// Everything below space, plus DEL. Tab is included: systemd strips
		// leading whitespace on continuation lines, and a value that means
		// something different depending on where the parser wraps is a value
		// nobody can review.
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s contains %q at byte %d; a systemd directive is "+
				"literal to end of line, so this would be a second directive",
				ErrUnsafeValue, field, r, i)
		}
	}
	return nil
}

// joinPaths renders a space-separated directive list.
//
// A space is systemd's separator for ReadWritePaths=, so a path CONTAINING a
// space would silently become two paths. Systemd's own answer is quoting;
// gvm's is to refuse, because a game directory with a space in it is a thing
// an operator can rename and a quoting rule is a thing that has to be right
// forever.
func joinPaths(field string, paths []string) (string, error) {
	for i, p := range paths {
		if err := checkUnitValue(fmt.Sprintf("%s[%d]", field, i), p); err != nil {
			return "", err
		}
		if strings.ContainsAny(p, " \t\"'\\") {
			return "", fmt.Errorf("%w: %s[%d] = %q contains a space or a quote; "+
				"systemd separates this directive's paths on whitespace",
				ErrUnsafeValue, field, i, p)
		}
	}
	return strings.Join(paths, " "), nil
}

// dedupe keeps the first occurrence of each string, preserving order.
//
// Order is semantic here rather than cosmetic: the drop-in lists install, then
// data, then the derived directories, which is the order an operator reads
// them in, and sorting would put /var before /opt and bury the two paths that
// matter. Duplicates arise honestly -- an adopted instance whose data lives
// inside its install tree -- and a repeated path in ReadWritePaths is noise a
// reviewer has to explain.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
