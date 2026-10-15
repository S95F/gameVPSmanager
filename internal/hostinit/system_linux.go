//go:build linux

package hostinit

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	sd "github.com/coreos/go-systemd/v22/dbus"
)

// linuxSystem is the real host. It runs exactly two external programs —
// systemd's own sysusers and tmpfiles tools — each by absolute path with an
// argv and no shell (rule 2, asserted by noshell_test.go), and drives the
// manager reload and enable over D-Bus rather than through `systemctl`, for
// the same reason the rest of gvm does: systemctl's exit codes conflate "the
// unit failed" with "the unit does not exist".
type linuxSystem struct{ conn *sd.Conn }

// NewSystem opens the systemd connection. The closer releases it.
func NewSystem(ctx context.Context) (System, func() error, error) {
	conn, err := sd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to systemd: %w; host init needs root and a running systemd", err)
	}
	return &linuxSystem{conn: conn}, func() error { conn.Close(); return nil }, nil
}

func (s *linuxSystem) Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// ReplaceBinary writes this executable to a sibling of dest and rename(2)s it
// into place. A game runner that is executing the old /usr/bin/gvm keeps its
// open inode, so the replace cannot produce "text file busy" and the running
// game is untouched — the DoD asserts its pid and starttime do not change.
func (s *linuxSystem) ReplaceBinary(dest string) error {
	selfPath, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(selfPath)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp := dest + ".new"
	// 0755 and root:root: the temp carries its final mode and owner BEFORE the
	// rename, so the atomic swap never exposes a half-owned or group-writable
	// binary for even an instant.
	dstf, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dstf, src); err != nil {
		dstf.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := dstf.Chmod(0o755); err != nil {
		dstf.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := dstf.Chown(0, 0); err != nil {
		dstf.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := dstf.Sync(); err != nil {
		dstf.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := dstf.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// WriteRoot writes root-owned data atomically: a temp sibling, chowned and
// chmoded BEFORE the rename, so a failed chown never leaves a half-owned spec
// and a reader never sees a partial file.
func (s *linuxSystem) WriteRoot(path string, data []byte, mode uint32) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gvm-init-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(os.FileMode(mode)); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chown(0, 0); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func (s *linuxSystem) Sysusers(ctx context.Context) error {
	prog, err := systemdTool("systemd-sysusers")
	if err != nil {
		return err
	}
	return runArgv(ctx, prog)
}

func (s *linuxSystem) Tmpfiles(ctx context.Context, path string) error {
	prog, err := systemdTool("systemd-tmpfiles")
	if err != nil {
		return err
	}
	return runArgv(ctx, prog, "--create", path)
}

func (s *linuxSystem) ReloadDaemon(ctx context.Context) error {
	return s.conn.ReloadContext(ctx)
}

// EnableUnit installs the unit's [Install] symlinks under /etc (runtime=false,
// so it survives a reboot) and replaces a stale symlink (force=true). It does
// not start the unit: the timers host init enables fire on their own schedule.
func (s *linuxSystem) EnableUnit(ctx context.Context, unit string) error {
	_, _, err := s.conn.EnableUnitFilesContext(ctx, []string{unit}, false, true)
	return err
}

// systemdTools is the complete set of absolute paths host init may execute.
// Both are systemd's own, and the candidate list is absolute so PATH never
// decides what runs as root. noshell_test.go asserts nothing else appears.
var systemdTools = map[string][]string{
	"systemd-sysusers": {"/usr/bin/systemd-sysusers", "/bin/systemd-sysusers"},
	"systemd-tmpfiles": {"/usr/bin/systemd-tmpfiles", "/bin/systemd-tmpfiles"},
}

func systemdTool(name string) (string, error) {
	for _, p := range systemdTools[name] {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found at %v; is systemd installed?", name, systemdTools[name])
}

// runArgv runs one program with an argv and no shell. The environment is left
// as inherited (systemd's tools read none of gvm's), and the output is folded
// into the error on failure so a packaging mistake is legible.
func runArgv(ctx context.Context, prog string, args ...string) error {
	cmd := exec.CommandContext(ctx, prog, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", prog, args, err, out)
	}
	return nil
}
