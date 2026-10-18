package hostinit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/S95F/gameVPSmanager/internal/secrets"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/version"
	"github.com/S95F/gameVPSmanager/packaging"
)

// versionString is the version recorded in the first-init marker, so an
// operator reading host-init.json can see which build set up the box.
func versionString() string { return version.Version }

// System is the set of host effects `Run` performs, named so the orchestration
// is driven and tested without a real host.
//
// It is one interface rather than free functions for the reason the rest of
// gvm keeps its privileged surface behind one: the interesting thing about an
// install is the ORDER it touches the host in — users before the units that
// run as them, files before the reload that reads them, the reload before the
// enable — and an order is only observable when every touch goes to the same
// recorder. The Linux implementation (system_linux.go) is the real thing; the
// non-Linux stub refuses, so the whole package still compiles into the
// cross-platform `make check`.
type System interface {
	// Exists reports whether an absolute path is present on the host.
	Exists(path string) (bool, error)
	// ReplaceBinary installs the currently running executable at dest, by
	// writing a sibling and rename(2)ing it into place so a live runner keeps
	// the old inode and there is no "text file busy".
	ReplaceBinary(dest string) error
	// WriteRoot writes data to an absolute path, root-owned, at mode, via a
	// temp file and rename so a reader never sees a half-written file.
	WriteRoot(path string, data []byte, mode uint32) error
	// Sysusers applies the sysusers.d configuration (creates the accounts).
	Sysusers(ctx context.Context) error
	// Tmpfiles applies one tmpfiles.d file (creates its directories).
	Tmpfiles(ctx context.Context, path string) error
	// ReloadDaemon reloads systemd so it sees the units just written.
	ReloadDaemon(ctx context.Context) error
	// EnableUnit installs a unit's [Install] symlinks. It does not start it.
	EnableUnit(ctx context.Context, unit string) error
}

// Options controls one run.
type Options struct {
	// Diff prints every write and changes nothing. It is the operator's look
	// before the leap, and the converged-box check the DoD asserts prints 0
	// changes.
	Diff bool
	// Dashboard also installs the control plane (DashboardManifest: gvmd,
	// gvm-web, the migrate oneshot, the caddy-group tmpfiles), creates
	// master.key if absent, and prints the postgresql setup lines. It starts
	// nothing (Phase 48 item 3).
	Dashboard bool
}

// marker is written to MarkerPath after the first successful init. Its presence
// — not its contents — is what makes timer enablement first-init-only; the
// fields are there for an operator reading the file to see when and by what.
type marker struct {
	FirstInitAt time.Time `json:"first_init_at"`
	Version     string    `json:"version"`
}

// Run installs, or (with Diff) reports, the host state this build expects.
//
// It is idempotent: running it twice converges, and the second run reports
// "nothing to do" for files that already match. The steps are ordered so a
// failure part-way leaves a host that is no worse than before each step —
// accounts and directories first (nothing depends on gvm yet), then the
// binary, then the units and a single reload.
func Run(ctx context.Context, sys System, out io.Writer, opt Options) error {
	files := Manifest()
	if opt.Dashboard {
		files = append(files, DashboardManifest()...)
	}

	if opt.Diff {
		return diff(ctx, sys, out, files, opt)
	}

	fmt.Fprintln(out, "gvm host init")

	// 1. The binary itself, at the one path the units name.
	if err := sys.ReplaceBinary(BinaryPath); err != nil {
		return fmt.Errorf("install %s: %w", BinaryPath, err)
	}
	fmt.Fprintf(out, "  binary    %s\n", BinaryPath)

	// 2. Every packaged file. Writing them before the reload means systemd sees
	//    a complete set, never half of one.
	for _, f := range files {
		data, err := packaging.FS.ReadFile(f.Src)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", f.Src, err)
		}
		if err := sys.WriteRoot(f.Dest, data, uint32(f.Mode)); err != nil {
			return fmt.Errorf("write %s: %w", f.Dest, err)
		}
		fmt.Fprintf(out, "  file      %s\n", f.Dest)
	}

	// 3. Accounts, then directories. systemd-sysusers before systemd-tmpfiles
	//    because a tmpfiles line names an owner by name and would fail for an
	//    account that does not exist yet.
	if err := sys.Sysusers(ctx); err != nil {
		return fmt.Errorf("systemd-sysusers: %w", err)
	}
	fmt.Fprintln(out, "  accounts  systemd-sysusers applied")
	for _, f := range files {
		if f.Kind != KindTmpfiles {
			continue
		}
		if err := sys.Tmpfiles(ctx, f.Dest); err != nil {
			return fmt.Errorf("systemd-tmpfiles %s: %w", f.Dest, err)
		}
		fmt.Fprintf(out, "  dirs      systemd-tmpfiles applied (%s)\n", f.Dest)
	}

	// 4. One reload, now that the whole unit set is on disk.
	if err := sys.ReloadDaemon(ctx); err != nil {
		return fmt.Errorf("systemd daemon-reload: %w", err)
	}
	fmt.Fprintln(out, "  systemd   reloaded")

	// 5. Timers, on the first init only. The marker is what makes it "only":
	//    an operator who disables gvm-guard.timer next week must not have it
	//    re-enabled by the next `host init`.
	firstInit, err := sys.Exists(MarkerPath)
	if err != nil {
		return fmt.Errorf("check %s: %w", MarkerPath, err)
	}
	if !firstInit {
		for _, t := range FirstTimers() {
			if err := sys.EnableUnit(ctx, t); err != nil {
				return fmt.Errorf("enable %s: %w", t, err)
			}
			fmt.Fprintf(out, "  timer     %s enabled (first init)\n", t)
		}
		m, _ := json.MarshalIndent(marker{FirstInitAt: time.Now().UTC(), Version: versionString()}, "", "  ")
		if err := sys.WriteRoot(MarkerPath, append(m, '\n'), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", MarkerPath, err)
		}
	} else {
		fmt.Fprintln(out, "  timers    left as configured (not first init)")
	}

	// 6. A commented default config, only if the operator has none. gvm must
	//    never overwrite an edited /etc/gvm/gvm.yaml.
	hasConfig, err := sys.Exists(ConfigPath)
	if err != nil {
		return fmt.Errorf("check %s: %w", ConfigPath, err)
	}
	if !hasConfig {
		h := spec.DefaultHostConfig()
		data, err := spec.MarshalYAML(&h)
		if err != nil {
			return fmt.Errorf("render default host config: %w", err)
		}
		if err := sys.WriteRoot(ConfigPath, data, uint32(spec.ModeSpec)); err != nil {
			return fmt.Errorf("write %s: %w", ConfigPath, err)
		}
		fmt.Fprintf(out, "  config    %s written from defaults\n", ConfigPath)
	} else {
		fmt.Fprintf(out, "  config    %s kept (already present)\n", ConfigPath)
	}

	// 7. The dashboard extras: the master key (0400 root, created once and
	//    never rewritten — rewriting it would orphan every sealed TOTP secret)
	//    and the postgresql guidance. Units are installed above with the rest;
	//    nothing is started (the operator enables gvmd and gvm-web themselves).
	if opt.Dashboard {
		hasKey, err := sys.Exists(secrets.MasterKeyPath)
		if err != nil {
			return fmt.Errorf("check %s: %w", secrets.MasterKeyPath, err)
		}
		if !hasKey {
			key := make([]byte, secrets.MasterKeySize)
			if _, err := rand.Read(key); err != nil {
				return fmt.Errorf("generate master key: %w", err)
			}
			if err := sys.WriteRoot(secrets.MasterKeyPath, key, uint32(secrets.MasterKeyMode)); err != nil {
				return fmt.Errorf("write %s: %w", secrets.MasterKeyPath, err)
			}
			fmt.Fprintf(out, "  key       %s created (0400 root; losing it costs every TOTP enrolment)\n",
				secrets.MasterKeyPath)
		} else {
			fmt.Fprintf(out, "  key       %s kept (already present)\n", secrets.MasterKeyPath)
		}
		printDashboardHints(out)
		printAptHints(out)
		fmt.Fprintln(out, "\nnext: sudo gvm db migrate")
		return nil
	}

	printAptHints(out)
	fmt.Fprintln(out, "\nnext: sudo gvm doctor")
	return nil
}

// printDashboardHints prints what gvm will not do for the operator: the apt
// packages and the postgresql peer-auth lines (printed, never run — D-41's
// spirit applies to Postgres too).
func printDashboardHints(out io.Writer) {
	fmt.Fprintln(out, "\nThe dashboard needs postgresql and (for caddy_managed) Caddy — gvm installs neither:")
	fmt.Fprintln(out, "  sudo apt-get install postgresql")
	fmt.Fprintln(out, "  # Caddy, from its official repository: https://caddyserver.com/docs/install#debian-ubuntu-raspbian")
	fmt.Fprintln(out, "\nCreate the database and its roles once:")
	fmt.Fprintln(out, "  sudo -u postgres psql -c \"CREATE ROLE gvm_migrate LOGIN; CREATE ROLE gvm_ops LOGIN; CREATE ROLE gvm_web LOGIN;\"")
	fmt.Fprintln(out, "  sudo -u postgres psql -c \"CREATE DATABASE gvm OWNER gvm_migrate;\"")
	fmt.Fprintln(out, "\nPeer auth, in /etc/postgresql/*/main/pg_hba.conf — insert above `local all all peer`:")
	fmt.Fprintln(out, "  local  gvm  gvm_migrate  peer map=gvm")
	fmt.Fprintln(out, "  local  gvm  gvm_ops      peer map=gvm")
	fmt.Fprintln(out, "  local  gvm  gvm_web      peer map=gvm")
	fmt.Fprintln(out, "and in pg_ident.conf:")
	fmt.Fprintln(out, "  gvm  root  gvm_migrate")
	fmt.Fprintln(out, "  gvm  root  gvm_ops")
	fmt.Fprintln(out, "  gvm  gvm   gvm_web")
	fmt.Fprintln(out, "then: sudo systemctl reload postgresql")
	fmt.Fprintln(out, "\nAfter `gvm db migrate`:")
	fmt.Fprintln(out, "  sudo systemctl enable --now gvmd gvm-web")
	fmt.Fprintln(out, "  sudo gvm user create --email you@example.com --role owner")
}

// diff prints what a real run would write and changes nothing. Every line it
// prints is a line Run would act on, read from the same Manifest, so the
// preview cannot drift from the act.
func diff(ctx context.Context, sys System, out io.Writer, files []File, opt Options) error {
	fmt.Fprintln(out, "gvm host init --diff (nothing will be changed)")
	fmt.Fprintf(out, "  would install binary    %s\n", BinaryPath)
	for _, f := range files {
		fmt.Fprintf(out, "  would write   file      %s\n", f.Dest)
	}
	fmt.Fprintln(out, "  would run     systemd-sysusers")
	for _, f := range files {
		if f.Kind == KindTmpfiles {
			fmt.Fprintf(out, "  would run     systemd-tmpfiles --create %s\n", f.Dest)
		}
	}
	fmt.Fprintln(out, "  would reload  systemd")

	firstInit, err := sys.Exists(MarkerPath)
	if err != nil {
		return fmt.Errorf("check %s: %w", MarkerPath, err)
	}
	if !firstInit {
		for _, t := range FirstTimers() {
			fmt.Fprintf(out, "  would enable  timer     %s (first init)\n", t)
		}
	} else {
		fmt.Fprintln(out, "  timers already managed; would leave them as configured")
	}
	hasConfig, err := sys.Exists(ConfigPath)
	if err != nil {
		return fmt.Errorf("check %s: %w", ConfigPath, err)
	}
	if !hasConfig {
		fmt.Fprintf(out, "  would write   config    %s from defaults\n", ConfigPath)
	} else {
		fmt.Fprintf(out, "  config    %s already present; would keep it\n", ConfigPath)
	}
	if opt.Dashboard {
		hasKey, err := sys.Exists(secrets.MasterKeyPath)
		if err != nil {
			return fmt.Errorf("check %s: %w", secrets.MasterKeyPath, err)
		}
		if !hasKey {
			fmt.Fprintf(out, "  would write   key       %s (0400 root)\n", secrets.MasterKeyPath)
		} else {
			fmt.Fprintf(out, "  key       %s already present; would keep it\n", secrets.MasterKeyPath)
		}
	}
	printAptHints(out)
	return nil
}

func printAptHints(out io.Writer) {
	fmt.Fprintln(out, "\nPackages per game family — gvm prints these and runs none of them:")
	for _, h := range AptHints() {
		fmt.Fprintf(out, "  %s\n    (%s)\n    %s\n", h.Family, h.Why, h.Cmd)
	}
}
