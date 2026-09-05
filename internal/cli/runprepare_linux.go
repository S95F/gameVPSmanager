//go:build linux

package cli

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// applyPrepare performs the chown and the chmod, in that order.
//
// The order matters: chown clears the setgid bit on Linux (it is part of
// POSIX's "clear set-user-ID and set-group-ID on a successful chown by a
// non-owner" rule, and Linux applies it to directories as well), so a chmod
// that ran first would be silently undone and the socket would come out grouped
// as the instance's own group. gvm-web would then be unable to reach any
// console, with nothing anywhere saying why.
func applyPrepare(t prepareTarget) error {
	uid, err := lookupUID(t.User)
	if err != nil {
		return err
	}
	gid, err := lookupGID(t.Group)
	if err != nil {
		return fmt.Errorf("%w; create it with `groupadd --system %s`", err, t.Group)
	}
	// The directory is systemd's to create (RuntimeDirectory=gvm/%i). Creating
	// it here would work on the first boot and then hide a unit that had lost
	// the directive.
	fi, err := os.Stat(t.Dir)
	if err != nil {
		return fmt.Errorf("%w; the unit's RuntimeDirectory= should have created it", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", t.Dir)
	}
	if err := os.Chown(t.Dir, uid, gid); err != nil {
		return err
	}
	return os.Chmod(t.Dir, t.Mode)
}

// lookupUID and lookupGID go through os/user, which with CGO_ENABLED=0 reads
// /etc/passwd and /etc/group directly. That is the right behaviour for this
// command: it runs as ExecStartPre on a booting unit, and an NSS lookup that
// blocks on a network directory service would hang the unit start rather than
// fail it.
func lookupUID(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}

func lookupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}
