// Package sshport detects the port(s) sshd is actually listening on.
//
// This is the single most safety-critical computation in gvm: it is what stops
// `gvm firewall enable` from locking the operator out of their own VPS.
//
// The six-rung ladder, highest confidence first:
//
//  1. spec.firewall.ssh.ports — explicit operator override, wins outright
//  2. SSH_CONNECTION — the live session's port: if we're connected on it, it works
//  3. Listening socket — /proc/net/tcp{,6} mapped to sshd PIDs (pure, no exec)
//  4. ssh.socket ListenStream= — socket activation (Ubuntu 22.10+/24.04)
//  5. sshd_config Port lines — ALL of them, not just the first
//  6. 22 — fallback; sets Confident=false
//
// Ports is the UNION of every non-fallback source. Allowing only the "best"
// source is how you lock out the session that is not the one you are typing in.
package sshport

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Source describes where a detected port came from.
type Source string

const (
	SourceHostConfig  Source = "gvm.yaml spec.firewall.ssh.ports"
	SourceLiveSession Source = "SSH_CONNECTION"
	SourceListener    Source = "listening socket"
	SourceSocketUnit  Source = "ssh.socket ListenStream"
	SourceSSHDConfig  Source = "sshd_config"
	SourceFallback    Source = "default"
)

// Finding is one piece of evidence from one source.
type Finding struct {
	Source Source
	Port   int
	Detail string // e.g. "sshd pid 812"
}

// Detection is the result of running the ladder.
type Detection struct {
	// Ports is the UNION of every non-fallback source, sorted.
	Ports           []int
	LiveSessionPort int
	Findings        []Finding
	// Confident is false when the only evidence is SourceFallback. gvm still
	// stages rules, but refuses to enable a deny-by-default firewall.
	Confident bool
}

// Env is the external surface the detector reads.
// Replacing it allows deterministic tests without touching os.Getenv or /proc.
type Env interface {
	// SSHConnection returns the value of SSH_CONNECTION, empty if unset.
	SSHConnection() string
	// ListenersFor returns ports sshd/sshd.service/ssh.socket is bound to.
	ListenersFor(names []string) ([]int, error)
	// ReadSocketUnitPorts returns ListenStream= values from ssh.socket.
	ReadSocketUnitPorts() ([]int, error)
	// ReadSSHDConfig returns an io.ReadCloser for the given path.
	ReadSSHDConfig(path string) (io.ReadCloser, error)
	// SSHDConfigPath is the main sshd_config path.
	SSHDConfigPath() string
	// SSHDConfigDPath is the Include-glob directory (commonly sshd_config.d/).
	SSHDConfigDPath() string
}

// Detect runs the full six-rung ladder.
// configuredPorts is spec.firewall.ssh.ports; empty means autodetect.
func Detect(env Env, configuredPorts []int) Detection {
	var d Detection
	seen := map[int]bool{}

	add := func(src Source, port int, detail string) {
		d.Findings = append(d.Findings, Finding{Source: src, Port: port, Detail: detail})
		if !seen[port] {
			seen[port] = true
		}
	}

	// Rung 1: explicit config — wins outright.
	if len(configuredPorts) > 0 {
		for _, p := range configuredPorts {
			add(SourceHostConfig, p, "")
		}
		d.Ports = sorted(seen)
		d.Confident = true
		return d
	}

	// Rung 2: SSH_CONNECTION "client_ip client_port server_ip server_port"
	if conn := env.SSHConnection(); conn != "" {
		parts := strings.Fields(conn)
		if len(parts) >= 4 {
			if port, err := strconv.Atoi(parts[3]); err == nil && port > 0 {
				d.LiveSessionPort = port
				add(SourceLiveSession, port, conn)
			}
		}
	}

	// Rung 3: live listening sockets (/proc/net/tcp{,6} mapped to PIDs).
	if ports, err := env.ListenersFor([]string{"sshd", "sshd.service"}); err == nil {
		for _, p := range ports {
			add(SourceListener, p, "sshd")
		}
	}

	// Rung 4: ssh.socket ListenStream= (Ubuntu socket-activation).
	// IMPORTANT: on Ubuntu 22.10+ sshd is socket-activated and sshd_config's
	// Port= is IGNORED. An operator who moved SSH to 2222 the systemd way
	// would get a rule for 22 and a locked box if we trusted sshd_config first.
	if ports, err := env.ReadSocketUnitPorts(); err == nil {
		for _, p := range ports {
			add(SourceSocketUnit, p, "ssh.socket")
		}
	}

	// Rung 5: sshd_config Port lines — ALL of them, not just the first.
	sshdPorts := readSSHDConfigPorts(env)
	for _, p := range sshdPorts {
		add(SourceSSHDConfig, p, "")
	}

	// Rung 6: fallback.
	if len(seen) == 0 {
		add(SourceFallback, 22, "")
		d.Confident = false
	} else {
		d.Confident = true
	}

	d.Ports = sorted(seen)
	return d
}

// readSSHDConfigPorts reads ALL Port= directives from the main config and
// any files in the sshd_config.d Include directory.
func readSSHDConfigPorts(env Env) []int {
	var ports []int
	seen := map[int]bool{}

	read := func(rc io.ReadCloser) {
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// Handle both "Port 22" and "Port=22" (sshd_config.d may use either).
			kv := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == '=' })
			if len(kv) == 2 && strings.EqualFold(kv[0], "Port") {
				if p, err := strconv.Atoi(kv[1]); err == nil && p > 0 && p <= 65535 && !seen[p] {
					seen[p] = true
					ports = append(ports, p)
				}
			}
		}
	}

	if rc, err := env.ReadSSHDConfig(env.SSHDConfigPath()); err == nil {
		read(rc)
	}

	// Include glob: sshd_config.d/*.conf (the Ubuntu cloud-init convention).
	dpath := env.SSHDConfigDPath()
	if dpath != "" {
		matches, err := filepath.Glob(filepath.Join(dpath, "*.conf"))
		if err == nil {
			for _, m := range matches {
				if rc, err := env.ReadSSHDConfig(m); err == nil {
					read(rc)
				}
			}
		}
	}

	return ports
}

func sorted(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// OS returns an Env backed by the real operating system.
// On non-Linux the env stubs return nothing; callers see Confident=false.
func OS() Env { return osEnv{} }

// osEnv reads from real files; the Linux vs stub split is in env_{linux,stub}.go.
type osEnv struct{}

func (osEnv) SSHConnection() string                          { return os.Getenv("SSH_CONNECTION") }
func (osEnv) SSHDConfigPath() string                         { return "/etc/ssh/sshd_config" }
func (osEnv) SSHDConfigDPath() string                        { return "/etc/ssh/sshd_config.d" }
func (osEnv) ReadSSHDConfig(p string) (io.ReadCloser, error) { return os.Open(p) }
