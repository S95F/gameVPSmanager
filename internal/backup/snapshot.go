package backup

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// Snapshot strategy: keep the freeze window in milliseconds where the
// filesystem allows it, and say so out loud where it does not (section 13.2).
//
// The freeze window is the period during which the game is not persisting. The
// whole of this file exists to make that window as short as the host can
// manage, and -- just as importantly -- to RECORD which of the three answers
// the host gave, because the operator of an 8 GB ext4 VPS deserves to know
// that their nightly backup pauses their world for seventy-four seconds rather
// than for four milliseconds.
//
//	reflink  -- FICLONE per file. Window ~ms. XFS and btrfs.
//	btrfs    -- read-only subvolume snapshot, when the world paths are
//	            subvolume roots. Window ~ms, and atomic across the whole tree.
//	direct   -- tar straight from the live tree. Window = the whole archive.
//	            ext4's answer, which is to say most VPS images' answer.
//
// The ORDER of operations differs between the two families, and that is the
// part a reader is most likely to get wrong:
//
//	reflink/btrfs:  freeze -> clone -> THAW -> tar from the clone -> delete the clone
//	direct:         freeze -> tar -> thaw
//
// Under the fast methods the thaw happens before the expensive part, so the
// game is saving again while the archive is still being written. Under direct
// it cannot: there is nothing to archive from except the live tree.
// Method.ThawBeforeArchive is that distinction as one bool, and the freeze
// lease's deadline is computed from it (BudgetFor) -- because a 120-second
// lease on a seventy-four-second direct tar would re-enable autosave in the
// middle of the read.

// Method is how the bytes are captured.
type Method string

const (
	// MethodReflink clones each file with FICLONE into the snapshot
	// directory. Copy-on-write, so the clone costs metadata rather than
	// bytes, and it is MANDATORY that the destination be on the same
	// filesystem -- which is why the clone lives in
	// /var/lib/gvm/instances/<name>/snap rather than in /tmp.
	MethodReflink Method = "reflink"
	// MethodBtrfs takes a read-only subvolume snapshot. Preferred over
	// reflink on btrfs because it is ONE atomic operation over the whole
	// tree, where a per-file clone walk is a sequence of them -- and a world
	// cloned file by file is only a point-in-time image because the freeze is
	// holding the game still.
	MethodBtrfs Method = "btrfs"
	// MethodDirect tars the live tree. The freeze covers the whole archive.
	MethodDirect Method = "direct"
	// MethodZFS takes a native ZFS dataset snapshot via the ZFS_IOC_SNAPSHOT
	// ioctl. The snapshot is atomic and instantaneous, like MethodBtrfs. The
	// ioctl implementation (NVList encoding + /dev/zfs) is in snapshot_linux.go;
	// Choose falls back to MethodReflink or MethodDirect for ZFS hosts until the
	// dataset path can be derived from the world path.
	MethodZFS Method = "zfs"
)

// Methods is every method, in the order Choose prefers them.
func Methods() []Method { return []Method{MethodBtrfs, MethodReflink, MethodDirect} }

// Valid reports whether m is a known method.
func (m Method) Valid() bool {
	return m == MethodReflink || m == MethodBtrfs || m == MethodDirect || m == MethodZFS
}

func (m Method) String() string { return string(m) }

// ThawBeforeArchive reports whether the archive is written from a CLONE, and
// therefore after the thaw.
//
// It is the single fact the rest of the package needs from the choice: it
// decides the order of the steps, it decides the lease budget, and it decides
// whether "stall" means milliseconds or minutes.
func (m Method) ThawBeforeArchive() bool {
	return m == MethodReflink || m == MethodBtrfs || m == MethodZFS
}

// StallRed is the freeze duration above which the dashboard renders the number
// in red (section 13.2).
//
// The constant lives here rather than in the web tier because the number IS
// the judgement -- "half a minute without persistence is not routine" -- and a
// threshold that lived only in a template would drift from the code that
// produces the measurement.
const StallRed = 30 * time.Second

// DefaultFloorMBps is the throughput floor BudgetFor derives a direct
// snapshot's lease extension from.
//
// A floor and not an estimate. pgzip at level 5 on a 4-vCPU VPS manages
// rather more than this; the number is deliberately pessimistic so that a
// lease derived from it is generous, because the cost of a lease that is too
// short is a thaw in the middle of a tar.
const DefaultFloorMBps = 20

// SnapDirMode matches hostops' DirSnap: 0750 root:root under
// /var/lib/gvm/instances/<name>, where the control plane's own working files
// live and a compromised mod cannot reach (section 5.3).
const SnapDirMode = 0o750

// Snapshot errors.
var (
	// ErrNoClone means the host cannot clone: no reflink support, not btrfs,
	// or the ioctl refused. It is NOT fatal to a backup -- the engine falls
	// back to direct with the reason recorded -- which is why it is a
	// sentinel rather than a message.
	ErrNoClone = errors.New("backup: this filesystem cannot snapshot without copying")
	// ErrSnapshot is a clone that was attempted and failed partway. The
	// partial clone is removed by the caller's defer; the archive is then
	// taken directly.
	ErrSnapshot = errors.New("backup: the snapshot could not be taken")
)

// Capability is what the filesystem under the world paths can actually do.
//
// Probe fills it in from statfs and a trial clone (snapshot_linux.go); off
// Linux the stub fills in nothing and says so, which makes Choose's answer
// `direct` on the development machine -- the honest answer, and the one whose
// ordering is hardest to get right, so it is the one the tests exercise most.
type Capability struct {
	// FSType is the statfs magic rendered as a name, for the record and for
	// `gvm doctor`. An unrecognised magic is rendered as hex rather than
	// guessed at.
	FSType string
	// Reflink says FICLONE worked on a trial file. It is a trial and not an
	// inference from the filesystem type, because XFS only supports it when
	// the volume was made with reflink=1 and ext4 is growing support
	// gradually -- so asking the kernel is the only answer that does not go
	// stale.
	Reflink bool
	// Btrfs says the world paths are on btrfs.
	Btrfs bool
	// Subvolumes says EVERY world path is a subvolume root. A snapshot of a
	// directory that merely sits inside a subvolume is not possible, and one
	// taken of the enclosing subvolume would archive somebody else's data.
	Subvolumes bool
	// ZFS says the world paths are on a ZFS dataset.
	ZFS bool
	// Note explains anything the fields above cannot, and goes in the job
	// record verbatim.
	Note string
}

// Choose is the decision table, and it is pure.
//
// allow restricts the answer -- an operator who has had a bad experience with
// their kernel's FICLONE can pin an instance to `direct`, and `gvm doctor`
// renders what each instance would choose without taking a backup. A nil allow
// means every method.
//
// It never returns an invalid method and never returns an error: there is
// always a correct answer, and `direct` is it. A filesystem probe that failed
// is a reason to tar, not a reason to refuse to back up.
func Choose(c Capability, allow []Method) (Method, string) {
	permitted := func(m Method) bool { return len(allow) == 0 || slices.Contains(allow, m) }
	fsName := c.FSType
	if fsName == "" {
		fsName = "an unidentified filesystem"
	}

	switch {
	case c.Btrfs && c.Subvolumes && permitted(MethodBtrfs):
		return MethodBtrfs, fmt.Sprintf("every world path on %s is a subvolume root, so the "+
			"freeze lasts one atomic snapshot", fsName)
	case c.Reflink && permitted(MethodReflink):
		return MethodReflink, fmt.Sprintf("%s clones with FICLONE, so the freeze lasts a "+
			"metadata copy rather than the archive", fsName)
	case c.Btrfs && permitted(MethodBtrfs):
		// btrfs without subvolume roots. Reported rather than silently
		// downgraded: "you are on btrfs and not getting btrfs snapshots" is a
		// thing an operator can fix, and will not fix if nobody says it.
		return MethodDirect, fmt.Sprintf("%s is btrfs but the world paths are not subvolume "+
			"roots, so the whole archive is taken inside the freeze; making them subvolumes "+
			"would cut the stall to milliseconds", fsName)
	case c.ZFS:
		// ZFS detected. The native snapshot ioctl (MethodZFS) is wired in but
		// requires the dataset name, which is not yet surfaced from the world
		// path; FICLONE is tried first. If neither is available, fall through
		// to direct and say so -- "you are on ZFS without FICLONE" is a useful
		// thing to know.
		return MethodDirect, fmt.Sprintf("%s is ZFS; the native snapshot ioctl (MethodZFS) is "+
			"not yet wired, so the whole archive is taken inside the freeze", fsName)
	case !permitted(MethodDirect):
		// Every method the caller allowed is unavailable, and direct is not
		// among them. Direct is returned anyway WITH the contradiction named:
		// refusing to back up because a preference could not be honoured
		// would turn a configuration opinion into a gap in history.
		return MethodDirect, fmt.Sprintf("none of the allowed snapshot methods are available on "+
			"%s, and a backup not taken is worse than a slow one, so the archive is taken "+
			"inside the freeze", fsName)
	default:
		return MethodDirect, fmt.Sprintf("%s supports neither reflink nor btrfs subvolumes, so "+
			"the whole archive is taken inside the freeze", fsName)
	}
}

// Plan is the snapshot half of a backup, decided before anything is frozen.
//
// It is decided in the PREFLIGHT and not in the snapshot step, because the
// freeze lease's deadline depends on which family was chosen and the lease is
// armed before the freeze. A plan computed after the freeze would be a budget
// computed after the thing it bounds.
type Plan struct {
	Method Method
	Why    string
	// Cap is what the probe found, recorded so a job log can explain a
	// `direct` choice without re-probing.
	Cap Capability
	// SnapDir is /var/lib/gvm/instances/<name>/snap, the staging directory a
	// clone lands in. It MUST be on the same filesystem as the world for
	// reflink to be possible at all, which is why it is under the instance's
	// state directory and not under /tmp.
	SnapDir string
	// Clone is the directory the clone will be created as, under SnapDir. It
	// is empty for a direct snapshot.
	Clone string
	// Sources are the world roots being captured, absolute.
	Sources []string
}

// SnapshotResult is what the snapshot step did, for the job record.
type SnapshotResult struct {
	Method Method `json:"snapshot_method"`
	Why    string `json:"snapshot_why,omitempty"`
	// StallMs is how long the game spent not persisting. Section 13.2's
	// number: the dashboard renders it in red above StallRed.
	StallMs int64 `json:"stall_ms"`
	// Fallback says the chosen method failed and the archive was taken
	// directly instead. The reason is in Why, appended.
	Fallback bool `json:"snapshot_fallback,omitempty"`
	// ClonePath is the staging directory, for the case where the cleanup
	// defer did not get to run and the startup sweep has to.
	ClonePath string `json:"clone_path,omitempty"`
}

// Red reports whether the stall is in the range section 13.2 wants rendered in
// red.
func (r SnapshotResult) Red() bool { return time.Duration(r.StallMs)*time.Millisecond > StallRed }

// ClonePathFor is where one run's clone goes.
//
// The name carries the timestamp so that a leftover clone can be aged by the
// startup sweep without stat'ing it, and so that two runs cannot collide on a
// directory one of them is about to delete.
func ClonePathFor(snapDir, instance string, at time.Time) string {
	return path.Join(snapDir, instance+"-"+at.UTC().Format("20060102-150405")+".snap")
}

// clonePrefixOf is the filename prefix SweepSnapDir recognises as ours.
func clonePrefixOf(instance string) string { return instance + "-" }

// appendNote joins two observations without producing a dangling separator,
// so a record with one note reads as a sentence rather than as a list of one.
func appendNote(existing, add string) string {
	switch {
	case add == "":
		return existing
	case existing == "":
		return add
	default:
		return existing + "; " + add
	}
}

// joinNames renders a set of filesystem names. More than one means the world
// paths straddle a mount point, which is itself worth seeing in the record.
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names, "+")
	}
}

// SweepSnapDir removes clones left behind by a job that did not get to run its
// cleanup.
//
// Section 13.2 asks for "a defer plus a startup sweep", and this is the second
// half. The defer covers an error and a panic; it does not cover SIGKILL, and a
// 2.4 GB reflink clone left in /var/lib/gvm is a disk that fills up over a
// month of bad nights.
//
// It only ever removes directories whose names this package wrote: a sweep that
// deleted everything under a directory would be one `snap_dir` typo away from
// deleting a world. olderThan is a grace period so a sweep running while
// another job holds a clone cannot delete it underneath that job.
func SweepSnapDir(f FS, snapDir, instance string, now time.Time, olderThan time.Duration) ([]string, error) {
	if f == nil || snapDir == "" {
		return nil, nil
	}
	entries, err := f.ReadDir(snapDir)
	if err != nil {
		// A missing snap directory is the normal state: it is created by the
		// first clone and removed with the instance.
		return nil, nil
	}
	prefix := clonePrefixOf(instance)
	var removed []string
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".snap") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".snap")
		at, perr := time.Parse("20060102-150405", stamp)
		if perr != nil {
			// A directory with our prefix and a name we did not write. Left
			// alone, for the same reason retention leaves an unmanaged
			// tarball alone.
			continue
		}
		if olderThan > 0 && now.Sub(at.UTC()) < olderThan {
			continue
		}
		p := path.Join(snapDir, name)
		if err := f.RemoveAll(p); err != nil {
			// A read-only btrfs subvolume clone cannot be removed with RemoveAll;
			// it needs BTRFS_IOC_SNAP_DESTROY_V2. Try that before giving up, so a
			// subvolume clone is actually reclaimed rather than left to shrink
			// free space every night (offsite-backup-dr-10).
			if derr := destroyBtrfsSubvolume(p); derr != nil {
				errs = append(errs, fmt.Errorf("backup: sweeping %s: %w (subvolume destroy: %v)",
					p, err, derr))
				continue
			}
		}
		removed = append(removed, p)
	}
	return removed, errors.Join(errs...)
}
