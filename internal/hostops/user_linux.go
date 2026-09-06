//go:build linux

package hostops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// newUsers returns the shadow-utils-backed account seam.
func newUsers() (platform.Users, error) {
	return &systemUsers{
		lookupUser:  lookupSystemUser,
		lookupGroup: lookupSystemGroup,
		stat:        os.Stat,
		run:         runTool,
	}, nil
}

// lookupSystemUser reads the account database.
//
// os/user with CGO_ENABLED=0 parses /etc/passwd directly rather than going
// through NSS. That is what this package wants: an apply must not block on a
// network directory service, and every account gvm models is local by
// construction (section 6.4).
func lookupSystemUser(name string) (platform.User, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return platform.User{}, fmt.Errorf("%w: %s", ErrNoSuchAccount, name)
		}
		return platform.User{}, fmt.Errorf("hostops: lookup user %s: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return platform.User{}, fmt.Errorf("hostops: account %s has a non-numeric uid %q", name, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return platform.User{}, fmt.Errorf("hostops: account %s has a non-numeric gid %q", name, u.Gid)
	}
	// Shell is not in os/user's model. It is left empty rather than guessed:
	// nothing in gvm acts on it, and a fabricated value would look like
	// evidence in a `gvm doctor` report.
	return platform.User{Name: u.Username, UID: uid, GID: gid, Home: u.HomeDir}, nil
}

func lookupSystemGroup(name string) error {
	if _, err := user.LookupGroup(name); err != nil {
		var unknown user.UnknownGroupError
		if errors.As(err, &unknown) {
			return fmt.Errorf("%w: %s", ErrNoSuchGroup, name)
		}
		return fmt.Errorf("hostops: lookup group %s: %w", name, err)
	}
	return nil
}

// runTool executes one account tool.
//
// argv[0] is an absolute path this package resolved by stat; there is no
// shell, no PATH lookup and no string that gets word-split on the way to
// execve. The environment is emptied for the same reason: shadow-utils reads
// nothing gvm needs to pass, and an inherited environment is one more thing
// that decides what a root subprocess does.
func runTool(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	// Exit status 9 ("name already in use") is a normal outcome the caller
	// decides about, so the code has to survive the wrapping.
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	return &toolError{Argv: argv, Code: code, Stderr: stderr.String(), Err: err}
}
