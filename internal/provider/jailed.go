package provider

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The path and identity refusals.
var (
	// ErrPaths is a path set a provider cannot be given: a missing root, or
	// one that does not resolve.
	ErrPaths = errors.New("provider: invalid paths")
	// ErrIdent is an account a provider must not run as.
	ErrIdent = errors.New("provider: invalid run-as identity")
)

// Paths are the four roots a provider may touch, each already proven to be
// where it claims to be.
//
// They are instance.Jailed rather than strings, which has one consequence that
// matters: a plain string will not compile where a destructive or writing
// operation wants a path, and the only way to get a Jailed is to resolve one
// through a jail, which runs EvalSymlinks and re-checks containment. Section
// 10's "a plain string will not compile" is this.
//
// The zero Paths is not usable: Dest refuses it. A composite literal from
// another package can set the four public fields and cannot set the jail, so
// "built by NewPaths" is checkable rather than assumed.
type Paths struct {
	Install, Data, Home, Work instance.Jailed

	// dest is a strict jail rooted at the RESOLVED install path, used to turn
	// an artifact's DestRel into a Jailed. Rooted at the resolved path and not
	// the configured one because strict mode compares a path's lexical and
	// resolved forms: a box whose /srv is a symlink would otherwise have every
	// single destination refused.
	dest *instance.Jail
}

// NewPaths resolves one instance's game paths for a provider.
//
// Work may be empty -- it is SteamCMD's per-instance HOME (D-30) and a
// non-Steam game has none -- and is then left invalid, so a driver that needs
// it finds out by asking rather than by writing into the wrong directory.
// Install, Data and Home are required: the first is where a provider writes,
// the second is the world it must not write into, and the third is where the
// steamclient shim has to live (section 5.2).
func NewPaths(p spec.Paths) (Paths, error) {
	roots := map[string]string{"install": p.Install, "data": p.Data, "home": p.Home}
	for name, v := range roots {
		if strings.TrimSpace(v) == "" {
			return Paths{}, fmt.Errorf("%w: paths.%s is empty", ErrPaths, name)
		}
	}

	allow := []string{p.Install, p.Data, p.Home}
	if p.Work != "" {
		allow = append(allow, p.Work)
	}
	// No protected paths. This jail answers containment -- "is this path
	// inside one of the roots this instance owns" -- and not world safety: the
	// destroy jail is instance.NewJail's job, with backup.world_paths
	// protected, and a jail that protected the world here would refuse to
	// resolve Data itself, since Data is an ancestor of the world.
	j, err := instance.NewJail(instance.JailConfig{Allow: allow})
	if err != nil {
		return Paths{}, fmt.Errorf("%w: %v", ErrPaths, err)
	}

	out := Paths{}
	for _, r := range []struct {
		name string
		in   string
		dst  *instance.Jailed
	}{
		{"install", p.Install, &out.Install},
		{"data", p.Data, &out.Data},
		{"home", p.Home, &out.Home},
		{"work", p.Work, &out.Work},
	} {
		if r.in == "" {
			continue
		}
		jp, err := j.Resolve(r.in)
		if err != nil {
			return Paths{}, fmt.Errorf("%w: paths.%s: %v", ErrPaths, r.name, err)
		}
		*r.dst = jp
	}

	// Strict: a destination that reaches its target through a symlink is
	// refused outright, even one staying inside the tree. The paths this jail
	// judges come from third-party archives and manifests, and zip-slip's
	// second act is a symlink member.
	dest, err := instance.NewJail(instance.JailConfig{
		Allow:  []string{out.Install.Path()},
		Strict: true,
	})
	if err != nil {
		return Paths{}, fmt.Errorf("%w: install jail: %v", ErrPaths, err)
	}
	out.dest = dest
	return out, nil
}

// Dest resolves one artifact's destination inside the install tree.
//
// This is the second of zip-slip's two doors. Artifact.Validate closed the
// lexical one at plan time; this one asks the filesystem, which is the only
// thing that knows that install/mods is a symlink to /home/someone-else.
func (p Paths) Dest(rel string) (instance.Jailed, error) {
	if p.dest == nil {
		return instance.Jailed{}, fmt.Errorf("%w: these Paths did not come from NewPaths, so "+
			"nothing has resolved the install root", ErrPaths)
	}
	if err := validDestRel(rel); err != nil {
		return instance.Jailed{}, err
	}
	return p.dest.Resolve(rel)
}

// Populated reports whether a tree has anything in it at all.
//
// One directory entry, not a walk: this answers "has an install ever started
// here", it is called on trees holding tens of thousands of files, and the
// question it is NOT allowed to answer is "is this installed" -- that is the
// manifest's sentinel (SentinelResult), because 40 GB of a 65 GB download is
// neither empty nor installed.
func Populated(root instance.Jailed) (bool, error) {
	if !root.Valid() {
		return false, fmt.Errorf("%w: unresolved path", ErrPaths)
	}
	d, err := os.Open(root.Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("provider: open %s: %w", root.Path(), err)
	}
	defer d.Close()

	names, err := d.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("provider: read %s: %w", root.Path(), err)
	}
	return len(names) > 0, nil
}

// UnixIdent is the account a provider's work runs as (D-16).
//
// The fields are unexported and the only constructor refuses root, because the
// zero value of a struct of ints is uid 0 -- the one identity third-party code
// must never run as, and the one a forgotten assignment produces. SteamCMD
// self-updates and executes Valve-supplied code; running that as root is the
// regression D-16 exists to prevent, so "nobody filled this in" has to be a
// refusal rather than a privilege escalation.
type UnixIdent struct {
	user  string
	group string
	uid   int
	gid   int
}

// NewUnixIdent names the account, refusing root and the unset value.
func NewUnixIdent(user, group string, uid, gid int) (UnixIdent, error) {
	switch {
	case strings.TrimSpace(user) == "":
		return UnixIdent{}, fmt.Errorf("%w: no user name", ErrIdent)
	case strings.TrimSpace(group) == "":
		return UnixIdent{}, fmt.Errorf("%w: no group name", ErrIdent)
	case uid <= 0:
		return UnixIdent{}, fmt.Errorf("%w: uid %d; third-party code never runs as root, and "+
			"an unset uid is root (D-16)", ErrIdent, uid)
	case gid <= 0:
		return UnixIdent{}, fmt.Errorf("%w: gid %d; an unset gid is the root group (D-16)",
			ErrIdent, gid)
	}
	return UnixIdent{user: user, group: group, uid: uid, gid: gid}, nil
}

// Valid reports whether this identity came from NewUnixIdent.
func (u UnixIdent) Valid() bool { return u.user != "" && u.uid > 0 && u.gid > 0 }

func (u UnixIdent) User() string  { return u.user }
func (u UnixIdent) Group() string { return u.group }
func (u UnixIdent) UID() int      { return u.uid }
func (u UnixIdent) GID() int      { return u.gid }

func (u UnixIdent) String() string {
	if !u.Valid() {
		return "<unset>"
	}
	return fmt.Sprintf("%s:%s (%d:%d)", u.user, u.group, u.uid, u.gid)
}
