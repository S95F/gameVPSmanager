// `gvm run` — the per-instance supervisor, and the only command systemd
// invokes as ExecStart (section 11).
//
// This file is wiring and nothing else. Every hard part already exists and is
// tested: the ring and the splitter in internal/console, the peer-credential
// tiers in internal/socket, the pty and the shutdown ladder in internal/runner,
// the verb router in internal/control. What is left is the order in which those
// are brought up and taken down, and that order is itself load-bearing:
//
//   - Preflight before spawn, so a ProtectHome= mistake fails in under a second
//     with an exit code that tells systemd not to retry, instead of silently at
//     the first world write forty minutes later.
//   - The socket before readiness, so `gvm console` can watch a forty-minute
//     Project Zomboid boot rather than being refused until it finishes.
//   - The ladder before Close, always. Close ends the ring, and the ring is how
//     an attached console learns the runner is gone; ending it first would take
//     the operator's window away exactly while the save they are waiting on is
//     happening.
//   - Spawn's error does NOT mean nothing is running. It can fail after exec,
//     and a caller that exits there orphans a live world holding an open file
//     handle on somebody's save.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/control/rconctl"
	"github.com/S95F/gameVPSmanager/internal/control/stdinctl"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/runner"
	"github.com/S95F/gameVPSmanager/internal/secrets"
	"github.com/S95F/gameVPSmanager/internal/socket"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/version"
)

// runActor and runReason are what the unit's own SIGTERM is recorded as.
//
// The shutdown report names an actor for every stop, and "systemd" is the
// honest answer for the one gvm did not initiate: an operator reading
// last-shutdown.json needs to distinguish `systemctl stop` from a dashboard
// button, because only one of those has a person to ask about it.
const (
	runActor  = "system:systemd"
	runReason = "the service manager sent SIGTERM"
)

type runOptions struct {
	Name string
	// GvmPath is the absolute path to this binary, for the console_compat
	// screen bridge. It is a flag because the bridge runs as the INSTANCE user
	// and must not resolve a program through that user's PATH; the default is
	// the packaged location.
	GvmPath string
}

func parseRunArgs(env *Env, args []string) (runOptions, error) {
	var opt runOptions
	flags := flag.NewFlagSet("gvm run", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	flags.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm run <instance>\n\n"+
			"Invoked by gvm@<instance>.service. Running it by hand while the unit is\n"+
			"up is refused by the job lock rather than producing a second server.\n\n")
		flags.PrintDefaults()
	}
	flags.StringVar(&opt.GvmPath, "gvm-path", runner.DefaultGvmPath,
		"absolute path to the gvm binary, for the console_compat screen bridge")
	pos, err := parseInterspersed(flags, args)
	if err != nil {
		return runOptions{}, flagErr(err)
	}
	if len(pos) != 1 {
		flags.Usage()
		return runOptions{}, errors.New("name exactly one instance")
	}
	opt.Name = pos[0]
	if err := instance.ValidateName(opt.Name); err != nil {
		return runOptions{}, err
	}
	return opt, nil
}

func runRun(ctx context.Context, env *Env, args []string) error {
	opt, err := parseRunArgs(env, args)
	if err != nil {
		return err
	}
	if err := requireLinuxHost("gvm run"); err != nil {
		return err
	}
	return supervise(ctx, env, opt)
}

// runSpec is everything `gvm run` reads off disk before it touches the host.
//
// It is a value rather than a sequence of statements inside supervise so the
// whole load — instance file, pinned manifest, the credential ids derived from
// the manifest's own templates, the renderer bound to all of it — is a pure
// function of a path set and an environment, and is therefore tested on the
// development machine against a temporary directory.
type runSpec struct {
	Instance *spec.Instance
	Manifest *spec.Manifest
	Renderer *spec.Renderer

	// CredIDs are the systemd credential ids this instance needs, in sorted
	// order. They are DERIVED from the manifest's `.Secret.<name>` references
	// rather than listed anywhere, because a hand-kept list stops covering a
	// secret the day somebody adds one, and the symptom is an empty
	// RCONPassword in a live config file.
	CredIDs []string
	CredDir string
}

func loadRunSpec(name string, p instance.Paths, getenv func(string) string) (*runSpec, error) {
	in, diags, err := spec.LoadInstance(p.Spec)
	if err != nil {
		return nil, err
	}
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: %s", p.Spec, firstErrorDiag(diags))
	}

	// LoadPinned, not LoadPinnedFor: the pin comparison belongs to preflight,
	// which turns a mismatch into exit 64 and a STATUS= line naming both
	// revisions (D-17). Verifying it here would produce the same refusal with
	// the wrong exit code, and systemd would retry it forever.
	m, err := spec.LoadPinned(p.SpecDir)
	if err != nil {
		return nil, err
	}

	refs, err := manifestRefs(m)
	if err != nil {
		return nil, err
	}
	if bad := refs.hostRefs; len(bad) > 0 {
		// `.Host.RamMB` and `.Host.IP4` are apply-time facts. The runner does
		// not read them, so rendering one here would quietly produce 0 or an
		// empty string in argv — a server sized for nothing, or bound nowhere.
		return nil, fmt.Errorf("%s references %s, which only `gvm apply` can supply; "+
			"a runtime render of it would silently be zero", spec.PinPath(p.SpecDir), strings.Join(bad, ", "))
	}

	credDir := getenv("CREDENTIALS_DIRECTORY")
	sec := spec.SecretValues{}
	ids := make([]string, 0, len(refs.secretNames))
	for _, tmpl := range refs.secretNames {
		id, err := secrets.FromTemplate(tmpl)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", spec.PinPath(p.SpecDir), err)
		}
		ids = append(ids, id)
		// A missing credential is NOT an error here. Preflight reports it as a
		// named, exit-coded refusal listing every id that is absent; failing on
		// the first one would tell an operator about one of three missing
		// secrets per restart.
		if credDir == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(credDir, id))
		if err != nil {
			continue
		}
		sec[tmpl] = strings.TrimRight(string(b), "\r\n")
	}
	sort.Strings(ids)

	return &runSpec{
		Instance: in,
		Manifest: m,
		Renderer: spec.NewRenderer(renderData(name, in, sec)),
		CredIDs:  ids,
		CredDir:  credDir,
	}, nil
}

func renderData(name string, in *spec.Instance, sec spec.SecretValues) spec.RenderData {
	group := in.Spec.Group
	if group == "" {
		group = in.Spec.User
	}
	ports := spec.PortValues{}
	for k, v := range in.Spec.Ports {
		ports[k] = v
	}
	vars := spec.VarValues{}
	for k, v := range in.Spec.Vars {
		vars[k] = v
	}
	content := map[string]spec.ContentValues{}
	for blockName, list := range in.Spec.Content {
		cv := spec.ContentValues{}
		for _, ref := range list.Acquire {
			cv.Acquire = append(cv.Acquire, ref.ID)
		}
		// Activate is deliberately empty: the activation namespace is the
		// resolver's output, not the operator's input, and conflating the two
		// is the bug internal/pz exists to prevent. It arrives with the content
		// lock.
		content[blockName] = cv
	}
	return spec.RenderData{
		Name:    name,
		Game:    in.Spec.Game,
		Install: in.Spec.Paths.Install,
		Data:    in.Spec.Paths.Data,
		Home:    in.Spec.Paths.Home,
		Work:    in.Spec.Paths.Work,
		User:    in.Spec.User,
		Group:   group,
		Port:    ports,
		Var:     vars,
		Secret:  sec,
		Content: content,
		// CPUs is free and true. RamMB and IP4 are deliberately left zero and
		// are refused above rather than rendered, because a wrong number here
		// reaches argv.
		Host:  spec.HostValues{CPUs: runtime.NumCPU()},
		Phase: spec.PhaseNormal,
	}
}

// manifestTemplateRefs is what one scan of a manifest's templates found.
type manifestTemplateRefs struct {
	// secretNames are the TEMPLATE spellings (`rcon_password`), not the store's
	// (`rcon`). secrets.FromTemplate is the one place that difference lives.
	secretNames []string
	hostRefs    []string
}

// manifestRefs walks every templated field in the pinned bytes.
//
// The walk itself is spec.ScanManifestRefs, because `gvm apply` derives the
// LoadCredential= lines it writes from the same scan: two implementations
// would eventually hand the runner a credential id the unit never loaded, and
// the symptom is an empty RCONPassword in a live config file rather than an
// error anywhere.
func manifestRefs(m *spec.Manifest) (manifestTemplateRefs, error) {
	refs, err := spec.ScanManifestRefs(m)
	if err != nil {
		return manifestTemplateRefs{}, err
	}
	// Only the two the runner cannot know. `.Host.CPUs` is answerable here and
	// is answered.
	return manifestTemplateRefs{secretNames: refs.Secrets, hostRefs: refs.RuntimeUnknowable()}, nil
}

func firstErrorDiag(d spec.Diags) string {
	for _, e := range d.Errors() {
		if e.Path != "" {
			return e.Path + ": " + e.Msg
		}
		return e.Msg
	}
	return "invalid"
}

const (
	// socketDrainBudget bounds the wait for a request handler that is mid-op
	// when the runner tears down. It is short because by the time it runs the
	// ladder has already finished; all that is left is the reply frame.
	socketDrainBudget = 2 * time.Second
	// stopWaitMargin is added to the ladder's own budget when the teardown
	// waits for it. The ladder already keeps a reserve back for its report, so
	// this only covers the write.
	stopWaitMargin = 30 * time.Second
	// stopWaitFallback bounds the teardown when the manifest declares no
	// shutdown budget, in which case systemd's TimeoutStopSec is the only
	// other wall. It is a watchdog: a ladder's declared waits are all finite,
	// so it is never expected to fire.
	stopWaitFallback = 15 * time.Minute
)

// stopWaitCtx bounds the teardown's wait for a ladder that is still running.
// It is deliberately not derived from the interrupted context: the whole point
// is to outlive the SIGTERM that started the stop.
func stopWaitCtx(r *runner.Runner) (context.Context, context.CancelFunc) {
	d := r.Plan().Budget.Duration()
	if d <= 0 {
		d = stopWaitFallback
	} else {
		d += stopWaitMargin
	}
	return context.WithTimeout(context.Background(), d)
}

// supervise is the lifecycle of section 11.5, in order.
func supervise(ctx context.Context, env *Env, opt runOptions) error {
	paths := instance.PathsFor(opt.Name)
	// The runner's own diagnostics go to stderr, which under the unit is the
	// journal. The GAME's console deliberately does not: journald drops more
	// than 10 000 messages per 30 s per service and would put holes in exactly
	// the post-mortem an operator is reading (section 11.3).
	log := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("instance", opt.Name)
	notify := platform.NewNotifier()

	// runCtx outlives the SIGTERM that ctx carries, and is cancelled only when
	// this function returns. The console socket and the status loop run on it
	// deliberately: section 11.4's transcript streams `ev:shutdown phase:step`
	// frames to an attached client, and status.json's `rung` field is what a
	// dashboard renders during a ninety second save. Both of those would be
	// gone before the ladder started if they rode the interrupted context.
	runCtx, endRun := context.WithCancel(context.WithoutCancel(ctx))
	defer endRun()

	ld, err := loadRunSpec(opt.Name, paths, env.Getenv)
	if err != nil {
		return runFailure(notify, &runner.Failure{
			Check: "spec", Code: runner.ExitUsage, Status: err.Error(), Err: err,
		})
	}

	r, err := runner.New(runner.Config{
		Instance: opt.Name,
		Game:     ld.Instance.Spec.Game,
		Build:    version.Commit,
		Unit:     instance.UnitName(opt.Name),
		Paths:    paths,
		Manifest: ld.Manifest,
		Renderer: ld.Renderer,
		Logger:   log,
		Notify:   notify,
	})
	if err != nil {
		return runFailure(notify, newFailure(err))
	}

	// Teardown is one closure rather than several defers because its ORDER is
	// the contract, and separate defers unwind in an order set by where they
	// happen to be registered.
	//
	// A running ladder comes first. A socket `stop` blocks its connection for
	// the ladder's whole duration and nothing else here waits for it, while
	// osProcess.Wait is a closed-channel broadcast -- so the ladder's own
	// successful `quit` wakes r.Wait below in the same instant as the ladder's
	// exit watcher, and whoever loses that race would otherwise close the ring
	// the ladder is still annotating and drop the connection before rep.Save
	// has returned. A lost record is the one outcome section 11.6 calls worse
	// than a degraded shutdown.
	//
	// The control channel goes next, not first: a `via: control` rung may still
	// be dialling, and a separately registered defer unwinds before this one.
	// Then the ring, because that is how an attached `gvm console` learns the
	// runner is gone instead of seeing a connection drop it cannot explain; the
	// socket after it; and the world lock last of all, so nothing can claim
	// this world until this runner has finished with it.
	var (
		lock      *platform.FileLock
		srv       *socket.Server
		closeSock func()
		cmd       *control.Commander
		ctrl      control.Channel
	)
	defer func() {
		stopCtx, cancelStop := stopWaitCtx(r)
		if rep := r.AwaitStop(stopCtx); rep != nil {
			log.Info("waited for an in-flight shutdown", "rung", rep.FinalRung, "degraded", rep.Degraded)
		}
		cancelStop()
		if srv != nil {
			srv.Drain(socketDrainBudget)
		}
		if ctrl != nil {
			_ = ctrl.Close()
		}
		if cmd != nil {
			// stdinctl.Close never closes the sink -- closing a game's stdin
			// sends it EOF, which is how a FIFO supervisor kills a server by
			// accident -- so this only drops the channel's own state.
			if cc := cmd.Console(); cc != nil {
				_ = cc.Close()
			}
		}
		if err := r.Close(); err != nil {
			log.Warn("closing the runner", "err", err)
		}
		if closeSock != nil {
			closeSock()
		}
		if lock != nil {
			_ = lock.Unlock()
		}
	}()

	lock, err = runner.Preflight(runner.Request{
		Instance:       opt.Name,
		Manifest:       ld.Manifest,
		Pin:            ld.Instance.Spec.Manifest,
		Exec:           r.Launch().Path,
		InstallDir:     ld.Instance.Spec.Paths.Install,
		DataDir:        ld.Instance.Spec.Paths.Data,
		LogDir:         paths.Logs,
		RunDir:         paths.Run,
		JobLock:        paths.JobLock,
		RunnerJSON:     paths.RunnerJSON,
		Secrets:        ld.CredIDs,
		CredentialsDir: ld.CredDir,
	})
	if err != nil {
		return runFailure(notify, err)
	}

	if err := r.Spawn(); err != nil {
		// Spawn can fail AFTER exec — writing the identity pin is the case that
		// does — and then a game is running with nobody supervising it. The
		// ladder, not os.Exit, is what that situation is owed.
		if r.Process() != nil {
			log.Error("spawn failed after exec; running the shutdown ladder", "err", err)
			r.Shutdown(runCtx, runActor, "the spawn failed after exec")
		}
		return runFailure(notify, newFailure(err))
	}

	cmd, ctrl, err = buildCommander(r, ld, paths, log)
	if err != nil {
		// A control channel that cannot be built is a manifest problem, and the
		// game is already running: stop it cleanly rather than leaving a server
		// nobody can send a verb to.
		r.Shutdown(runCtx, runActor, "the control channel could not be built")
		return runFailure(notify, &runner.Failure{
			Check: "control", Code: runner.ExitUsage, Status: err.Error(), Err: err,
		})
	}
	r.SetControl(ctrl)

	var stopSock func()
	srv, stopSock, err = serveConsole(runCtx, r, cmd, paths.ConsoleSock, log)
	if err != nil {
		r.Shutdown(runCtx, runActor, "the console socket could not be opened")
		return runFailure(notify, newFailure(err))
	}
	closeSock = stopSock
	r.SetConnCounter(srv.Conns)

	go r.StatusLoop(runCtx)

	if err := r.AwaitReady(ctx, portAddr(ld)); err != nil {
		if stopEndedTheBoot(ctx, err) {
			// A stop arrived during the boot. Project Zomboid's first boot is
			// forty minutes long, so this is a real case and it is not a
			// readiness failure -- recording it as one would put a probe's name
			// on an operator's decision, and exit 75, which is absent from
			// RestartPreventExitStatus and restarts the world somebody stopped.
			return finishStop(runCtx, r, log, runReason)
		}
		// The game is still running and is still saveable. Readiness failing is
		// a reason to stop it, not a reason to abandon it.
		log.Error("readiness", "err", err)
		rep := r.Shutdown(runCtx, runActor, "readiness failed: "+err.Error())
		// ExitTempFail retries, which is right for a probe that timed out on a
		// loaded box. StartLimitBurst=5 is what stops it becoming a loop.
		return &ExitError{Code: reportExit(rep, runner.ExitTempFail), Err: err}
	}
	if err := r.StartScreenCompat(opt.GvmPath); err != nil {
		// Never fatal. Refusing to run a game server because a compatibility
		// shim did not start would be the tail wagging the dog.
		log.Warn("console_compat", "err", err)
	}

	// From here the runner has two ways to end: the service manager asks it to
	// stop, or the game exits on its own.
	exit, werr := r.Wait(ctx)
	if werr != nil && ctx.Err() == nil {
		// Not a stop request and not a reaped child: the wait itself failed, so
		// the runner no longer knows what the game is doing. The ladder decides
		// that, not this function.
		log.Error("waiting for the game", "err", werr)
		rep := r.Shutdown(runCtx, runActor, "the runner lost track of the game: "+werr.Error())
		return &ExitError{Code: reportExit(rep, runner.ExitSoftware), Err: werr}
	}
	if werr == nil {
		select {
		case <-r.StopRequested():
			// The game did not exit on its own: a ladder ended it. Reporting
			// the child's non-zero `quit` status as a crash would restart an
			// instance somebody deliberately stopped, and would leave the
			// operator's own report unread.
			return finishStop(runCtx, r, log, runReason)
		default:
		}
		log.Info("the game exited on its own", "status", exit.String())
		if !exit.OK() {
			return &ExitError{Code: exitCodeOf(exit), Err: fmt.Errorf("the game exited %s", exit)}
		}
		return nil
	}
	return finishStop(runCtx, r, log, runReason)
}

// stopEndedTheBoot reports whether a readiness failure is really somebody
// stopping the game while it was still booting.
//
// Two spellings of the same event, because the stop can arrive by either road:
// the service manager cancels the context, or an operator's `stop` over the
// console socket runs the ladder directly. Neither is a probe failing.
func stopEndedTheBoot(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, runner.ErrStopRequested)
}

// finishStop runs the ladder and turns its report into an exit status.
//
// runCtx rather than the interrupted context: ctx is already cancelled -- that
// is what brought us here -- and handing a cancelled context to the ladder
// would abort the save on its first select.
func finishStop(runCtx context.Context, r *runner.Runner, log *slog.Logger, reason string) error {
	// A ladder may already be running: an operator's `gvm stop` over the
	// socket arriving at the same moment as the host's SIGTERM. Waiting for it
	// is the whole job. A second ladder loses the one-at-a-time CAS, gets a
	// refusal back immediately, and would let this process exit while the
	// first one is still saving -- after which KillMode=mixed reaps the world.
	rep := r.AwaitStop(runCtx)
	if rep == nil {
		rep = r.Shutdown(runCtx, runActor, reason)
		if len(rep.Steps) == 0 && rep.Err != "" {
			// Refused. If a ladder won the CAS in between, its report is the
			// one that describes what happened to the world.
			if won := r.AwaitStop(runCtx); won != nil {
				rep = won
			}
		}
	}
	log.Info("shutdown", "rung", rep.FinalRung, "degraded", rep.Degraded, "exit", rep.ExitCode)
	if rep.Degraded || rep.Err != "" {
		return &ExitError{Code: reportExit(rep, runner.ExitSoftware), Err: shutdownError(rep)}
	}
	if rep.ExitCode > 0 {
		// Section 11.5: exit with the child's code. Many games exit non-zero on
		// `quit`, and systemd does not restart after an explicit `systemctl
		// stop` whatever the status, so propagating it costs nothing and is the
		// only way an unexpected death is distinguishable from a clean one.
		return &ExitError{Code: rep.ExitCode}
	}
	return nil
}

// reportExit prefers the game's own status and falls back to a runner code, so
// Restart=on-failure sees the child's answer whenever there is one.
func reportExit(rep *runner.Report, fallback int) int {
	if rep != nil && rep.ExitCode > 0 {
		return rep.ExitCode
	}
	return fallback
}

func shutdownError(rep *runner.Report) error {
	if rep.Err != "" {
		return fmt.Errorf("the shutdown ladder ended at the %s rung: %s", rep.FinalRung, rep.Err)
	}
	return fmt.Errorf("the shutdown ladder reached the %s rung and is recorded as degraded", rep.FinalRung)
}

func exitCodeOf(st runner.ExitStatus) int {
	if st.Signaled {
		// The shell convention, so `systemctl status` and a script agree about
		// what a SIGKILLed server looks like.
		return 128 + int(st.Signal)
	}
	return st.Code
}

// newFailure gives a non-preflight error the exit code its cause deserves.
//
// The split is between "the same bytes will fail the same way" and "the host is
// in the way". A manifest whose ladder cannot be rendered is the first and must
// not be retried; a log directory the sandbox will not let the runner write is
// the second, and is the ProtectHome= mistake section 11.5 gives 78 to.
func newFailure(err error) error {
	var f *runner.Failure
	if errors.As(err, &f) {
		return err
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return &runner.Failure{
			Check: "paths", Code: runner.ExitConfig,
			Status: fmt.Sprintf("%s on %s: %v", pe.Op, pe.Path, pe.Err), Err: err,
		}
	}
	return &runner.Failure{Check: "config", Code: runner.ExitUsage, Status: err.Error(), Err: err}
}

// runFailure reports a refusal the way the unit reads it: a STATUS= line for
// `systemctl status`, and an exit code that decides whether it retries.
func runFailure(n *platform.Notifier, err error) error {
	status := runner.StatusOf(err)
	if status != "" {
		_ = n.Status(status)
	}
	return &ExitError{Code: runner.ExitCode(err), Err: err}
}

// portAddr resolves a manifest port NAME to the address a probe dials.
//
// Always loopback unless the manifest pinned a specific bind. A probe runs on
// the same box as the game, and dialling the public address would test the
// firewall and the NAT rather than the server — turning a correctly running
// instance into a boot that never reaches ready.
func portAddr(ld *runSpec) func(string) (string, error) {
	return func(name string) (string, error) {
		port, ok := ld.Instance.Spec.Ports[name]
		if !ok || port == 0 {
			return "", fmt.Errorf("port %q has no materialised allocation in %s.yaml; run `gvm apply` first",
				name, ld.Instance.Metadata.Name)
		}
		host := "127.0.0.1"
		if ps, ok := ld.Manifest.Spec.PortByName(name); ok {
			switch ps.Bind {
			case "", "0.0.0.0", "::", "*":
			default:
				host = ps.Bind
			}
		}
		return net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
}

// buildCommander wires the two channels and the verb router (section 11.9).
func buildCommander(r *runner.Runner, ld *runSpec, paths instance.Paths, log *slog.Logger) (*control.Commander, control.Channel, error) {
	m := ld.Manifest

	// The console channel is the runner itself: Runner.Send writes the line and
	// its terminator in one write and echoes it into the ring as SourceActor.
	// The gate lives here rather than inside Send, because Send's own refusal
	// is about there being no stream at all.
	consoleCh, err := stdinctl.New(stdinctl.Options{
		Sink: control.SenderFunc(r.Send),
		Gate: func() bool {
			s := r.State().Live()
			return s == spec.StateActivating || s == spec.StateReady
		},
		Reason: "the server is not accepting console input",
	})
	if err != nil {
		return nil, nil, err
	}

	ctrl, err := buildControlChannel(r, ld)
	if err != nil {
		_ = consoleCh.Close()
		return nil, nil, err
	}

	cmd, err := control.New(control.Options{
		Manifest: m,
		Instance: ld.Instance,
		Renderer: ld.Renderer,
		Console:  consoleCh,
		Control:  ctrl,
		Recorder: verbRecorder{r: r, log: log},
		Live:     func() spec.Live { return liveState(r, ld, paths) },
	})
	if err != nil {
		_ = consoleCh.Close()
		_ = ctrl.Close()
		return nil, nil, err
	}
	return cmd, ctrl, nil
}

// buildControlChannel returns the accelerator, or an honest refusal shaped like
// one. It never returns nil: a nil channel has nowhere to carry the sentence,
// and the sentence is the entire product difference between a disabled button
// and a broken one.
func buildControlChannel(r *runner.Runner, ld *runSpec) (control.Channel, error) {
	m := ld.Manifest
	c := m.Spec.Control
	if c == nil {
		return control.None(m.Metadata.Name), nil
	}
	kind := spec.ControlKind(c.Kind)
	if spec.ControlSupport(kind) != spec.SupportImplemented {
		// The sentence comes from spec.Capabilities so the tooltip on the
		// disabled button and the channel's own Unready() are one string.
		return control.Unsupported(kind, controlCapReason(m, ld.Instance)), nil
	}

	addr, err := portAddr(ld)(c.Port)
	if err != nil {
		return nil, err
	}
	pw, _, err := ld.Renderer.Render("spec.control.password", c.Password)
	if err != nil {
		return nil, err
	}
	return rconctl.New(rconctl.Options{
		Addr:     addr,
		Password: secrets.New(pw),
		Dialect:  c.Dialect,
		// Rule 1 of section 11.9: never dial before the game is listening.
		// Project Zomboid does not bind RCON until it has finished booting,
		// which on a modded first boot is forty to sixty minutes of
		// connection-refused in the log an operator will read when something
		// actually goes wrong. The rule lives on the runner because the
		// shutdown ladder depends on the same answer -- see ControlGateOpen.
		Gate:           r.ControlGateOpen,
		NotReadyReason: c.NotReadyReason,
		MinPoll:        c.MinPoll.Duration(),
	})
}

func controlCapReason(m *spec.Manifest, in *spec.Instance) string {
	states := spec.Capabilities(m, in, spec.Live{})
	if s, ok := spec.FindCap(states, spec.CapControl); ok {
		return s.Reason
	}
	return "this build does not implement " + m.Spec.Control.Kind
}

// liveState is the capability input the runner can see.
func liveState(r *runner.Runner, ld *runSpec, paths instance.Paths) spec.Live {
	l := spec.Live{State: r.State().Live()}

	// The file is present iff at least one item is AWAITING_MANUAL_FILES
	// (section 5). Its contents belong to the content subsystem; all the
	// capability table needs from here is whether the count is zero, and
	// guessing a larger number would be inventing a figure the dashboard would
	// print.
	if _, err := os.Stat(paths.BlockedMods); err == nil {
		l.Blocked = 1
	}
	if lg := ld.Manifest.Spec.Legal; lg != nil && lg.EULA != nil {
		l.EULAAccepted = ld.Instance.Spec.Vars[lg.EULA.Var] == "true"
	}
	if p := ld.Manifest.Spec.Presets; p != nil && p.SourceGlob != "" {
		if g, _, err := ld.Renderer.Render("spec.presets.source_glob", p.SourceGlob); err == nil {
			if hits, err := filepath.Glob(g); err == nil {
				l.PresetFiles = len(hits)
			}
		}
	}
	// ControlReady is deliberately not set: Commander.Live always overwrites it
	// from the channel itself, so the capability table and the router cannot
	// disagree about whether RCON is up.
	return l
}

// verbRecorder closes section 11.9's honesty gap.
//
// A CONSOLE verb is already in the ring — Send echoed it as SourceActor, and
// res.Seq is non-zero to say so. A CONTROL verb touches nothing the game
// prints, so without a line of its own an RCON broadcast is invisible in
// console.log forever, and the post-mortem of "who told the server to shut
// down" has no entry for it.
//
// The audit ROW is deliberately not written here. An audit record needs
// actor_kind and actor_via, and only the component that authenticated the
// request knows those; what reaches this recorder is the resolved actor string.
// gvmd writes that row at the boundary where the identity is real (D-09).
type verbRecorder struct {
	r   *runner.Runner
	log *slog.Logger
}

func (v verbRecorder) RecordVerb(actor string, res control.Result, err error) {
	if err != nil {
		v.log.Warn("verb refused", "actor", actor, "verb", res.Verb, "err", err)
		// %q, not %s. A verb NAME arrives over the socket and
		// console.Request.Validate only checks that it is non-blank, so an
		// unknown verb called "broadcast\nSERVER STARTED" would forge lines in
		// the console log -- the record of who typed what, which is precisely
		// what an attacker would want to write for themselves. internal/control
		// already quotes the name inside its own errors; this is the one place
		// that had it raw.
		v.r.Note("-- gvm: refused verb %q from %s: %v", res.Verb, actor, err)
		return
	}
	v.log.Info("verb", "actor", actor, "verb", res.Verb,
		"channel", res.Channel, "elapsed", res.Elapsed)
	if res.Seq != 0 {
		return // the console path already echoed it; a second line would double it
	}
	// res.Command is spec.Redacted when the template carried a secret, and the
	// plaintext was never returned. There is nothing to recover and nothing to
	// try.
	text := res.Command
	if console.ValidateSendLine(text) != nil {
		// It should be impossible to get here: the ladder's plan builder and
		// internal/control both refuse a CR or LF in a rendered command. This
		// is the belt for the braces, because the cost of being wrong is a
		// forged line in the one record that says who did what.
		text = strconv.Quote(text)
	}
	v.r.Ring().Append(console.Line{
		Source: console.SourceActor, Actor: actor, Text: text,
	})
}

// consoleBackend is the runner plus the verb router: exactly socket.Backend.
//
// The runner deliberately does not implement Caps and Verb itself. Both need
// the Commander, the Commander needs a control channel whose gate closes over
// the runner's state, and a runner that constructed its own commander would
// have to know how to build an RCON client — which is how internal/runner would
// end up importing internal/control/rconctl and, through it, a net.Dialer.
type consoleBackend struct {
	*runner.Runner
	cmd *control.Commander
}

func (b consoleBackend) Caps() (json.RawMessage, error) {
	return json.Marshal(b.cmd.Capabilities())
}

func (b consoleBackend) Verb(ctx context.Context, actor, verb string, args map[string]string) (socket.VerbResult, error) {
	// The control gate is deliberately open during `stopping` so the ladder's
	// own `via: control` rungs can run. Nothing else may use it: internal/
	// runner's state graph refuses `stopping -> ready` precisely because a
	// verb executed against a world that is mid-save is the kind of bug that
	// only shows up on the world it destroys.
	if b.Stopping() {
		return socket.VerbResult{}, fmt.Errorf("%w: %w: the server is shutting down",
			socket.ErrBadArgument, control.ErrNotReady)
	}
	res, err := b.cmd.Do(ctx, actor, verb, args)
	if err != nil {
		// control.Rejected covers every refusal that is the CALLER's fault --
		// an unsafe argument, an unknown or reserved verb, a channel that is
		// not ready. Mapping them to bad_request is what lets gvm-web branch
		// between "fix your input" and "the runner is broken".
		if control.Rejected(err) {
			return socket.VerbResult{}, fmt.Errorf("%w: %w", socket.ErrBadArgument, err)
		}
		return socket.VerbResult{}, err
	}
	return socket.VerbResult{Channel: res.Channel, Output: res.Output}, nil
}

// serveConsole opens the console socket and starts serving it.
func serveConsole(ctx context.Context, r *runner.Runner, cmd *control.Commander, sockPath string, log *slog.Logger) (*socket.Server, func(), error) {
	policy, err := socket.DefaultPolicy()
	if err != nil {
		// A host with no gvm-console group yields a POLICY THAT STILL WORKS —
		// it simply has no console tier, so gvm-web cannot attach. That is a
		// doctor finding, not a reason to refuse to run a game server.
		log.Warn("console socket policy", "err", err)
	}
	if err := policy.Validate(); err != nil {
		return nil, nil, err
	}

	l, err := socket.Listen(sockPath)
	if err != nil {
		return nil, nil, err
	}
	srv, err := socket.New(consoleBackend{Runner: r, cmd: cmd}, policy, socket.PeerIdentifier,
		socket.WithLogger(log))
	if err != nil {
		_ = l.Close()
		return nil, nil, err
	}
	go func() {
		if err := srv.Serve(ctx, l); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error("console socket", "err", err)
		}
	}()
	return srv, func() { _ = srv.Close() }, nil
}

// ---------------------------------------------------------------------------
// `gvm run-shutdown` — layer 3 of section 11.8.
//
// It reads the rendered ladder the runner published at spawn and executes it
// with no ring, no socket and no manifest loading, so gvmd or a human can stop
// a game whose supervisor has died. A stale pin is a clean "nothing to stop"
// report rather than an error: the world is already safe, and reporting a
// failure would make an operator go looking for a process that is not there.

func runRunShutdown(ctx context.Context, env *Env, args []string) error {
	var actor, reason string
	flags := flag.NewFlagSet("gvm run-shutdown", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	flags.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm run-shutdown <instance>\n\n"+
			"Runs the ladder recorded in /run/gvm/<instance>/shutdown.json, for when\n"+
			"the runner is gone. Prefer `gvm stop` while it is alive.\n\n")
		flags.PrintDefaults()
	}
	flags.StringVar(&actor, "actor", "root_cli", "attribution recorded in the shutdown report")
	flags.StringVar(&reason, "reason", "standalone shutdown", "why this stop happened")
	pos, err := parseInterspersed(flags, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 1 {
		flags.Usage()
		return errors.New("name exactly one instance")
	}
	name := pos[0]
	if err := instance.ValidateName(name); err != nil {
		return err
	}
	if err := requireLinuxHost("gvm run-shutdown"); err != nil {
		return err
	}

	// Every path root writes to is derived from the name validated above, not
	// read out of the plan: /run/gvm/<name> belongs to the instance user, so a
	// compromised game can replace shutdown.json, and this command runs it as
	// root.
	p := instance.PathsFor(name)
	rep, err := runner.RunShutdown(ctx, runner.StandaloneOptions{
		PlanPath:     p.ShutdownJSON,
		Instance:     name,
		ShutdownLog:  p.ShutdownLog,
		LastShutdown: p.LastShutdown,
		Actor:        actor,
		Reason:       reason,
		// No control channel: dialling RCON needs the instance's secret, which
		// this path deliberately does not load. Every `via: control` rung is
		// then recorded as skipped and the ladder falls through to the console
		// and the signals, which is what the manifests are written to survive.
	})
	if err != nil {
		if errors.Is(err, runner.ErrNoPlan) {
			return fmt.Errorf("%w: %s has not been started by this build of gvm", err, name)
		}
		return err
	}
	printShutdownReport(env, rep)
	if rep.Degraded {
		return &ExitError{Code: runner.ExitTempFail,
			Err: fmt.Errorf("the ladder reached the %s rung; the stop is recorded as degraded", rep.FinalRung)}
	}
	return nil
}

func printShutdownReport(env *Env, rep *runner.Report) {
	if rep == nil {
		return
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintf(env.Stdout, "final rung %s, degraded %v\n", rep.FinalRung, rep.Degraded)
		return
	}
	fmt.Fprintln(env.Stdout, string(b))
}
