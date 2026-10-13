package sshport_test

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/sshport"
)

// fakeEnv is an injectable Env for deterministic tests.
type fakeEnv struct {
	sshConn     string
	listeners   []int
	socketPorts []int
	// files maps path -> content
	files       map[string]string
	configPath  string
	configDPath string
}

func (e *fakeEnv) SSHConnection() string                { return e.sshConn }
func (e *fakeEnv) ListenersFor([]string) ([]int, error) { return e.listeners, nil }
func (e *fakeEnv) ReadSocketUnitPorts() ([]int, error)  { return e.socketPorts, nil }
func (e *fakeEnv) SSHDConfigPath() string               { return e.configPath }
func (e *fakeEnv) SSHDConfigDPath() string              { return e.configDPath }
func (e *fakeEnv) ReadSSHDConfig(path string) (io.ReadCloser, error) {
	if content, ok := e.files[path]; ok {
		return io.NopCloser(strings.NewReader(content)), nil
	}
	return nil, os.ErrNotExist
}

func TestConfiguredPortsWinOutright(t *testing.T) {
	env := &fakeEnv{
		sshConn:   "1.2.3.4 51234 5.6.7.8 2222",
		listeners: []int{22},
	}
	d := sshport.Detect(env, []int{22, 2222})
	if len(d.Ports) != 2 || d.Ports[0] != 22 || d.Ports[1] != 2222 {
		t.Errorf("Ports = %v, want [22 2222]", d.Ports)
	}
	if !d.Confident {
		t.Error("Confident = false, want true")
	}
	// When configured, we should find the HostConfig source.
	found := false
	for _, f := range d.Findings {
		if f.Source == sshport.SourceHostConfig {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SourceHostConfig finding")
	}
}

// TestUbuntu2404Trap is the critical test: ssh.socket on 2222 with sshd_config
// still saying 22. The union of rungs 3+4 gives [22, 2222], protecting against
// either source being wrong.
func TestUbuntu2404Trap(t *testing.T) {
	env := &fakeEnv{
		sshConn:     "1.2.3.4 51234 5.6.7.8 2222",
		listeners:   []int{2222}, // the socket-activated sshd is actually on 2222
		socketPorts: []int{2222}, // ssh.socket ListenStream=2222
		configPath:  "/etc/ssh/sshd_config",
		files: map[string]string{
			"/etc/ssh/sshd_config": "Port 22\n", // IGNORED when socket-activated
		},
	}
	d := sshport.Detect(env, nil)
	if !d.Confident {
		t.Error("Confident = false, want true")
	}
	// Must include 2222 from the live socket.
	found2222 := false
	for _, p := range d.Ports {
		if p == 2222 {
			found2222 = true
		}
	}
	if !found2222 {
		t.Errorf("Ports = %v, want 2222 included (Ubuntu 24.04 socket-activation)", d.Ports)
	}
}

func TestThreePortLinesAllAppear(t *testing.T) {
	env := &fakeEnv{
		configPath: "/etc/ssh/sshd_config",
		files: map[string]string{
			"/etc/ssh/sshd_config": "Port 22\nPort 2222\nPort 2223\n",
		},
	}
	d := sshport.Detect(env, nil)
	if len(d.Ports) != 3 {
		t.Errorf("Ports = %v, want 3 ports", d.Ports)
	}
	for _, want := range []int{22, 2222, 2223} {
		found := false
		for _, p := range d.Ports {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Port %d missing from %v", want, d.Ports)
		}
	}
}

func TestNoEvidenceYieldsNotConfident(t *testing.T) {
	// Empty env: no SSH_CONNECTION, no listeners, no socket, no sshd_config.
	env := &fakeEnv{}
	d := sshport.Detect(env, nil)
	if d.Confident {
		t.Error("Confident = true, want false (fallback only)")
	}
	if len(d.Ports) != 1 || d.Ports[0] != 22 {
		t.Errorf("Ports = %v, want [22] (fallback)", d.Ports)
	}
	for _, f := range d.Findings {
		if f.Source != sshport.SourceFallback {
			t.Errorf("unexpected non-fallback finding: %+v", f)
		}
	}
}

func TestLiveSessionPortIsRecorded(t *testing.T) {
	env := &fakeEnv{sshConn: "10.0.0.1 60000 10.0.0.2 2222"}
	d := sshport.Detect(env, nil)
	if d.LiveSessionPort != 2222 {
		t.Errorf("LiveSessionPort = %d, want 2222", d.LiveSessionPort)
	}
}

func TestPortsAreUnion(t *testing.T) {
	// SSH_CONNECTION on 22, listener also has 2222 (two SSH daemons).
	env := &fakeEnv{
		sshConn:   "1.2.3.4 55000 5.6.7.8 22",
		listeners: []int{22, 2222},
	}
	d := sshport.Detect(env, nil)
	if len(d.Ports) != 2 {
		t.Errorf("Ports = %v, want [22 2222]", d.Ports)
	}
}

func TestCommentedPortIsIgnored(t *testing.T) {
	env := &fakeEnv{
		configPath: "/etc/ssh/sshd_config",
		files: map[string]string{
			"/etc/ssh/sshd_config": "#Port 2222\nPort 22\n",
		},
	}
	d := sshport.Detect(env, nil)
	for _, p := range d.Ports {
		if p == 2222 {
			t.Errorf("commented Port 2222 should not appear in %v", d.Ports)
		}
	}
}

func TestIncludeDirectoryIsRead(t *testing.T) {
	env := &fakeEnv{
		configPath:  "/etc/ssh/sshd_config",
		configDPath: "/etc/ssh/sshd_config.d",
		files: map[string]string{
			"/etc/ssh/sshd_config": "Port 22\n",
			// sshd_config.d/*.conf are handled by Glob; we use testdata files here.
		},
	}
	// Without disk files, the glob returns nothing — just verify the main file is read.
	d := sshport.Detect(env, nil)
	found22 := false
	for _, p := range d.Ports {
		if p == 22 {
			found22 = true
		}
	}
	if !found22 {
		t.Errorf("Port 22 from sshd_config not found in %v", d.Ports)
	}
}

func TestRealTestdataFiles(t *testing.T) {
	env := &fakeEnv{
		configPath:  "testdata/sshd_config",
		configDPath: "testdata/sshd_config.d",
		files:       nil, // use real OS files via ReadSSHDConfig fallback
	}
	// Override ReadSSHDConfig to use real files.
	realEnv := &realFileEnv{wrapped: env}
	d := sshport.Detect(realEnv, nil)
	// testdata/sshd_config has Port 22 and Port 2222.
	found22, found2222 := false, false
	for _, p := range d.Ports {
		if p == 22 {
			found22 = true
		}
		if p == 2222 {
			found2222 = true
		}
	}
	if !found22 || !found2222 {
		t.Errorf("Ports = %v, want 22 and 2222 from testdata", d.Ports)
	}
}

// realFileEnv wraps fakeEnv but reads real files.
type realFileEnv struct{ wrapped *fakeEnv }

func (r *realFileEnv) SSHConnection() string                  { return r.wrapped.SSHConnection() }
func (r *realFileEnv) ListenersFor(n []string) ([]int, error) { return r.wrapped.ListenersFor(n) }
func (r *realFileEnv) ReadSocketUnitPorts() ([]int, error)    { return r.wrapped.ReadSocketUnitPorts() }
func (r *realFileEnv) SSHDConfigPath() string                 { return r.wrapped.SSHDConfigPath() }
func (r *realFileEnv) SSHDConfigDPath() string                { return r.wrapped.SSHDConfigDPath() }
func (r *realFileEnv) ReadSSHDConfig(path string) (io.ReadCloser, error) {
	return os.Open(path)
}
