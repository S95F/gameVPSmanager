//go:build linux

package sshport

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ListenersFor returns the ports that processes named in `names` are listening on,
// by parsing /proc/net/tcp and /proc/net/tcp6 (pure, no exec, no iproute2).
func (osEnv) ListenersFor(names []string) ([]int, error) {
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}

	// Collect PIDs of matching processes.
	pids, err := procsMatchingNames(nameSet)
	if err != nil || len(pids) == 0 {
		return nil, err
	}

	// Collect socket inodes belonging to those PIDs.
	inodes, err := inodesForPIDs(pids)
	if err != nil || len(inodes) == 0 {
		return nil, err
	}

	// Parse /proc/net/tcp and tcp6 for LISTEN rows matching those inodes.
	var ports []int
	seen := map[int]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		ps, _ := listenPortsInFile(f, inodes)
		for _, p := range ps {
			if !seen[p] {
				seen[p] = true
				ports = append(ports, p)
			}
		}
	}
	return ports, nil
}

// procsMatchingNames returns PIDs of processes whose comm matches any name in the set.
func procsMatchingNames(names map[string]bool) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		// comm is truncated to 15 bytes; match with or without .service suffix.
		for n := range names {
			base := n
			if i := strings.Index(n, "."); i >= 0 {
				base = n[:i]
			}
			if len(base) > 15 {
				base = base[:15]
			}
			if name == base {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

// inodesForPIDs returns all socket inodes held open by the given PIDs.
func inodesForPIDs(pids []int) (map[string]bool, error) {
	inodes := map[string]bool{}
	for _, pid := range pids {
		fdDir := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			// socket:[inode]
			if strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
				inodes[target[8:len(target)-1]] = true
			}
		}
	}
	return inodes, nil
}

// listenPortsInFile parses a /proc/net/tcp or tcp6 file for LISTEN rows.
// Format: sl  local_addr:port  remote_addr  st  ...  inode
// st=0A is TCP_LISTEN; local_addr is hex big-endian for tcp6, little-endian for tcp.
func listenPortsInFile(path string, inodes map[string]bool) ([]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var ports []int
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // skip header
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		// st is fields[3]; 0A = TCP_LISTEN
		if fields[3] != "0A" {
			continue
		}
		// local_addr is fields[1]: "hex_ip:hex_port"
		parts := strings.SplitN(fields[1], ":", 2)
		if len(parts) != 2 {
			continue
		}
		port64, err := strconv.ParseInt(parts[1], 16, 32)
		if err != nil || port64 <= 0 {
			continue
		}
		// inode is fields[9]
		if len(inodes) > 0 && !inodes[fields[9]] {
			continue
		}
		ports = append(ports, int(port64))
	}
	return ports, sc.Err()
}

// ReadSocketUnitPorts returns ListenStream= values from ssh.socket.
func (osEnv) ReadSocketUnitPorts() ([]int, error) {
	// Try both the override and the original unit location.
	paths := []string{
		"/etc/systemd/system/ssh.socket.d/override.conf",
		"/lib/systemd/system/ssh.socket",
		"/usr/lib/systemd/system/ssh.socket",
	}
	var ports []int
	seen := map[int]bool{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(strings.ToLower(line), "listenstream=") {
				continue
			}
			val := strings.TrimSpace(line[len("ListenStream="):])
			// ListenStream= can be bare port, ip:port, or socket path.
			portStr := val
			if i := strings.LastIndex(val, ":"); i >= 0 {
				portStr = val[i+1:]
			}
			if p, err := strconv.Atoi(portStr); err == nil && p > 0 && !seen[p] {
				seen[p] = true
				ports = append(ports, p)
			}
		}
		f.Close()
	}
	return ports, nil
}
