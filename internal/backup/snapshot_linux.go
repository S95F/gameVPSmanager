//go:build linux

package backup

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The Linux half of the snapshot strategy: the two measurements and the two
// clones.
//
// Everything in here is a question for the kernel rather than an inference
// from a filesystem name, and that is deliberate. XFS supports reflink only
// when the volume was made with reflink=1 (the mkfs default since 5.x, but not
// before), ext4's support is arriving gradually, and overlayfs answers
// differently again depending on what is underneath it. A table of
// "filesystems that can clone" would have been wrong on somebody's VPS the
// week it was written. Asking FICLONE whether it works cannot go stale.

// btrfsFirstFreeObjectID is the inode number of a btrfs SUBVOLUME ROOT.
//
// This is how a subvolume is distinguished from an ordinary directory inside
// one, and the distinction matters: BTRFS_IOC_SNAP_CREATE_V2 can only snapshot
// a subvolume root, and snapshotting the subvolume that merely CONTAINS a
// world would capture whatever else lives in it.
const btrfsFirstFreeObjectID = 256

// btrfsIocSnapCreateV2 is _IOW(BTRFS_IOCTL_MAGIC, 23,
// struct btrfs_ioctl_vol_args_v2).
//
// Spelled out as a number with its derivation because x/sys/unix does not
// export it and the project takes no new dependencies:
//
//	dir=1 (write) << 30         = 0x40000000
//	size=4096 (the struct)<< 16 = 0x10000000
//	type='X'=0x94          << 8 = 0x00009400
//	nr=23                       = 0x00000017
const btrfsIocSnapCreateV2 = 0x50009417

// btrfsSubvolRDOnly makes the snapshot read-only, which is what a backup
// source should be: nothing can write into the image between the thaw and the
// tar.
const btrfsSubvolRDOnly = 1 << 1

// The offsets of the three fields used inside struct
// btrfs_ioctl_vol_args_v2.
//
// Written as explicit offsets into a fixed 4096-byte buffer rather than as a
// Go struct, on purpose. A Go struct mirroring a kernel union depends on this
// compiler laying out padding the way that kernel does; explicit offsets
// depend on nothing, are checkable against the header by eye, and cannot
// silently shift when a field is added above them.
//
//	 0  __s64 fd            -- the subvolume to snapshot
//	 8  __u64 transid
//	16  __u64 flags         -- BTRFS_SUBVOL_RDONLY
//	24  union { size; qgroup_inherit* } / __u64 unused[4]
//	56  char name[4040]     -- the new subvolume's name, NUL-terminated
const (
	btrfsVolArgsSize    = 4096
	btrfsVolArgsFD      = 0
	btrfsVolArgsFlag    = 16
	btrfsVolArgsName    = 56
	btrfsVolArgsNameMax = btrfsVolArgsSize - btrfsVolArgsName - 1
)

// Probe asks the kernel what the filesystem under these paths can do.
//
// snapDir is part of the question and not an afterthought: a reflink clone's
// destination MUST be on the same filesystem as its source, so a world on
// /home with its snapshot staging on a separate /var is not a reflink
// candidate however capable either filesystem is. Checking it here means the
// answer Choose gets is about the operation that will actually be attempted.
func Probe(worldPaths []string, snapDir string) (Capability, error) {
	if len(worldPaths) == 0 {
		return Capability{Note: "no world paths were declared, so there was nothing to probe"},
			errors.New("backup: probe needs at least one world path")
	}

	var cap Capability
	var magics []int64
	allBtrfs, allSubvol := true, true

	for _, p := range worldPaths {
		var st unix.Statfs_t
		if err := unix.Statfs(p, &st); err != nil {
			return Capability{Note: fmt.Sprintf("statfs %s failed: %v", p, err)},
				fmt.Errorf("backup: statfs %s: %w", p, err)
		}
		magic := int64(st.Type)
		if !containsInt64(magics, magic) {
			magics = append(magics, magic)
		}
		if magic != unix.BTRFS_SUPER_MAGIC {
			allBtrfs = false
			allSubvol = false
			continue
		}
		root, err := isSubvolumeRoot(p)
		if err != nil || !root {
			allSubvol = false
		}
	}

	cap.FSType = fsTypeName(magics)
	cap.Btrfs = allBtrfs
	cap.Subvolumes = allBtrfs && allSubvol

	// The reflink trial. It runs against the STAGING directory, because that
	// is where a clone's destination lands, and it compares devices with the
	// world first -- a successful FICLONE inside /var/lib/gvm proves nothing
	// about cloning /home/pzserver/Zomboid into it.
	same, why := sameFilesystem(worldPaths, snapDir)
	switch {
	case !same:
		cap.Note = appendNote(cap.Note, why)
	default:
		ok, trialErr := trialClone(snapDir)
		cap.Reflink = ok
		if trialErr != nil {
			cap.Note = appendNote(cap.Note, "the FICLONE trial failed: "+trialErr.Error())
		}
	}
	if cap.Btrfs && !cap.Subvolumes {
		cap.Note = appendNote(cap.Note, "the world paths are on btrfs but are not subvolume roots")
	}
	return cap, nil
}

// isSubvolumeRoot reports whether p is a btrfs subvolume root.
func isSubvolumeRoot(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("backup: stat returned no unix metadata")
	}
	return sys.Ino == btrfsFirstFreeObjectID, nil
}

// sameFilesystem reports whether every world path is on the same device as the
// staging directory.
//
// The staging directory may not exist yet -- it is created by the first clone
// -- so the nearest existing ancestor is what gets stat'ed. That is the right
// question anyway: a directory created under /var/lib/gvm lands on whatever
// filesystem /var/lib/gvm is on.
func sameFilesystem(worldPaths []string, snapDir string) (bool, string) {
	if snapDir == "" {
		return false, "no snapshot staging directory was named, so a clone had nowhere to land"
	}
	dev, err := deviceOf(nearestExisting(snapDir))
	if err != nil {
		return false, fmt.Sprintf("the snapshot staging directory %s could not be stat'ed: %v", snapDir, err)
	}
	for _, p := range worldPaths {
		wd, err := deviceOf(p)
		if err != nil {
			return false, fmt.Sprintf("%s could not be stat'ed: %v", p, err)
		}
		if wd != dev {
			return false, fmt.Sprintf("%s and the staging directory %s are on different "+
				"filesystems, so a reflink clone is not possible", p, snapDir)
		}
	}
	return true, ""
}

func deviceOf(p string) (uint64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("backup: stat returned no unix metadata")
	}
	return uint64(sys.Dev), nil
}

// nearestExisting walks up until something exists, so a staging directory that
// has never been created can still be located on a filesystem.
func nearestExisting(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// trialClone writes two small files in dir and asks FICLONE to clone one into
// the other.
//
// A trial rather than a lookup table, for the reason at the top of this file.
// Both files are removed whatever happens: this runs in
// /var/lib/gvm/instances/<name>/snap, which is gvm's own working directory,
// and leaving probe litter in it would be swept later as if it were a clone.
func trialClone(dir string) (bool, error) {
	if err := os.MkdirAll(dir, SnapDirMode); err != nil {
		return false, err
	}
	src, err := os.CreateTemp(dir, ".ficlone-probe-src*")
	if err != nil {
		return false, err
	}
	defer func() { src.Close(); os.Remove(src.Name()) }()
	// A page of bytes rather than none: FICLONE on an empty file is a
	// degenerate case some filesystems accept and others reject, and neither
	// answer would say anything about cloning a world.
	if _, err := src.Write(make([]byte, 4096)); err != nil {
		return false, err
	}
	if err := src.Sync(); err != nil {
		return false, err
	}
	dst, err := os.CreateTemp(dir, ".ficlone-probe-dst*")
	if err != nil {
		return false, err
	}
	defer func() { dst.Close(); os.Remove(dst.Name()) }()

	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err != nil {
		// EOPNOTSUPP and EXDEV are the two expected refusals and are not
		// errors in the operational sense: they are the answer "no". Anything
		// else is reported so that a permissions problem does not masquerade
		// as an unsupported filesystem.
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) ||
			errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EINVAL) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Clone captures src at dst by the chosen method.
//
// It returns the notes a job log should carry -- a skipped socket, a device
// node -- alongside the error, because a clone that silently dropped a file
// would produce an archive missing data that the manifest would then faithfully
// describe as complete.
func Clone(ctx context.Context, m Method, src, dst string) ([]string, error) {
	switch m {
	case MethodBtrfs:
		return nil, btrfsSnapshot(src, dst)
	case MethodReflink:
		return reflinkTree(ctx, src, dst)
	default:
		return nil, fmt.Errorf("%w: %q is not a cloning method", ErrNoClone, m)
	}
}

// btrfsSnapshot takes a read-only subvolume snapshot of src as dst.
//
// The ioctl is issued on the PARENT of dst (the directory the new subvolume is
// created in) with the source subvolume's descriptor in the struct, which is
// the inverse of what the name suggests and is the single most common way to
// get this call wrong.
func btrfsSnapshot(src, dst string) error {
	name := filepath.Base(dst)
	if len(name) == 0 || len(name) > btrfsVolArgsNameMax {
		return fmt.Errorf("%w: %q is not a usable subvolume name", ErrSnapshot, name)
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, SnapDirMode); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrSnapshot, parent, err)
	}

	srcFD, err := unix.Open(src, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: open %s: %w", ErrSnapshot, src, err)
	}
	defer unix.Close(srcFD)

	parentFD, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: open %s: %w", ErrSnapshot, parent, err)
	}
	defer unix.Close(parentFD)

	var args [btrfsVolArgsSize]byte
	binary.LittleEndian.PutUint64(args[btrfsVolArgsFD:], uint64(srcFD))
	binary.LittleEndian.PutUint64(args[btrfsVolArgsFlag:], btrfsSubvolRDOnly)
	copy(args[btrfsVolArgsName:btrfsVolArgsName+btrfsVolArgsNameMax], name)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(parentFD),
		uintptr(btrfsIocSnapCreateV2), uintptr(unsafe.Pointer(&args[0])))
	if errno != 0 {
		// Wrapped in ErrNoClone and not ErrSnapshot when the kernel says the
		// operation does not apply, so the engine's fallback to `direct`
		// treats "this is not really a subvolume" as a capability answer
		// rather than as a broken host.
		if errno == unix.ENOTTY || errno == unix.EINVAL || errno == unix.EOPNOTSUPP {
			return fmt.Errorf("%w: BTRFS_IOC_SNAP_CREATE_V2 on %s: %w", ErrNoClone, src, errno)
		}
		return fmt.Errorf("%w: BTRFS_IOC_SNAP_CREATE_V2 on %s: %w", ErrSnapshot, src, errno)
	}
	return nil
}

// reflinkTree copies src to dst, cloning every regular file.
//
// Directories get their modes, symlinks get recreated as symlinks, and
// anything else is skipped with a note -- a socket or a device node in a world
// directory is not data a tar would have carried either, and following one
// would be a clone that blocks on a read.
func reflinkTree(ctx context.Context, src, dst string) ([]string, error) {
	var notes []string
	srcClean := filepath.Clean(src)

	err := filepath.WalkDir(srcClean, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, rerr := filepath.Rel(srcClean, p)
		if rerr != nil {
			return rerr
		}
		target := dst
		if rel != "." {
			target = filepath.Join(dst, rel)
		}

		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			link, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			return os.Symlink(link, target)
		case !info.Mode().IsRegular():
			notes = append(notes, "skipped "+rel+" ("+info.Mode().String()+"): not a regular file")
			return nil
		}
		return cloneFile(p, target, info.Mode().Perm())
	})
	if err != nil {
		return notes, fmt.Errorf("%w: cloning %s: %w", ErrSnapshot, src, err)
	}
	return notes, nil
}

// cloneFile FICLONEs one file.
func cloneFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		return fmt.Errorf("FICLONE %s: %w", src, err)
	}
	return out.Close()
}

// fsTypeName renders the statfs magic numbers as names.
//
// Only the handful that change a decision are named. Everything else is
// reported as hex rather than guessed at, because "0x2fc12fc1" in a job log is
// something an operator can look up and "unknown" is not.
func fsTypeName(magics []int64) string {
	names := make([]string, 0, len(magics))
	for _, m := range magics {
		switch m {
		case unix.BTRFS_SUPER_MAGIC:
			names = append(names, "btrfs")
		case unix.XFS_SUPER_MAGIC:
			names = append(names, "xfs")
		case unix.EXT4_SUPER_MAGIC:
			names = append(names, "ext2/3/4")
		case unix.TMPFS_MAGIC:
			names = append(names, "tmpfs")
		case unix.OVERLAYFS_SUPER_MAGIC:
			names = append(names, "overlayfs")
		default:
			names = append(names, "0x"+strconv.FormatInt(m, 16))
		}
	}
	return joinNames(names)
}

func containsInt64(xs []int64, x int64) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
