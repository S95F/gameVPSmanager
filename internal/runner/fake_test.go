package runner

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// This file is the reason the supervisor can be developed on Windows.
//
// Everything the runner does to a process goes through the Process interface,
// so a fake that records signals and exits when told exercises the real
// control flow -- the state machine, the pump, the injector, and later the
// shutdown ladder -- on a machine with no /proc, no pty and no kill(2).

// recordedSignal is one delivery, in order.
type recordedSignal struct {
	Sig   platform.Signal
	Scope Scope
}

// fakeProcess is an in-memory Process. Its zero value is not usable; call
// newFakeProcess.
type fakeProcess struct {
	mu       sync.Mutex
	id       Identity
	alive    bool
	aliveErr error
	empty    bool
	emptyErr error
	signals  []recordedSignal
	status   ExitStatus
	done     chan struct{}
	dead     bool

	// exitOn decides which signal ends this process, so a test can model a
	// server that saves on SIGTERM and one that ignores everything until
	// SIGKILL. nil means no signal ever kills it.
	exitOn func(sig platform.Signal, scope Scope) bool
	// signalErr forces a delivery failure, for the rungs that must escalate
	// rather than report success.
	signalErr error
}

var _ Process = (*fakeProcess)(nil)

func newFakeProcess() *fakeProcess {
	return &fakeProcess{
		id:    Identity{PID: 41799, PGID: 41799, StartTicks: 8391244},
		alive: true,
		done:  make(chan struct{}),
	}
}

func (p *fakeProcess) PID() int  { return p.id.PID }
func (p *fakeProcess) PGID() int { return p.id.PGID }

func (p *fakeProcess) Identity() Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.id
}

func (p *fakeProcess) Alive() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive, p.aliveErr
}

func (p *fakeProcess) Empty() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.empty, p.emptyErr
}

func (p *fakeProcess) Signal(sig platform.Signal, scope Scope) error {
	p.mu.Lock()
	p.signals = append(p.signals, recordedSignal{sig, scope})
	if !p.alive {
		p.mu.Unlock()
		return ErrGone
	}
	if p.signalErr != nil {
		err := p.signalErr
		p.mu.Unlock()
		return err
	}
	kill := p.exitOn != nil && p.exitOn(sig, scope)
	p.mu.Unlock()
	if kill {
		p.exit(ExitStatus{Code: -1, Signal: sig, Signaled: true})
	}
	return nil
}

func (p *fakeProcess) Wait(ctx context.Context) (ExitStatus, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.status, nil
	case <-ctx.Done():
		return ExitStatus{}, ctx.Err()
	}
}

// exit ends the fake process, exactly once.
func (p *fakeProcess) exit(st ExitStatus) {
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		return
	}
	p.dead, p.alive, p.empty, p.status = true, false, true, st
	p.mu.Unlock()
	close(p.done)
}

func (p *fakeProcess) sent() []recordedSignal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedSignal(nil), p.signals...)
}

// fakeSpawn returns a SpawnFunc that hands back a fixed process and console,
// recording the Launch it was asked to start.
type fakeSpawn struct {
	proc   Process
	out    io.Reader
	in     io.Writer
	err    error
	closed chan struct{}

	mu      sync.Mutex
	got     []Launch
	closeN  int
	closeOK bool
}

func newFakeSpawn(proc Process, out io.Reader, in io.Writer) *fakeSpawn {
	return &fakeSpawn{proc: proc, out: out, in: in, closed: make(chan struct{}), closeOK: true}
}

func (f *fakeSpawn) fn() SpawnFunc {
	return func(l Launch) (*Spawned, error) {
		f.mu.Lock()
		f.got = append(f.got, l)
		f.mu.Unlock()
		if f.err != nil {
			return nil, f.err
		}
		return &Spawned{Process: f.proc, Out: f.out, In: f.in, Close: f.close}, nil
	}
}

func (f *fakeSpawn) close() error {
	f.mu.Lock()
	f.closeN++
	n := f.closeN
	f.mu.Unlock()
	if n == 1 {
		close(f.closed)
	}
	if !f.closeOK {
		return errors.New("close failed")
	}
	return nil
}

func (f *fakeSpawn) launches() []Launch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Launch(nil), f.got...)
}

func (f *fakeSpawn) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeN
}

// syncWriter is a stdin sink that records what was written, with the locking a
// -race build needs because Send may be called from several goroutines.
type syncWriter struct {
	mu  sync.Mutex
	buf []byte
	err error
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// writes returns the sink's contents. It is deliberately the whole buffer
// rather than a line list: the point of several assertions is that a line and
// its terminator arrived in ONE write, undivided.
func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

func (w *syncWriter) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

// testManifest is a minimal but realistic manifest: a launcher script that
// does not exec, an argument carrying operator input, a conditional secret
// argument, and a pty console.
func testManifest() *spec.Manifest {
	return &spec.Manifest{
		APIVersion: "gvm.dev/v1",
		Kind:       "Game",
		Metadata:   spec.GameMeta{ID: "testgame", Name: "Test Game", Revision: 3},
		SHA256:     "1b40c9de",
		Spec: spec.GameSpec{
			Runtime: spec.RuntimeSpec{
				Exec: "./start-server.sh",
				Cwd:  "{{ .Install }}",
				Args: spec.Args{
					{Value: "-servername"},
					{Value: "{{ .Var.server_name }}"},
					{Value: "-adminpassword", When: &spec.When{Phase: "install"}},
					{Value: "{{ .Secret.admin }}", When: &spec.When{Phase: "install"}},
				},
				Env: map[string]string{
					"HOME": "{{ .Home }}",
					"LANG": "en_US.UTF-8",
				},
				Console: spec.ConsoleSpec{
					Mode:      spec.ConsoleModePTY,
					StripANSI: true,
					EchoInput: true,
				},
				ProcessGroup: true,
			},
		},
	}
}

func testRenderer() *spec.Renderer {
	return spec.NewRenderer(spec.RenderData{
		Name:    "pz-main",
		Game:    "testgame",
		Install: "/opt/pzserver",
		Data:    "/home/pzserver/Zomboid",
		Home:    "/home/pzserver",
		User:    "pzserver",
		Var:     spec.VarValues{"server_name": "My Server"},
		Secret:  spec.SecretValues{"admin": "hunter2"},
		Phase:   spec.PhaseNormal,
	})
}
