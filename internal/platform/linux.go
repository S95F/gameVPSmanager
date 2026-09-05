//go:build linux

package platform

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"sort"

	"golang.org/x/sys/unix"
)

// Statfs reports the space on the filesystem holding path. It is the
// pre-destroy and pre-archive preflight: an archive that runs out of room
// halfway leaves a truncated tarball where a verified one was promised, and
// gvm destroys nothing it has not archived first.
func Statfs(path string) (DiskUsage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskUsage{}, fmt.Errorf("platform: statfs %s: %w", path, err)
	}
	bs := uint64(st.Bsize)
	return DiskUsage{
		Total: uint64(st.Blocks) * bs,
		Free:  uint64(st.Bfree) * bs,
		Avail: uint64(st.Bavail) * bs,
	}, nil
}

// ephemeralPath is the sysctl the port allocator must consult. Read as a file
// rather than through a sysctl call so the parser takes bytes and can be
// tested anywhere.
const ephemeralPath = "/proc/sys/net/ipv4/ip_local_port_range"

// EphemeralRange reads the kernel's outbound port range. A listener bound
// inside it works until the day an outbound connection grabs the port first,
// and then fails to start with no change to anything the operator did --
// which is why section 6.7 makes this one of five occupancy sources.
func EphemeralRange() (PortRange, error) {
	b, err := os.ReadFile(ephemeralPath)
	if err != nil {
		return PortRange{}, fmt.Errorf("platform: %s: %w", ephemeralPath, err)
	}
	return ParsePortRange(b)
}

// UserExists reports whether a unix account exists.
//
// It answers only that question. Creating the account is hostops' business and
// modifying one is nobody's: an adopted instance's user predates gvm and may
// be shared with things gvm cannot see.
func UserExists(name string) (bool, error) {
	if _, err := user.Lookup(name); err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return false, nil
		}
		return false, fmt.Errorf("platform: lookup user %s: %w", name, err)
	}
	return true, nil
}

// GroupList returns the names of every group a user belongs to, primary
// included, sorted so the result is a stable set to compare against.
//
// `gvm doctor` asserts that an instance user is in exactly its own group
// (section 6.4): membership in gvm, gvm-console or gvm-content would let one
// compromised workshop item reach the control socket or another world.
func GroupList(name string) ([]string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("platform: lookup user %s: %w", name, err)
	}
	gids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("platform: group list for %s: %w", name, err)
	}
	out := make([]string, 0, len(gids))
	for _, gid := range gids {
		g, err := user.LookupGroupId(gid)
		if err != nil {
			// A gid with no group entry is a real state on a host where a
			// group was deleted out from under a user. Reporting the number
			// keeps doctor's comparison honest instead of dropping a
			// membership the operator needs to see.
			out = append(out, gid)
			continue
		}
		out = append(out, g.Name)
	}
	sort.Strings(out)
	return out, nil
}
