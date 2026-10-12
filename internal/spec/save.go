package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Modes for everything gvm writes under /etc/gvm (section 5). The directory is
// 0750 root:gvm and every file in it 0640 root:gvm, so the unprivileged web tier
// can read a spec in order to render it and can never write one.
const (
	ModeSpec    fs.FileMode = 0o640
	ModeSpecDir fs.FileMode = 0o750
)

// BackupSuffix names the previous version kept beside every file gvm rewrites.
const BackupSuffix = ".bak"

// Steps of the atomic write, in order. They are named because save_test.go
// asserts the order: a sync after the rename, or a backup taken after it, is
// still a working write on a machine that never loses power and a silent data
// loss on one that does — a failure no functional test can see.
const (
	stepWrite     = "write"
	stepSyncFile  = "sync_file"
	stepCloseFile = "close_file"
	stepChmod     = "chmod"
	stepBackup    = "backup"
	stepRename    = "rename"
	stepSyncDir   = "sync_dir"
)

// errProbeAbort is returned when a test asks the writer to stop at a named step.
var errProbeAbort = errors.New("write aborted at probe point")

// writeProbe is the seam save_test.go drives. Production callers pass nil.
//
// failAt stops the write immediately AFTER that step, and deliberately leaves
// the filesystem exactly as a killed process would — no temp-file cleanup, no
// unwinding — because that is the only state worth asserting about: the original
// file must still be there, whole.
type writeProbe struct {
	trace  []string
	failAt string
}

func (p *writeProbe) done(step string) error {
	if p == nil {
		return nil
	}
	p.trace = append(p.trace, step)
	if p.failAt == step {
		return fmt.Errorf("%w: %s", errProbeAbort, step)
	}
	return nil
}

// WriteFileAtomic replaces path with data, or leaves path exactly as it was.
//
// Temp file in the SAME directory (a rename across filesystems is a copy, and a
// copy is not atomic), fsync the temp, keep the previous contents as path.bak,
// rename, fsync the directory. A crash at any point leaves either the old file
// or the new one, never a truncated file and never a file that exists in the
// directory listing but has no data behind it.
//
// The .bak is written BEFORE the rename. Written after, a crash between the two
// would lose the previous version entirely — which is the one moment anybody
// wants it.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return writeAtomic(path, data, perm, true, -1, -1, nil)
}

// WriteFileAtomicNoBackup is WriteFileAtomic for a file whose previous
// contents must cease to exist when it is replaced.
//
// Rotating a credential that leaves the old value behind in path.bak has not
// rotated anything: the .bak is mode 0600 today and on a backup tape tomorrow,
// and it never expires. internal/secrets writes every file through here for
// that reason, and it is exported rather than reimplemented so that the crash
// ordering the probe points in this file pin down — sync, then rename, then
// sync the directory — holds for secrets too.
func WriteFileAtomicNoBackup(path string, data []byte, perm fs.FileMode) error {
	return writeAtomic(path, data, perm, false, -1, -1, nil)
}

// WriteFileAtomicOwned is WriteFileAtomicNoBackup with the owner set on the
// temporary file, before the rename, for the same reason the mode is: a file
// that appears at its final path owned by the wrong account -- even for a
// microsecond -- is a file its intended reader cannot open.
//
// It takes NUMBERS and not names, so this package needs no os/user lookup and
// the account resolution stays with the caller that knows which account it
// means. -1 leaves that half alone, which is what every caller outside
// internal/backup passes.
//
// internal/backup's freeze lease is the reason it exists. The runner reads
// that file as the instance user and treats one it cannot read as a freeze
// that IS armed -- so it thaws, and clears the lease -- which makes a
// published-then-chowned lease a published thaw.
func WriteFileAtomicOwned(path string, data []byte, perm fs.FileMode, uid, gid int) error {
	return writeAtomic(path, data, perm, false, uid, gid, nil)
}

func writeAtomic(path string, data []byte, perm fs.FileMode, keepBackup bool,
	uid, gid int, p *writeProbe) (err error) {
	dir := filepath.Dir(path)

	prev, prevErr := os.ReadFile(path)
	switch {
	case prevErr == nil:
	case errors.Is(prevErr, fs.ErrNotExist):
		prev = nil
	default:
		return fmt.Errorf("read current %s: %w", path, prevErr)
	}

	// The dot prefix keeps a temp file that outlives a crash out of every
	// *.yaml glob the catalog and the operator use, so a leftover can never be
	// mistaken for a manifest.
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer func() {
		if err == nil {
			return
		}
		// The handle always goes, because a killed process loses its handles
		// too. The temp FILE stays on a simulated crash: that is the state a
		// real one leaves behind, and the test's whole subject is what the
		// directory looks like afterwards.
		f.Close()
		if !errors.Is(err, errProbeAbort) {
			os.Remove(tmp)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err = p.done(stepWrite); err != nil {
		return err
	}

	if err = f.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err = p.done(stepSyncFile); err != nil {
		return err
	}

	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err = p.done(stepCloseFile); err != nil {
		return err
	}

	// Chmod before the rename: os.CreateTemp makes the file 0600, and a file
	// that appears at its final path with the wrong mode — even for a
	// microsecond — is a file the web tier cannot read or, worse, could write.
	if err = os.Chmod(tmp, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err = p.done(stepChmod); err != nil {
		return err
	}

	// Beside the chmod and for the same reason. Deliberately NOT a probe
	// point: a crash here leaves exactly what a crash at the chmod leaves --
	// the original file whole and a temp file nothing points at -- so adding
	// one would assert the same thing twice and change every trace the
	// ownerless callers produce.
	if uid != -1 || gid != -1 {
		if err = os.Chown(tmp, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", tmp, err)
		}
	}

	if keepBackup && prev != nil {
		// Atomic in its own right, so the .bak is always a complete previous
		// version rather than whatever survived a crash mid-copy.
		if err = writeAtomic(path+BackupSuffix, prev, perm, false, uid, gid, nil); err != nil {
			return fmt.Errorf("keep %s: %w", path+BackupSuffix, err)
		}
	}
	if err = p.done(stepBackup); err != nil {
		return err
	}

	if err = renameWithRetry(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	if err = p.done(stepRename); err != nil {
		return err
	}

	if err = syncDir(dir); err != nil {
		return err
	}
	return p.done(stepSyncDir)
}

// renameWithRetry replaces an existing target.
//
// os.Rename is MoveFileEx(MOVEFILE_REPLACE_EXISTING) on Windows and rename(2) on
// Linux, so replacing an existing file is atomic on both. The difference is that
// Windows refuses the replace outright while any other process holds the target
// open without FILE_SHARE_DELETE — an editor, a backup agent, an antivirus
// scanner that opened the file a millisecond ago — and reports it as a
// permission error. A few short retries turn a scanner's window into a
// non-event; retrying for longer would hide a file that is genuinely held open
// and turn "I cannot save" into "gvm hangs".
func renameWithRetry(oldPath, newPath string) error {
	var err error
	for attempt := range 5 {
		if err = os.Rename(oldPath, newPath); err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrPermission) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}

// syncDir makes the rename itself durable.
//
// Opening the directory can fail and that is worth reporting. The fsync failing
// is not: FlushFileBuffers has no meaning for a directory handle on Windows and
// returns an error there always, and by the time it is called the rename has
// already succeeded — telling the caller their write failed when it did not
// would send them into a retry loop that cannot improve anything.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s for fsync: %w", dir, err)
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

// ReplaceFile and SyncDir are writeAtomic's last two steps, exported for the
// one writer that cannot hold its bytes in memory: the blob store streams a
// multi-gigabyte download through a hasher into a staging file, so it cannot
// call WriteFileAtomic and must still end with the same publish.
//
// They are exported rather than reimplemented for the reason the comment on
// WriteFileAtomicNoBackup gives: the Windows retry window and the "an fsync
// failure after a successful rename is not the caller's problem" judgement are
// decisions, and a second copy of them would drift from this one silently.
func ReplaceFile(staged, path string) error { return renameWithRetry(staged, path) }

// SyncDir makes a rename in dir durable. See ReplaceFile.
func SyncDir(dir string) error { return syncDir(dir) }

// MarshalYAML encodes v the way gvm writes YAML: two-space indent, matching the
// shipped manifests, so a gvm-written file and a hand-written one do not differ
// cosmetically in every diff.
func MarshalYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SaveInstance writes /etc/gvm/instances/<name>.yaml atomically. This file is
// the definition of one server; a half-written one is a server nobody can start.
func SaveInstance(path string, in *Instance) error {
	data, err := MarshalYAML(in)
	if err != nil {
		return fmt.Errorf("encode instance: %w", err)
	}
	return WriteFileAtomic(path, data, ModeSpec)
}

// SaveHostConfig writes /etc/gvm/gvm.yaml atomically.
func SaveHostConfig(path string, h *HostConfig) error {
	data, err := MarshalYAML(h)
	if err != nil {
		return fmt.Errorf("encode host config: %w", err)
	}
	return WriteFileAtomic(path, data, ModeSpec)
}
