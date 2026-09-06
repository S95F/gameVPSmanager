package hostops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// Account errors. ErrNoSuchAccount is a sentinel because "absent" is the
// normal answer EnsureSystemUser acts on, and distinguishing it from a broken
// account database is the difference between creating a user and creating a
// second one beside a working world.
var (
	ErrNoSuchAccount = errors.New("hostops: no such unix account")
	ErrNoSuchGroup   = errors.New("hostops: no such unix group")
	ErrNoTool        = errors.New("hostops: the account tool is not installed")
)

// Absolute paths to the account tools. No PATH lookup anywhere.
//
// exec.LookPath would decide which binary runs as root from a variable gvm
// inherited from systemd's environment. On a box where that has been tampered
// with -- or merely where /usr/local/sbin shadows /usr/sbin -- gvm would run
// something else entirely with full privileges and record it as `useradd`.
// The candidate lists exist only because the merged-/usr transition left both
// spellings alive on Debian and derivatives.
var (
	useraddPaths  = []string{"/usr/sbin/useradd", "/sbin/useradd"}
	groupaddPaths = []string{"/usr/sbin/groupadd", "/sbin/groupadd"}

	// gpasswd lives in bin rather than sbin on Debian, so it gets its own
	// list. Deriving it from groupadd's directory would have been shorter and
	// would have produced a program path no static check could see.
	gpasswdPaths = []string{"/usr/bin/gpasswd", "/bin/gpasswd", "/usr/sbin/gpasswd"}

	// nologinPaths is an ARGUMENT, not a program this package runs: it becomes
	// the account's login shell. An account whose shell does not exist is
	// locked too, but naming a path the box actually has keeps `getent passwd`
	// readable and keeps a later `usermod -s` -- which this package will never
	// run -- unnecessary.
	nologinPaths = []string{"/usr/sbin/nologin", "/sbin/nologin"}
)

// exitAlreadyInUse is shadow-utils' exit code for "name already in use".
//
// Both useradd and groupadd use 9 for it. It is tolerated rather than treated
// as failure because the alternative is a `gvm apply` that fails the second
// time it is run, and a reconciler nobody dares run twice is not a reconciler.
// It also closes the small race between the lookup and the tool: two applies
// in flight for the same new instance both see "absent", and one of them must
// be allowed to lose.
const exitAlreadyInUse = 9

// toolError is one failed tool invocation, with everything needed to act on it.
type toolError struct {
	Argv   []string
	Code   int
	Stderr string
	Err    error
}

func (e *toolError) Error() string {
	msg := fmt.Sprintf("hostops: %s exited %d", strings.Join(e.Argv, " "), e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

func (e *toolError) Unwrap() error { return e.Err }

// exitCode reports a tool's exit status, or -1 when the failure was not an
// exit at all (the binary was missing, the context was cancelled).
func exitCode(err error) int {
	var te *toolError
	if errors.As(err, &te) {
		return te.Code
	}
	return -1
}

// systemUsers implements platform.Users through shadow-utils.
//
// Every host effect is a seam so that the decisions -- when a tool runs at
// all, what argv it gets, what the tolerated exit codes are -- are testable on
// the Windows development machine, where there is no useradd and no /etc/passwd.
// That matters more here than almost anywhere else in gvm: the one rule this
// type has to keep is "never modify an existing account", and a rule that can
// only be checked on a production host is a rule nobody checks.
type systemUsers struct {
	lookupUser  func(name string) (platform.User, error)
	lookupGroup func(name string) error
	stat        func(path string) (fs.FileInfo, error)
	run         func(ctx context.Context, argv []string) error
}

var _ platform.Users = (*systemUsers)(nil)

// tool resolves the first candidate that exists.
func (s *systemUsers) tool(candidates []string) (string, error) {
	for _, c := range candidates {
		if fi, err := s.stat(c); err == nil && fi.Mode().IsRegular() {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: none of %s exist; install the passwd package",
		ErrNoTool, strings.Join(candidates, ", "))
}

// nologin picks the locked shell to record on a new account, falling back to
// the canonical path when neither candidate is present.
func (s *systemUsers) nologin() string {
	if p, err := s.tool(nologinPaths); err == nil {
		return p
	}
	return nologinPaths[0]
}

func (s *systemUsers) Lookup(_ context.Context, name string) (platform.User, error) {
	return s.lookupUser(name)
}

// EnsureSystemUser creates a locked system account if it is absent and returns
// it either way.
//
// It NEVER modifies an account that exists, and that is the entire contract.
// An adopted instance's account predates gvm: pzserver owns /opt/pzserver and
// /home/pzserver/Zomboid, may be in groups gvm cannot see, may be the account
// a cron job or an sftp login uses. Section 6.4 says gvm creates no user and
// chowns nothing for one. "Ensure" in the configuration-management sense --
// make it match my idea of it -- would rewrite that account's home directory
// and shell on the first apply and take a working server down with no
// diagnosis available, because nothing would have recorded what it used to be.
//
// So there is no usermod anywhere in this package, and no branch that would
// call one. The existing account is returned exactly as found.
func (s *systemUsers) EnsureSystemUser(ctx context.Context, name, home string) (platform.User, error) {
	switch u, err := s.lookupUser(name); {
	case err == nil:
		return u, nil
	case !errors.Is(err, ErrNoSuchAccount):
		// A lookup that failed for any other reason is a broken account
		// database, and creating a user on top of one is how a box ends up
		// with two accounts owning one world.
		return platform.User{}, err
	}

	// The account's own group first: useradd --gid needs it to exist, and a
	// per-instance group of which the instance is the only member is what
	// keeps one compromised workshop item out of every other world (D-10).
	if err := s.EnsureGroup(ctx, name); err != nil {
		return platform.User{}, err
	}

	tool, err := s.tool(useraddPaths)
	if err != nil {
		return platform.User{}, err
	}
	argv := useraddArgv(tool, name, home, s.nologin())
	if err := s.run(ctx, argv); err != nil && exitCode(err) != exitAlreadyInUse {
		return platform.User{}, err
	}

	// Read the account back rather than reporting what was asked for. useradd
	// picks the uid, and an apply that reported a uid it invented would be
	// wrong in exactly the field a later chown depends on.
	u, err := s.lookupUser(name)
	if err != nil {
		return platform.User{}, fmt.Errorf("hostops: %s reported success but %s does not resolve: %w", tool, name, err)
	}
	return u, nil
}

func (s *systemUsers) EnsureGroup(ctx context.Context, name string) error {
	if err := s.lookupGroup(name); err == nil {
		return nil
	} else if !errors.Is(err, ErrNoSuchGroup) {
		return err
	}
	tool, err := s.tool(groupaddPaths)
	if err != nil {
		return err
	}
	if err := s.run(ctx, groupaddArgv(tool, name)); err != nil && exitCode(err) != exitAlreadyInUse {
		return err
	}
	return nil
}

// AddToGroup exists because platform.Users declares it and the packaging needs
// it -- gvm-web joins gvm-console at install time.
//
// It is deliberately NOT a verb. "Add any account to any group" is a complete
// privilege escalation on its own (the sudo group is a group), so exposing it
// on admin.sock would hand the web tier root by a route the enum would report
// as `group.member`. The gap between this method existing and there being no
// verb for it is where the two-daemon split is actually drawn.
func (s *systemUsers) AddToGroup(ctx context.Context, user, group string) error {
	tool, err := s.tool(gpasswdPaths)
	if err != nil {
		return err
	}
	return s.run(ctx, []string{tool, "--add", user, group})
}

// useraddArgv is section 6.4's invocation, verbatim.
//
// --system keeps the uid out of the login range. --no-create-home is what
// makes this safe to run before the directory exists: `dir.ensure` owns
// /srv/gvm/<name> and its mode, and letting useradd create it would produce a
// 0755 home from /etc/login.defs where section 5 says 0750.
//
// Deliberately not DynamicUser=: its uids come from 61184..65519 and change
// across restarts while the world files on disk do not.
func useraddArgv(tool, name, home, shell string) []string {
	return []string{
		tool,
		"--system",
		"--gid", name,
		"--home-dir", home,
		"--no-create-home",
		"--shell", shell,
		"--comment", "gvm instance " + strings.TrimPrefix(name, "gvm-"),
		name,
	}
}

func groupaddArgv(tool, name string) []string {
	return []string{tool, "--system", name}
}
