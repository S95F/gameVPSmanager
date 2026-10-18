// `gvm gvmd` — the privileged verb gateway (Phase 48 item 4).
//
// It serves internal/gvmd's CLOSED verb set on /run/gvm/admin.sock, calling
// the SAME lifecycle and hostops paths as the root CLI, so a stop from the
// dashboard always walks the ladder (rule 4). At startup it never auto-resumes
// a lease and never actuates the reconciler (rule 6), and it never runs
// migrations (rule 8): it checks AssertCompatible per auth call and refuses
// while the schema is behind.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/gvmd"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/identity"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/secrets"
	"github.com/S95F/gameVPSmanager/internal/socket"
	"github.com/S95F/gameVPSmanager/internal/store"
	"github.com/S95F/gameVPSmanager/internal/store/dbgen/opsq"
)

func runGvmd(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("gvm gvmd", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	sock := fs.String("socket", AdminSocket, "the admin socket to serve")
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if env.Geteuid() != 0 {
		return fmt.Errorf("gvmd is the privileged verb gateway and runs as root " +
			"(systemd's gvmd.service); run: sudo gvm gvmd")
	}

	h, err := env.host(ctx)
	if err != nil {
		return err
	}

	// The master key unlocks the TOTP column. Refusing to start without it
	// beats serving logins that can never complete: the key is created by
	// `sudo gvm host init --dashboard`.
	key, err := secrets.LoadMasterKey(secrets.MasterKeyPath)
	if err != nil {
		return fmt.Errorf("gvmd: %w\n  run: sudo gvm host init --dashboard", err)
	}
	box, err := identity.NewBox([]byte(key.Value.Reveal()))
	if err != nil {
		return fmt.Errorf("gvmd: master key: %w", err)
	}

	// The store probes in the background; gvmd serves with Postgres down
	// (rule 1) and the auth verb answers degraded until it is reachable.
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("gvmd: open store: %w", err)
	}
	defer st.Close()

	backend := &gvmdBackend{host: h, store: st, box: box}

	webUID, err := lookupUID32("gvm")
	if err != nil {
		return fmt.Errorf("gvmd: the web tier's account is missing: %w\n  run: sudo gvm host init", err)
	}

	srv, err := gvmd.NewServer(backend.backend(), gvmd.DefaultIdentifier,
		func(uid uint32) bool { return uid == webUID }, slog.Default())
	if err != nil {
		return err
	}

	// A stale socket from an unclean shutdown refuses the bind.
	_ = os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		return fmt.Errorf("gvmd: listen %s: %w", *sock, err)
	}
	defer ln.Close()
	// 0660 root:gvm — the web tier connects; nobody else can even see a path
	// to it that matters, and the uid check inside the server is the real
	// decision (the group also reads /etc/gvm, which must not mean "holds the
	// verb surface").
	if err := chownGroup(*sock, "gvm"); err != nil {
		return fmt.Errorf("gvmd: %w", err)
	}
	if err := os.Chmod(*sock, 0o660); err != nil {
		return fmt.Errorf("gvmd: chmod %s: %w", *sock, err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(env.Stdout, "gvmd: serving %s\n", *sock)
	return srv.Serve(ctx, ln)
}

// lookupUID32 resolves an account name to its uid (the width SO_PEERCRED speaks).
func lookupUID32(name string) (uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("account %s has uid %q", name, u.Uid)
	}
	return uint32(uid), nil
}

// chownGroup sets path to root:<group>.
func chownGroup(path, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return fmt.Errorf("group %s: %w", group, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("group %s has gid %q", group, g.Gid)
	}
	return os.Chown(path, 0, gid)
}

// ── The backend: gvmd verbs onto the CLI's own paths ─────────────────────────

type gvmdBackend struct {
	host  *HostEnv
	store *store.Store
	box   *identity.Box
}

func (b *gvmdBackend) backend() gvmd.Backend {
	return gvmd.Backend{
		Statuses:    b.statuses,
		Start:       func(ctx context.Context, in string) error { return b.lifecycle(ctx, lifecycle.VerbStart, in) },
		Stop:        func(ctx context.Context, in string) error { return b.lifecycle(ctx, lifecycle.VerbStop, in) },
		Restart:     func(ctx context.Context, in string) error { return b.lifecycle(ctx, lifecycle.VerbRestart, in) },
		ConsoleSend: b.consoleSend,
		BackupNow:   b.backupNow,
		VerifyTOTP:  b.verifyTOTP,
	}
}

// statuses is `gvm list`'s sweep: the observing Reconciler, which cannot
// actuate anything.
func (b *gvmdBackend) statuses(ctx context.Context, instanceName string) ([]lifecycle.Status, error) {
	rc := &lifecycle.Reconciler{Obs: b.host.observer()}
	rep, err := rc.Sweep(ctx)
	if err != nil {
		return nil, err
	}
	all := lifecycle.Statuses(rep)
	if instanceName == "" {
		return all, nil
	}
	for _, st := range all {
		if st.Name == instanceName {
			return []lifecycle.Status{st}, nil
		}
	}
	return nil, nil
}

// lifecycle walks the one Manager path the root CLI walks, as the gvm-web
// actor; a lease refusal surfaces as the typed error the gateway maps to 409.
func (b *gvmdBackend) lifecycle(ctx context.Context, verb lifecycle.Verb, instanceName string) error {
	mgr := &lifecycle.Manager{
		Obs:      b.host.observer(),
		Host:     b.host.Ops,
		Shutdown: b.host.shutdown(),
		Now:      b.host.Now,
	}
	_, err := mgr.Do(ctx, lifecycle.Request{
		Verb:     verb,
		Instance: instanceName,
		Actor:    audit.System(audit.ViaWeb),
	})
	var ref *lifecycle.Refusal
	if errors.As(err, &ref) && strings.HasPrefix(string(ref.Rule), "lease") {
		return &gvmd.ErrLeaseHeld{Line: ref.Reason}
	}
	return err
}

// consoleSend writes one line to the instance's console socket, as the web
// tier's actor name.
func (b *gvmdBackend) consoleSend(ctx context.Context, instanceName, line string) error {
	paths := instance.PathsFor(instanceName)
	client, err := socket.Dial(ctx, paths.ConsoleSock)
	if err != nil {
		return fmt.Errorf("gvmd: console %s: %w", instanceName, err)
	}
	defer client.Close()
	_, err = client.Send("gvm-web", line, nil)
	return err
}

// backupNow starts gvm-job@<instance>_backup.service; the run survives gvmd
// dying because systemd owns it from here.
func (b *gvmdBackend) backupNow(ctx context.Context, instanceName string) error {
	_, err := b.host.Ops.Do(ctx, hostops.Request{
		Verb:     hostops.VerbUnitStart,
		Actor:    audit.System(audit.ViaWeb),
		Instance: instanceName,
		Unit:     hostops.UnitJob,
		Job:      "backup",
	})
	return err
}

// verifyTOTP opens the sealed secret and checks the code, replay-protected by
// last_totp_step. Postgres down or a schema behind the binary answers
// degraded: gvmd never migrates (rule 8).
func (b *gvmdBackend) verifyTOTP(ctx context.Context, email, code string) error {
	select {
	case <-b.store.Ready():
	default:
		return gvmd.ErrDegraded
	}
	pool := b.store.Pool()
	if err := store.NewMigrator(pool).AssertCompatible(ctx); err != nil {
		return fmt.Errorf("%w: %v", gvmd.ErrDegraded, err)
	}

	row, err := opsq.GetTOTP(ctx, pool, email)
	if err != nil {
		if errors.Is(err, opsq.ErrNotFound) {
			// An unknown account answers exactly like a wrong code.
			return identity.ErrInvalidTOTP
		}
		return fmt.Errorf("%w: %v", gvmd.ErrDegraded, err)
	}
	if row.Disabled {
		return identity.ErrInvalidTOTP
	}
	if !row.Enrolled || len(row.Sealed) == 0 {
		return gvmd.ErrTOTPNotEnrolled
	}
	secret, err := b.box.OpenTOTP(row.UserID, row.Sealed)
	if err != nil {
		return fmt.Errorf("gvmd: unseal totp: %w", err)
	}
	newStep, err := identity.VerifyTOTP(secret, code, row.LastStep)
	if err != nil {
		return err
	}
	return opsq.SetLastTOTPStep(ctx, pool, row.UserID, newStep)
}
