package ports

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Listener is a socket found in /proc/net, attributed to a process where the
// running user is allowed to see one.
//
// PID is 0 and Comm empty when the owning process could not be identified,
// which is the ordinary case for a socket owned by another user when gvm is not
// running as root. That is reported honestly rather than hidden: "a foreign
// listener whose owner is not visible" is still a listener, and the allocator
// must move around it either way.
type Listener struct {
	Endpoint Endpoint
	Addr     netip.Addr
	Inode    uint64
	UID      uint32
	PID      int
	Comm     string

	// Owner is the gvm instance this socket belongs to, once somebody who
	// knows the instance-to-pid mapping has stamped it (AttributeListeners).
	// Empty means "not ours, or not known to be ours", and an empty owner
	// always blocks.
	Owner string
}

// Describe renders the listener the way an exhaustion report needs it.
func (l Listener) Describe() string {
	if l.Owner != "" {
		return fmt.Sprintf("gvm@%s (pid %d %s)", l.Owner, l.PID, l.Comm)
	}
	if l.PID > 0 {
		return fmt.Sprintf("a foreign listener (pid %d %s)", l.PID, l.Comm)
	}
	return fmt.Sprintf("a foreign listener on %s (uid %d, socket inode %d; the owning process is not visible from this one)",
		l.Addr, l.UID, l.Inode)
}

// AttributeListeners stamps an owner on every listener whose pid is known to
// belong to a gvm instance.
//
// It is a separate step because this package has no way to know the mapping:
// the pid of a running server comes from runner.json, which the lifecycle layer
// owns. Without it an instance's own live socket looks like a foreign listener,
// which is harmless during allocation (it blocks a port that is already the
// instance's own) but reads as a false conflict in `gvm doctor`.
func AttributeListeners(ls []Listener, byPID map[int]string) []Listener {
	out := make([]Listener, len(ls))
	copy(out, ls)
	for i := range out {
		if name, ok := byPID[out[i].PID]; ok && out[i].PID > 0 {
			out[i].Owner = name
		}
	}
	return out
}

// ProcSource is the seam over /proc. The Linux implementation reads the real
// thing; a test supplies a fake, which is why the inode-to-pid logic below can
// be exercised on a machine with no /proc.
type ProcSource interface {
	// NetFile opens one of "tcp", "tcp6", "udp", "udp6". A protocol table that
	// does not exist -- /proc/net/tcp6 on a host booted with ipv6.disable=1 --
	// must return an error satisfying fs.ErrNotExist, not an empty reader.
	NetFile(name string) (io.ReadCloser, error)

	// PIDs lists the process ids currently visible.
	PIDs() ([]int, error)

	// SocketInodes lists the socket inodes pid holds open.
	SocketInodes(pid int) ([]uint64, error)

	// Comm is the process's command name.
	Comm(pid int) (string, error)
}

// netTables is the fixed set of socket tables, and the protocol each belongs
// to. Both address families are read for both protocols: a server bound to ::
// on a dual-stack host accepts IPv4 traffic on the same port and therefore
// occupies it, and reading only the v4 table is how a port that is very
// obviously in use gets handed out anyway.
var netTables = []struct {
	name  string
	proto Proto
}{
	{"tcp", TCP},
	{"tcp6", TCP},
	{"udp", UDP},
	{"udp6", UDP},
}

// CollectListeners reads the four socket tables and attributes each bound
// socket to a process.
//
// A missing table is tolerated (an IPv6-less host has no tcp6) but all four
// missing is an error: "no listeners found" and "the listener tables could not
// be read" produce the same empty slice and mean opposite things, and only one
// of them is safe to allocate on top of.
//
// Failure to attribute a socket is not an error. Sockets owned by other users
// are invisible in /proc unless gvm runs as root, and refusing to allocate
// because of that would refuse on every correctly configured host.
func CollectListeners(src ProcSource) ([]Listener, error) {
	var (
		out     []Listener
		missing []string
	)
	for _, t := range netTables {
		f, err := src.NetFile(t.name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				missing = append(missing, t.name)
				continue
			}
			return nil, fmt.Errorf("ports: open %s socket table: %w", t.name, err)
		}
		entries, err := ParseProcNet(f, t.proto)
		cerr := f.Close()
		if err != nil {
			return nil, fmt.Errorf("ports: %s: %w", t.name, err)
		}
		if cerr != nil {
			return nil, fmt.Errorf("ports: close %s socket table: %w", t.name, cerr)
		}
		for _, e := range entries {
			if !e.Bound() {
				continue
			}
			out = append(out, Listener{
				Endpoint: e.Endpoint(),
				Addr:     e.Local.Addr(),
				Inode:    e.Inode,
				UID:      e.UID,
			})
		}
	}
	if len(missing) == len(netTables) {
		return nil, fmt.Errorf("ports: none of the socket tables %s could be read; occupancy is unknown, not empty",
			strings.Join(missing, ", "))
	}

	if err := attribute(out, src); err != nil {
		return nil, err
	}
	sortListeners(out)
	return out, nil
}

// attribute walks the process table once and fills in pid and comm for the
// sockets whose inodes it can see.
//
// One walk, not one per socket: a busy host has thousands of processes and
// tens of thousands of descriptors, and doing this per listener turns a
// millisecond into a minute.
func attribute(ls []Listener, src ProcSource) error {
	if len(ls) == 0 {
		return nil
	}
	want := make(map[uint64][]int, len(ls))
	for i, l := range ls {
		want[l.Inode] = append(want[l.Inode], i)
	}

	pids, err := src.PIDs()
	if err != nil {
		// The process table being unreadable is a host problem worth naming;
		// individual processes vanishing under us is not.
		return fmt.Errorf("ports: list processes: %w", err)
	}
	for _, pid := range pids {
		inodes, err := src.SocketInodes(pid)
		if err != nil {
			// A process that exited between the directory listing and this
			// read is the normal case, not an incident.
			continue
		}
		var comm string
		var haveComm bool
		for _, in := range inodes {
			idx, ok := want[in]
			if !ok {
				continue
			}
			if !haveComm {
				comm, _ = src.Comm(pid)
				haveComm = true
			}
			for _, i := range idx {
				// The first process seen holding the inode wins. A socket
				// shared across a fork has several equally true answers and
				// picking one deterministically beats reporting a list.
				if ls[i].PID == 0 {
					ls[i].PID = pid
					ls[i].Comm = comm
				}
			}
		}
	}
	return nil
}

func sortListeners(ls []Listener) {
	sort.Slice(ls, func(i, j int) bool {
		if ls[i].Endpoint != ls[j].Endpoint {
			return ls[i].Endpoint.less(ls[j].Endpoint)
		}
		if ls[i].Addr != ls[j].Addr {
			return ls[i].Addr.String() < ls[j].Addr.String()
		}
		return ls[i].Inode < ls[j].Inode
	})
}

// ParseSocketLink pulls the inode out of a /proc/<pid>/fd/<n> link target.
//
// The kernel writes "socket:[18443]". Every other target -- a regular file, a
// pipe, an anon_inode -- is not a socket and is skipped, which is why this
// reports found rather than erroring: a process holding an open file is not a
// malformed anything.
func ParseSocketLink(target string) (uint64, bool) {
	const prefix = "socket:["
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "]") {
		return 0, false
	}
	n, err := strconv.ParseUint(target[len(prefix):len(target)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// DirProcSource reads a procfs mounted at Root.
//
// Root is a parameter rather than a hardcoded "/proc" for two reasons: a test
// can point it at a fixture tree, and a container-aware caller can point it at
// a host procfs bind-mounted somewhere else. Nothing here executes anything --
// see noshell_test.go, which enforces that.
type DirProcSource struct {
	Root string
}

// NewDirProcSource builds a source over root, defaulting to /proc.
func NewDirProcSource(root string) DirProcSource {
	if root == "" {
		root = "/proc"
	}
	return DirProcSource{Root: root}
}

func (s DirProcSource) NetFile(name string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(s.Root, "net", name))
}

func (s DirProcSource) PIDs() ([]int, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Only the all-numeric directories are processes; /proc/net and
		// /proc/sys are not, and neither is /proc/self.
		n, err := strconv.Atoi(e.Name())
		if err != nil || n <= 0 {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

func (s DirProcSource) SocketInodes(pid int) ([]uint64, error) {
	dir := filepath.Join(s.Root, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			// One descriptor closing mid-walk is routine.
			continue
		}
		if in, ok := ParseSocketLink(target); ok {
			out = append(out, in)
		}
	}
	return out, nil
}

func (s DirProcSource) Comm(pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.Root, strconv.Itoa(pid), "comm"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
