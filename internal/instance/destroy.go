package instance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// The preflight's refusals. They are separate sentinels because the operator's
// next move differs for each: a missing archive means the receipt is stale and
// the job must archive again, an archive inside the destroy set means the
// backup directory is misconfigured, and a space refusal means the destroy
// would be one the operator could not undo.
var (
	// ErrArchiveMissing means the file the Ref names is not on disk, is not a
	// regular file, or is not the size the Ref recorded. A receipt for a file
	// that has since been deleted or rewritten is not a receipt: verification
	// happened at some earlier moment, and this is the check that the moment
	// still applies.
	ErrArchiveMissing = errors.New("instance: the archive named by the ref is not on disk as recorded")

	// ErrArchiveInTarget means the archive lives inside the destroy set, so
	// completing the destroy would delete the only copy of what it destroyed.
	// It is the one way archive-before-destroy can be satisfied on paper and
	// violated in fact.
	ErrArchiveInTarget = errors.New("instance: the archive is inside the destroy set")

	// ErrDiskPreflight is section 13.6 step 7's space refusal: after this
	// destroy there would not be room to put the world back.
	ErrDiskPreflight = errors.New("instance: not enough free space to restore this archive afterwards")
)

// RestoreHeadroom is section 13.6 step 7's `estimate * 1.2`.
//
// The 20% is not superstition. A restore streams every member into a staging
// directory on the destination filesystem and renames only after the whole
// stream verifies, so at the moment of the rename the filesystem is holding
// the restored tree AND whatever the destination already had; and an
// allocation-group-fragmented ext4 at 100% is a filesystem that fails writes
// well before df says zero.
const RestoreHeadroom = 1.2

// VolumeCheck is the space arithmetic for one allow root's filesystem.
//
// Checked is false when the platform cannot answer -- Statfs is Linux-only and
// this package is developed on Windows. A refusal based on the ABSENCE of
// information would be a refusal that fires on the development box and never
// on the target, which is the wrong way round for a safety check, so an
// unanswerable volume is reported rather than failed.
type VolumeCheck struct {
	Root     string `json:"root"`
	Freeing  int64  `json:"freeing"`  // bytes this destroy would release here
	Avail    int64  `json:"avail"`    // bytes free before the destroy
	Required int64  `json:"required"` // bytes a restore would need here, with headroom
	Checked  bool   `json:"checked"`
}

// Fits reports whether a restore would fit on this volume once the destroy has
// released its bytes. An unchecked volume fits by definition: see Checked.
func (v VolumeCheck) Fits() bool {
	return !v.Checked || saturatingAdd(v.Avail, v.Freeing) >= v.Required
}

// Preflight is everything Destroy checks before it removes anything, and
// everything `gvm rebuild --dry` should print about step 8.
//
// It is a value rather than a bool because the operator's question is never
// "may I" alone -- it is "what is about to go, how big is it, and can I put it
// back". Destroy answers the first; this answers all three, without deleting
// anything, which is what makes a dry run worth having.
type Preflight struct {
	// Archive is the receipt's path and size on disk, re-stated after being
	// checked rather than copied from the Ref.
	Archive      string `json:"archive"`
	ArchiveBytes int64  `json:"archive_bytes"`

	// Estimate is what a restore of this archive would write. It comes from
	// the archive's own head manifest when it has one; EstimateIsFloor says it
	// fell back to the COMPRESSED size, which is a floor and not a prediction.
	Estimate        int64 `json:"estimate"`
	EstimateIsFloor bool  `json:"estimate_is_floor"`

	// Paths, Bytes, Files and Dirs are the destroy set as measured now.
	Paths []string `json:"paths"`
	Bytes int64    `json:"bytes"`
	Files int      `json:"files"`
	Dirs  int      `json:"dirs"`

	Volumes []VolumeCheck `json:"volumes"`
}

// Check returns the refusal, or nil when the destroy may proceed.
func (p *Preflight) Check() error {
	for _, v := range p.Volumes {
		if v.Fits() {
			continue
		}
		short := v.Required - saturatingAdd(v.Avail, v.Freeing)
		return fmt.Errorf("%w: %s has %s free and this destroy releases %s, but restoring %s "+
			"would need %s -- %s short",
			ErrDiskPreflight, v.Root, bytesHuman(v.Avail), bytesHuman(v.Freeing),
			p.Archive, bytesHuman(v.Required), bytesHuman(short))
	}
	return nil
}

// Destroy removes an instance's data. It is the only function in gvm that does.
//
// The two arguments are the whole design:
//
//   - *archive.Ref is archive-before-destroy encoded as a type. A caller that
//     has not written and verified an archive has nothing to pass, and a nil
//     one is refused before anything is touched. This is rebuild.sh's ordering
//     invariant -- the archive is taken after content verification and before
//     deletion -- promoted from a comment in a shell script to a parameter.
//
//   - []Jailed is the destroy jail. Jailed's fields are unexported, so the
//     only way to obtain one is Jail.Resolve, which has already refused "..",
//     symlink escapes, world paths, ancestors of world paths, paths outside
//     the allow roots and paths across a mount. Destroy therefore cannot be
//     called with a string, and there is no argument to have about whether the
//     caller checked: it could not have got here otherwise.
//
// It PANICS rather than returning an error when the receipt is missing or
// unverified, per section 13.1. Reaching it that way is a programming error,
// not an operating condition: a panic inside a job is recovered, audited and
// reported, whereas a returned error is something a future caller can route
// around with an `if err != nil { log }`. DestroyPreflight is the same set of
// checks in the form that ANSWERS instead of destroying, and that one returns
// an error naming what was missing -- a dry run asking "would this work" must
// not be able to crash the process by asking badly.
//
// Everything the preflight finds is a returned error, because those ARE
// operating conditions: an archive somebody moved, a backup directory
// misconfigured inside the world, a volume too full to undo the destroy.
func Destroy(ctx context.Context, ref *archive.Ref, paths []Jailed) error {
	if ref == nil {
		panic("instance: Destroy called with a nil archive ref; nothing is destroyed " +
			"before it is archived (section 13.1)")
	}
	if !ref.Verified() {
		panic(fmt.Sprintf("instance: Destroy called with an unverified archive ref %q; "+
			"a truncated tarball is exactly the failure archive-before-destroy prevents", ref.Path))
	}
	if len(paths) == 0 {
		return fmt.Errorf("%w: no paths; a destroy that removes nothing must not report success",
			ErrJail)
	}
	for i, p := range paths {
		if !p.Valid() {
			panic(fmt.Sprintf("instance: Destroy called with a zero Jailed at index %d; "+
				"the only legitimate source of a Jailed is Jail.Resolve", i))
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	pf, err := destroyPreflight(ref, paths, platform.Statfs)
	if err != nil {
		return err
	}
	if err := pf.Check(); err != nil {
		return err
	}
	// Only now, and through the chokepoint: archive.Destroy re-asserts the
	// receipt and the proof of every target, so the guarantee does not depend
	// on this function having got the order right.
	if _, err := archive.Destroy(ctx, ref, paths2targets(paths)); err != nil {
		return err
	}
	return nil
}

// DestroyPreflight answers what Destroy would do, and refuses in the form a
// caller can print.
//
// It exists so `gvm rebuild --dry` can show the destroy set and the space
// arithmetic without the operator having to trust a description of them, and
// so the checks live in exactly one place: Destroy calls this and then acts on
// the answer.
func DestroyPreflight(ref *archive.Ref, paths []Jailed) (*Preflight, error) {
	return destroyPreflight(ref, paths, platform.Statfs)
}

// DestroyPreflightWith is DestroyPreflight with the statfs seam supplied.
//
// It is exported for the same reason the unexported form takes the seam as a
// parameter: platform.Statfs is Linux-only, so on the development box every
// volume comes back unchecked and Fits() is true by definition. A caller that
// wants its handling of a space REFUSAL tested has to be able to produce one.
func DestroyPreflightWith(ref *archive.Ref, paths []Jailed, statfs func(string) (platform.DiskUsage, error)) (*Preflight, error) {
	return destroyPreflight(ref, paths, statfs)
}

// destroyPreflight takes the statfs seam as a parameter rather than reading a
// package variable, so the space arithmetic is tested on a box that has no
// statfs at all without two tests ever racing over a global.
func destroyPreflight(ref *archive.Ref, paths []Jailed, statfs func(string) (platform.DiskUsage, error)) (*Preflight, error) {
	switch {
	case ref == nil:
		return nil, fmt.Errorf("%w: no archive ref; nothing is destroyed before it is archived",
			ErrArchiveMissing)
	case ref.Path == "":
		return nil, fmt.Errorf("%w: the archive ref names no path", ErrArchiveMissing)
	case !ref.Verified():
		return nil, fmt.Errorf("%w: the archive %q was written but never verified",
			ErrArchiveMissing, ref.Path)
	case len(paths) == 0:
		return nil, fmt.Errorf("%w: no paths; a destroy that removes nothing must not report success",
			ErrJail)
	}
	for i, p := range paths {
		if !p.Valid() {
			return nil, fmt.Errorf("%w: a zero Jailed at index %d; the only legitimate source of "+
				"a Jailed is Jail.Resolve", ErrJail, i)
		}
	}

	archivePath, size, err := checkArchiveOnDisk(ref)
	if err != nil {
		return nil, err
	}
	pf := &Preflight{Archive: archivePath, ArchiveBytes: size}
	pf.Estimate, pf.EstimateIsFloor = restoreEstimate(ref, archivePath, size)

	freeing := map[string]int64{}
	var roots []string
	for _, p := range paths {
		if under(archivePath, p.Path()) {
			return nil, fmt.Errorf("%w: %s is inside %s, so completing this destroy would delete "+
				"the only copy of what it destroyed", ErrArchiveInTarget, archivePath, p.Path())
		}
		files, dirs, bytes, err := archive.Measure(p.Path())
		if err != nil {
			return nil, fmt.Errorf("instance: measure %s before destroying it: %w", p.Path(), err)
		}
		pf.Paths = append(pf.Paths, p.Path())
		pf.Files += files
		pf.Dirs += dirs
		pf.Bytes += bytes
		if _, seen := freeing[p.Root()]; !seen {
			roots = append(roots, p.Root())
		}
		freeing[p.Root()] += bytes
	}

	for _, root := range roots {
		v := VolumeCheck{Root: root, Freeing: freeing[root]}
		// The restore lands back where the data came from, so each volume is
		// asked for its own SHARE of the estimate -- apportioned by how much
		// of the destroy set lives there. Charging every volume the whole
		// archive would refuse a perfectly restorable two-root destroy.
		v.Required = platform.Required(apportion(pf.Estimate, freeing[root], pf.Bytes), RestoreHeadroom)
		du, err := statfs(root)
		switch {
		case err == nil:
			v.Avail = saturatingInt64(du.Avail)
			v.Checked = true
		case errors.Is(err, platform.ErrNotSupported):
			// Left unchecked on purpose: see VolumeCheck.Checked.
		default:
			return nil, fmt.Errorf("instance: statfs %s for the destroy preflight: %w", root, err)
		}
		pf.Volumes = append(pf.Volumes, v)
	}
	return pf, nil
}

// checkArchiveOnDisk re-stats the receipt.
//
// Verification happened at some earlier moment -- in a rebuild, one step
// earlier; from a plan, possibly minutes. Between then and now retention, an
// operator's `rm`, or a half-finished rsync can have taken the file away or
// changed it, and every one of those turns a verified Ref into a promise about
// bytes that are gone. This is the check that the earlier moment still applies.
func checkArchiveOnDisk(ref *archive.Ref) (path string, size int64, err error) {
	path = filepath.Clean(ref.Path)
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %s: %v", ErrArchiveMissing, path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%w: %s is not a regular file", ErrArchiveMissing, path)
	}
	if fi.Size() == 0 {
		return "", 0, fmt.Errorf("%w: %s is empty", ErrArchiveMissing, path)
	}
	if ref.Bytes > 0 && fi.Size() != ref.Bytes {
		return "", 0, fmt.Errorf("%w: %s is %d bytes, but was verified at %d",
			ErrArchiveMissing, path, fi.Size(), ref.Bytes)
	}
	// Resolved AFTER the stat, so a symlinked backup directory that points
	// into the world is caught by the containment check rather than hidden by
	// it. A path that cannot be resolved keeps its cleaned form; the stat
	// above already proved something is there.
	if real, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
		path = real
	}
	return path, fi.Size(), nil
}

// restoreEstimate is how many bytes putting this archive back would write.
//
// The head manifest carries the world fingerprint's uncompressed byte count and
// is readable from the first 8 KB, so the good answer costs one short read. The
// fallback is the COMPRESSED size, which is a floor -- a 2.4 GB world in a
// 700 MB tarball would be under-estimated by a factor of three -- and that is
// why the caller is told which one it got instead of being handed a number that
// looks equally trustworthy either way.
func restoreEstimate(ref *archive.Ref, path string, size int64) (estimate int64, isFloor bool) {
	if m, err := archive.PeekManifestFile(path); err == nil {
		if m.WorldFingerprint != nil && m.WorldFingerprint.Bytes > 0 {
			return m.WorldFingerprint.Bytes, false
		}
	}
	if ref.Bytes > 0 {
		return ref.Bytes, true
	}
	return size, true
}

// paths2targets widens []Jailed to []archive.Target.
//
// Go will not do this implicitly, and that is fortunate: the conversion is the
// one place a caller could try to slip something else in, and there is nowhere
// else in the package that builds this slice.
func paths2targets(paths []Jailed) []archive.Target {
	targets := make([]archive.Target, len(paths))
	for i, p := range paths {
		targets[i] = p
	}
	return targets
}

// apportion splits total in the ratio part/whole, in float64 because the
// integer form (total*part) overflows int64 at terabyte scale.
func apportion(total, part, whole int64) int64 {
	switch {
	case total <= 0 || part <= 0:
		return 0
	case whole <= 0 || part >= whole:
		return total
	}
	return int64(math.Ceil(float64(total) * (float64(part) / float64(whole))))
}

func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func saturatingInt64(u uint64) int64 {
	if u > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(u)
}

// bytesHuman renders a size the way a refusal should read. It is deliberately
// coarse: an operator deciding whether to free space needs "2.4 GB", not
// 2251799813 with the digits to count.
func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
