// `gvm run-prepare` — the thirty lines that make D-09 expressible.
//
// The shipped unit has `RuntimeDirectory=gvm/%i` and `ExecStartPre=+/usr/local/
// bin/gvm run-prepare %i`, the '+' running this one command as root regardless
// of User=. systemd creates /run/gvm/<name> owned by the instance user and
// grouped by the instance's own group, and there is no directive that can make
// it 2750 gvm-<name>:gvm-console — which is what gvm-web needs in order to
// traverse into an instance's runtime directory and reach the console socket
// (section 6.5).
//
// Two properties, and both are load-bearing:
//
//   - The SETGID bit is the whole mechanism. The runner then creates
//     console.sock and status.json inside this directory as an unprivileged
//     user, and they inherit group gvm-console with no further help. Nothing
//     downstream chgrps anything, and nothing downstream needs to be root.
//   - 2750, not 2755. Instance A cannot stat instance B's runtime directory
//     (D-10). The socket's own 0660 is the second lock, and the peer-credential
//     tiers in internal/socket are the third; this is the first.
//
// It reads the instance file for the account name rather than deriving it,
// because an ADOPTED instance keeps the account it already had — pzserver, not
// gvm-pz-main — and preparing the directory for an account the game does not
// run as would leave the runner unable to write its own status file.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/socket"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// RunDirMode is the mode of /run/gvm/<name>: setgid, group-traversable,
// world-nothing.
const RunDirMode fs.FileMode = fs.ModeSetgid | 0o750

// prepareTarget is what run-prepare intends to do, resolved but not yet done.
//
// Splitting the decision from the syscalls is what lets the interesting half —
// which account, which group, which mode — be asserted on the development
// machine, where chown does not exist.
type prepareTarget struct {
	Dir   string
	User  string
	Group string
	Mode  fs.FileMode
}

// resolvePrepare reads the instance definition and decides the ownership.
func resolvePrepare(name string, load func(path string) (*spec.Instance, spec.Diags, error)) (prepareTarget, error) {
	if err := instance.ValidateName(name); err != nil {
		return prepareTarget{}, err
	}
	p := instance.PathsFor(name)
	in, _, err := load(p.Spec)
	if err != nil {
		return prepareTarget{}, err
	}
	user := in.Spec.User
	if user == "" {
		// An instance file with no user is a spec bug, not a licence to guess:
		// chowning the runtime directory to the wrong account produces a runner
		// that cannot write status.json and a socket nobody can reach, both of
		// which are silent.
		return prepareTarget{}, fmt.Errorf("%s names no user; run `gvm apply %s` first", p.Spec, name)
	}
	return prepareTarget{
		Dir:   p.Run,
		User:  user,
		Group: socket.ConsoleGroup,
		Mode:  RunDirMode,
	}, nil
}

func runRunPrepare(_ context.Context, env *Env, args []string) error {
	flags := flag.NewFlagSet("gvm run-prepare", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	flags.Usage = func() { fmt.Fprint(env.Stderr, "Usage: gvm run-prepare <instance>\n") }
	pos, err := parseInterspersed(flags, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 1 {
		flags.Usage()
		return errors.New("name exactly one instance")
	}
	if err := requireLinuxHost("gvm run-prepare"); err != nil {
		return err
	}

	t, err := resolvePrepare(pos[0], spec.LoadInstance)
	if err != nil {
		return err
	}
	if err := applyPrepare(t); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s is %s:%s %04o\n", t.Dir, t.User, t.Group, t.Mode.Perm()|0o2000)
	return nil
}
