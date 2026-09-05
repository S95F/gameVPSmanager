package platform

import (
	"bytes"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Signal is the closed set of signals gvm is willing to send. It mirrors
// spec.knownSignals: a manifest declares "SIGINT" as a string and this is what
// that string resolves to. The numbers are the Linux ones, written out rather
// than taken from package syscall, because syscall's constants are not the
// same set on the development machine and a seam that changes meaning with
// GOOS is not a seam.
type Signal int

const (
	SIGHUP  Signal = 1
	SIGINT  Signal = 2
	SIGQUIT Signal = 3
	SIGKILL Signal = 9
	SIGUSR1 Signal = 10
	SIGUSR2 Signal = 12
	SIGTERM Signal = 15
)

var signalNames = map[Signal]string{
	SIGHUP:  "SIGHUP",
	SIGINT:  "SIGINT",
	SIGQUIT: "SIGQUIT",
	SIGKILL: "SIGKILL",
	SIGUSR1: "SIGUSR1",
	SIGUSR2: "SIGUSR2",
	SIGTERM: "SIGTERM",
}

func (s Signal) String() string {
	if n, ok := signalNames[s]; ok {
		return n
	}
	return "signal " + strconv.Itoa(int(s))
}

// Known reports whether s is one of the signals gvm will send.
func (s Signal) Known() bool { _, ok := signalNames[s]; return ok }

// ParseSignal resolves a manifest's signal name.
//
// One spelling only, the SIG-prefixed one spec's validator accepts. Neither
// the bare form nor the number is allowed: a manifest is the only thing that
// names a signal, "9" hides the most destructive rung of the shutdown ladder
// behind a digit, and a second accepted spelling means two ladders that behave
// identically diff against each other.
func ParseSignal(name string) (Signal, error) {
	n := strings.ToUpper(strings.TrimSpace(name))
	for sig, sn := range signalNames {
		if sn == n {
			return sig, nil
		}
	}
	return 0, fmt.Errorf("platform: %q is not a signal gvm sends", name)
}

// Stat is the part of /proc/<pid>/stat gvm pins a process identity on.
//
// StartTicks is field 22, in clock ticks since boot. Together with the pid it
// is the identity: a pid can be recycled within seconds on a busy box, and
// signalling a recycled pid during a shutdown ladder means signalling an
// innocent process instead of the game.
type Stat struct {
	PID        int
	Comm       string
	State      string
	PPID       int
	PGID       int
	StartTicks uint64
}

// ParseStat decodes one /proc/<pid>/stat record.
//
// THE TRAP: field 2 is the comm, wrapped in parentheses, and the kernel does
// not escape it. A process named "Web Content" prints "(Web Content)" and one
// named "evil) 1 2 3" prints "(evil) 1 2 3)". Splitting the whole line on
// whitespace therefore reads a field 22 that the process itself chose -- and
// the (pid, starttime) pin silently matches a process that is not the game.
// The comm is the only field that can contain a ')', so scanning to the LAST
// ')' is exact, and everything after it is a fixed, space-separated table.
func ParseStat(b []byte) (Stat, error) {
	open := bytes.IndexByte(b, '(')
	closed := bytes.LastIndexByte(b, ')')
	if open < 0 || closed < open {
		return Stat{}, fmt.Errorf("platform: malformed /proc/<pid>/stat: no parenthesised comm in %q", clip(b))
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(b[:open])))
	if err != nil {
		return Stat{}, fmt.Errorf("platform: malformed /proc/<pid>/stat: field 1: %w", err)
	}

	comm := string(b[open+1 : closed])
	// TASK_COMM_LEN is 16 including the NUL, so the kernel cannot emit a
	// longer comm. A longer one here means the last ')' was not the comm's
	// closer and every field index below is off, which must fail loudly
	// rather than yield a plausible wrong starttime.
	if len(comm) > 15 {
		return Stat{}, fmt.Errorf("platform: malformed /proc/<pid>/stat: comm %q is %d bytes, over the kernel's 15", comm, len(comm))
	}

	// Field 3 (state) is at index 0 here, so field N is at index N-3.
	f := strings.Fields(string(b[closed+1:]))
	const startTicksIdx = 22 - 3
	if len(f) <= startTicksIdx {
		return Stat{}, fmt.Errorf("platform: malformed /proc/%d/stat: %d fields after comm, want at least %d", pid, len(f), startTicksIdx+1)
	}

	st := Stat{PID: pid, Comm: comm, State: f[0]}
	if st.PPID, err = strconv.Atoi(f[4-3]); err != nil {
		return Stat{}, fmt.Errorf("platform: malformed /proc/%d/stat: field 4 (ppid): %w", pid, err)
	}
	if st.PGID, err = strconv.Atoi(f[5-3]); err != nil {
		return Stat{}, fmt.Errorf("platform: malformed /proc/%d/stat: field 5 (pgrp): %w", pid, err)
	}
	if st.StartTicks, err = strconv.ParseUint(f[startTicksIdx], 10, 64); err != nil {
		return Stat{}, fmt.Errorf("platform: malformed /proc/%d/stat: field 22 (starttime): %w", pid, err)
	}
	return st, nil
}

// ParseStartTicks is ParseStat reduced to the pinned field.
func ParseStartTicks(b []byte) (uint64, error) {
	st, err := ParseStat(b)
	return st.StartTicks, err
}

// ParsePIDs decodes a cgroup.procs body: one decimal pid per line.
func ParsePIDs(b []byte) ([]int, error) {
	var out []int
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("platform: cgroup.procs: %q is not a pid", line)
		}
		if pid <= 0 {
			return nil, fmt.Errorf("platform: cgroup.procs: %d is not a pid", pid)
		}
		out = append(out, pid)
	}
	return out, nil
}

// ParseSelfCgroup extracts the unified-hierarchy path from a /proc/<pid>/cgroup
// body. Lines are "hierarchy-id:controllers:path"; the cgroup v2 entry is the
// one with hierarchy 0 and no controllers.
//
// A v1-only host is an error rather than a fallback: gvm's whole resource model
// is v2 unified (MemoryMax, the gvm.slice tree), so a v1 host must fail in
// doctor with a sentence an operator can act on, not degrade into signalling
// the wrong set of pids.
func ParseSelfCgroup(b []byte) (string, error) {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			return "", fmt.Errorf("platform: /proc/self/cgroup: malformed line %q", line)
		}
		if parts[0] == "0" && parts[1] == "" {
			if !strings.HasPrefix(parts[2], "/") {
				return "", fmt.Errorf("platform: /proc/self/cgroup: v2 path %q is not absolute", parts[2])
			}
			return parts[2], nil
		}
	}
	return "", fmt.Errorf("platform: /proc/self/cgroup: no cgroup v2 entry; this host is not running the unified hierarchy")
}

const cgroupRoot = "/sys/fs/cgroup"

// cgroupProcsPath maps a cgroup path as /proc/self/cgroup reports it onto the
// cgroup.procs file that lists its members. It is deliberately not
// filepath.Join: the result names a Linux file, and on the Windows development
// machine filepath.Join would produce backslashes that no test could catch.
func cgroupProcsPath(rel string) (string, error) {
	if !strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("platform: cgroup path %q is not absolute", rel)
	}
	// Checked before Clean, not after: path.Clean("/../../etc") is "/etc",
	// so cleaning first turns a caller's malformed path into a different
	// perfectly valid one and the mistake is never reported. /proc/self/cgroup
	// never emits a ".." segment, so one here is always a constructed path.
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("platform: cgroup path %q contains a %q segment", rel, "..")
		}
	}
	return path.Join(cgroupRoot, rel, "cgroup.procs"), nil
}

// checkPID rejects the pids that turn kill(2) into something other than
// "signal this process": 0 signals the caller's own process group, negative
// values signal a group, and 1 is init.
//
// It exists because the cgroup sweep of section 11.6's last rung signals pids
// it read out of a file, and a malformed read that yielded 0 would have the
// runner SIGKILL itself and every process it shares a group with.
func checkPID(pid int) error {
	switch {
	case pid <= 0:
		return fmt.Errorf("platform: %d is not a pid; SignalProcess never signals a group", pid)
	case pid == 1:
		return fmt.Errorf("platform: refusing to signal pid 1")
	}
	return nil
}

// checkPGID rejects the two process-group ids that turn kill(-pgid) into a
// weapon: 0 signals the caller's own group, which for the runner means killing
// itself and the game together and losing the shutdown report, and 1 signals
// every process the uid may signal, which on a root runner is the whole box.
func checkPGID(pgid int) error {
	switch {
	case pgid < 0:
		return fmt.Errorf("platform: process group %d is not a pgid; SignalGroup negates it itself", pgid)
	case pgid == 0:
		return fmt.Errorf("platform: refusing to signal process group 0: kill(-0) targets gvm's own group")
	case pgid == 1:
		return fmt.Errorf("platform: refusing to signal process group 1: kill(-1) targets every process on the host")
	}
	return nil
}

func clip(b []byte) string {
	const max = 80
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
