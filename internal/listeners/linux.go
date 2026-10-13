//go:build linux

package listeners

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func byName(names []string) ([]Entry, error) {
	nameSet := map[string]bool{}
	for _, n := range names {
		s := n
		if len(s) > 15 {
			s = s[:15]
		}
		nameSet[s] = true
	}

	pids, err := allPIDs()
	if err != nil {
		return nil, err
	}

	// pid -> comm, filtered by name set.
	pidComm := map[int]string{}
	for _, pid := range pids {
		comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			continue
		}
		c := strings.TrimSpace(string(comm))
		if len(names) == 0 || nameSet[c] {
			pidComm[pid] = c
		}
	}
	if len(pidComm) == 0 && len(names) > 0 {
		return nil, nil
	}

	// Socket inode → (pid, comm).
	type owner struct {
		pid  int
		comm string
	}
	inodeOwner := map[string]owner{}
	for pid, comm := range pidComm {
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
			if strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
				inode := target[8 : len(target)-1]
				inodeOwner[inode] = owner{pid, comm}
			}
		}
	}

	var entries []Entry
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		first := true
		for sc.Scan() {
			if first {
				first = false
				continue
			}
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			parts := strings.SplitN(fields[1], ":", 2)
			if len(parts) != 2 {
				continue
			}
			port64, err := strconv.ParseInt(parts[1], 16, 32)
			if err != nil || port64 <= 0 {
				continue
			}
			inode := fields[9]
			o, ok := inodeOwner[inode]
			if !ok && len(inodeOwner) > 0 {
				continue
			}
			entries = append(entries, Entry{Port: int(port64), PID: o.pid, Comm: o.comm})
		}
		fh.Close()
	}
	return entries, nil
}

func allPIDs() ([]int, error) {
	ds, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, d := range ds {
		if d.IsDir() {
			if pid, err := strconv.Atoi(d.Name()); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids, nil
}
