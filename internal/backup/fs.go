package backup

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// FS is every filesystem effect this package has that is not the archive
// itself.
//
// It is the same seam hostops.FS is, declared again here for the same reason
// internal/lifecycle and internal/reconcile each declare their own narrow Host
// interface: the interesting half of arming a freeze lease is the path, the
// mode and the owner, and none of that can be exercised against a real
// /run/gvm on the Windows development machine. A fake implementing this
// interface lets the lease's whole derivation -- including that a chown failure
// is FATAL -- be asserted without root and without Linux.
//
// The archive bytes do not go through it. internal/archive owns writing and
// reading those, it already has its own two-pass digest check, and routing a
// 2.4 GB tar through an interface here would be a second archiver with none of
// the first one's guarantees.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// WriteFile replaces path or leaves it alone. The freeze lease is a
	// runtime handshake on tmpfs rather than an operator-editable document,
	// so there is no .bak and no previous version to keep: a stale copy of a
	// file whose whole purpose is to say "a freeze is armed NOW" is the one
	// kind of litter that could be misread as live.
	WriteFile(path string, data []byte, perm fs.FileMode) error
	// WriteFileOwned is WriteFile with the owner set in the SAME publication.
	//
	// It exists because the reader is a different process with a different
	// account. A lease published root-owned 0600 and chowned afterwards is
	// briefly unreadable by the runner, and the runner reads an unreadable
	// lease as a freeze that IS armed -- so it thaws, and then clears the
	// lease that was armed a microsecond ago. An empty owner means "do not
	// chown", which is the armed-without-an-owner case Arm already records.
	WriteFileOwned(path string, data []byte, perm fs.FileMode, owner, group string) error
	// Chown sets the owner by NAME, with an empty group meaning the account's
	// own primary group -- the only correct answer for an adopted instance,
	// whose pzserver account's primary group is pzserver.
	Chown(path, owner, group string) error
	Remove(path string) error
	RemoveAll(path string) error
	MkdirAll(path string, perm fs.FileMode) error
	ReadDir(path string) ([]fs.DirEntry, error)
	DiskUsage(path string) (platform.DiskUsage, error)
}

// OS is the production FS.
var OS FS = osFS{}

type osFS struct{}

func (osFS) Stat(p string) (fs.FileInfo, error)      { return os.Stat(p) }
func (osFS) ReadFile(p string) ([]byte, error)       { return os.ReadFile(p) }
func (osFS) Remove(p string) error                   { return os.Remove(p) }
func (osFS) RemoveAll(p string) error                { return os.RemoveAll(p) }
func (osFS) MkdirAll(p string, m fs.FileMode) error  { return os.MkdirAll(p, m) }
func (osFS) ReadDir(p string) ([]fs.DirEntry, error) { return os.ReadDir(p) }

func (osFS) DiskUsage(p string) (platform.DiskUsage, error) { return platform.Statfs(p) }

// WriteFile and WriteFileOwned are spec's atomic write rather than a second
// implementation of it.
//
// The write has to be atomic because the reader is a different process: a
// runner that read half a lease would see an unparseable document and thaw a
// world nobody had frozen yet. That is the same requirement an instance YAML
// has, so the crash ordering spec's probe points pin down -- write, fsync,
// chmod, rename, fsync the directory -- is the ordering this file wants too,
// and the NoBackup variant is the one that matches the no-.bak reasoning on
// the interface above. internal/hostops' own FS already delegates for the
// same reason, in the same words.
//
// The OWNER is set on the temporary file, beside the mode and before the
// rename, because the lease's reader is not only a different process but a
// different ACCOUNT: a lease published root-owned 0600 and chowned afterwards
// is briefly unreadable by the runner, and the runner reads an unreadable
// lease as a freeze that IS armed -- so it thaws, and then clears the lease
// that was armed a microsecond ago. The name resolution stays here, where the
// account is known; spec takes numbers.
func (osFS) WriteFile(p string, data []byte, perm fs.FileMode) error {
	return spec.WriteFileAtomicNoBackup(p, data, perm)
}

// WriteFileOwned is WriteFile with the owner published atomically with the
// bytes. An empty owner skips the chown.
func (osFS) WriteFileOwned(p string, data []byte, perm fs.FileMode, owner, group string) error {
	uid, gid := -1, -1
	if owner != "" || group != "" {
		var err error
		if uid, gid, err = resolveOwner(owner, group); err != nil {
			// Before anything is created, so a passwd lookup that fails
			// publishes nothing at all.
			return fmt.Errorf("backup: chown %s: %w", p, err)
		}
	}
	return spec.WriteFileAtomicOwned(p, data, perm, uid, gid)
}

// Chown resolves names through os/user, which with CGO_ENABLED=0 reads
// /etc/passwd and /etc/group directly -- the behaviour this package wants,
// because arming a freeze must not block on an NSS lookup against a network
// directory service and every account gvm cares about is local by
// construction (section 6.4).
//
// On Windows os.Chown is unsupported and fails, which is correct rather than
// inconvenient: the one caller treats a failed chown as a failed arm, and
// every test that exercises the arm uses a fake FS precisely so that the
// derivation is asserted without pretending the host did something it did not.
func (osFS) Chown(p, owner, group string) error {
	uid, gid, err := resolveOwner(owner, group)
	if err != nil {
		return fmt.Errorf("backup: chown %s: %w", p, err)
	}
	if uid == -1 && gid == -1 {
		return nil
	}
	return os.Chown(p, uid, gid)
}

// resolveOwner turns an account and group NAME into the numbers chown(2)
// wants, returning -1 for "leave this one alone".
//
// Split out of Chown so that WriteFileOwned can do the lookup BEFORE it
// creates its temporary file: a failed lookup then publishes nothing, rather
// than leaving a file whose owner still has to be fixed.
func resolveOwner(owner, group string) (uid, gid int, err error) {
	uid, gid = -1, -1
	if owner != "" {
		u, lerr := user.Lookup(owner)
		if lerr != nil {
			return -1, -1, lerr
		}
		if uid, err = strconv.Atoi(u.Uid); err != nil {
			return -1, -1, fmt.Errorf("account %s has a non-numeric uid %q", owner, u.Uid)
		}
		if group == "" {
			if gid, err = strconv.Atoi(u.Gid); err != nil {
				return -1, -1, fmt.Errorf("account %s has a non-numeric gid %q", owner, u.Gid)
			}
		}
	}
	if group != "" {
		g, lerr := user.LookupGroup(group)
		if lerr != nil {
			return -1, -1, lerr
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return -1, -1, fmt.Errorf("group %s has a non-numeric gid %q", group, g.Gid)
		}
	}
	return uid, gid, nil
}

// dirOf is filepath.Dir for a path that may be either a slash-separated Linux
// path built by internal/instance or a native one from a test's temp
// directory.
//
// The two have to behave the same here because every production path in this
// package comes from instance.PathsFor -- which builds with path.Join and
// never filepath.Join, so that section 5 renders identically on the Windows
// development machine -- while every test path comes from t.TempDir.
func dirOf(p string) string {
	if strings.ContainsRune(p, filepath.Separator) {
		return filepath.Dir(p)
	}
	return path.Dir(p)
}
