package steamcmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Build comparison -- the reason a normal night stops nothing (section 13.5).
//
// `update.sh` stopped the server unconditionally and ran `app_update … validate`
// every night; on a multi-instance box that is N full validations a night and N
// restarts nobody asked for. gvm instead compares the installed build to the
// branch's published build and, on most nights, does nothing at all: state
// `skipped`, which is a first-class success (D-43).
//
// The whole scheme rests on one gotcha. SteamCMD caches app metadata in
// `appinfo.vdf` under HOME, and `app_info_print` serves that cache: without
// deleting it first, the remote buildid comes back stale forever and the
// nightly reports "no update" indefinitely while the server runs an old build.
// LinuxGSM deletes the file before every check. So does PurgeAppInfo, and it is
// called from the one place that asks for a remote build.

// The build-check refusals.
var (
	// ErrNoRemoteBuild is app_info_print returning no buildid for the branch.
	// It is NOT "no update": a check that could not read the remote build must
	// never be reported as a check that found nothing to do.
	ErrNoRemoteBuild = errors.New("steamcmd: app_info_print carried no buildid for this branch")
	// ErrNoSuchBranch is a manifest naming a branch Steam does not publish.
	ErrNoSuchBranch = errors.New("steamcmd: the app has no such branch")
	// ErrPurgeRoot is a refusal to walk a root that is not an absolute path.
	ErrPurgeRoot = errors.New("steamcmd: refusing to search for appinfo.vdf outside an absolute path")
)

// DefaultBranch is Steam's name for "not a beta". It is the literal string
// "public": `app_update <id> -beta public` is how an instance comes BACK off a
// beta, and an empty branch name would silently mean something else.
const DefaultBranch = "public"

// appInfoName is the cache file whose staleness this file exists to defeat.
const appInfoName = "appinfo.vdf"

// PurgeAppInfo deletes every cached appinfo.vdf under root.
//
// Mandatory before app_info_print. Without it Steam serves the cached metadata
// and an update that is available looks like an update that does not exist --
// or, after an update, a build that did nothing looks like a build that worked.
//
// root must be absolute: a relative root would walk whatever directory the
// daemon happens to be in, and the next call after a match here is os.Remove.
// A root that does not exist yet is not an error -- it is the first run.
//
// It returns every path removed, because "the nightly deleted nothing" and "the
// nightly deleted the cache" are different stories when a stale buildid is
// being investigated.
func PurgeAppInfo(root string) ([]string, error) {
	if !filepath.IsAbs(strings.TrimSpace(root)) {
		return nil, fmt.Errorf("%w: %q", ErrPurgeRoot, root)
	}
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("steamcmd: stat %s: %w", root, err)
	}

	var removed []string
	var errs []error
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read may still not contain the cache,
			// and the alternative to continuing is a check that refuses to run
			// because of an unrelated permission problem elsewhere under HOME.
			errs = append(errs, err)
			return nil
		}
		// Type().IsRegular() rather than a stat: a symlink named appinfo.vdf
		// must not be followed, and removing the link itself is correct.
		if d.IsDir() || !d.Type().IsRegular() || !strings.EqualFold(d.Name(), appInfoName) {
			return nil
		}
		if rmErr := os.Remove(p); rmErr != nil {
			errs = append(errs, fmt.Errorf("steamcmd: remove %s: %w", p, rmErr))
			return nil
		}
		removed = append(removed, p)
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	return removed, errors.Join(errs...)
}

// RemoteBuildID reads branches.<branch>.buildid out of one app's app_info
// block.
//
// node is what ParseAppInfo returned. Both spellings are accepted because the
// two callers hold different nodes: the app block (where the path is
// depots.branches.<branch>) and the depots block itself.
func RemoteBuildID(node *KV, branch string) (string, error) {
	if branch == "" {
		branch = DefaultBranch
	}
	branches, ok := node.Lookup("depots", "branches")
	if !ok {
		branches, ok = node.Lookup("branches")
	}
	if !ok {
		return "", fmt.Errorf("%w: the app_info block has no branches section (keys: %v)",
			ErrNoRemoteBuild, node.Keys())
	}
	b, ok := branches.Child(branch)
	if !ok {
		return "", fmt.Errorf("%w: %q; Steam publishes %v", ErrNoSuchBranch, branch, branches.Keys())
	}
	id := strings.TrimSpace(b.Str("buildid"))
	if id == "" {
		return "", fmt.Errorf("%w: branch %q carries no buildid (keys: %v)",
			ErrNoRemoteBuild, branch, b.Keys())
	}
	if !plausibleBuildID(id) {
		// The value lands in BuildCheck.Summary, which is Plan.Reason, which
		// is what the operator reads before authorizing a download. TrimSpace
		// does not touch an interior newline, and the KeyValues lexer
		// materialises \n and \r escapes into real bytes, so a published
		// buildid that is not a run of digits is refused rather than printed.
		// The quoting is %q so the refusal itself cannot carry them.
		return "", fmt.Errorf("%w: branch %q published %q, which is not a build id",
			ErrNoRemoteBuild, branch, id)
	}
	return id, nil
}

// EstimateBytes sums the app's depot sizes, for the disk preflight.
//
// It is the WORST case, not the delta: Steam does not publish how many bytes a
// particular update will transfer, and the figure the preflight needs is the
// one that must not be exceeded. Phase 0 of the update pipeline checks
// max(2 GiB, Bytes * 1.5) against statfs before anything stops, so a generous
// estimate costs a warning and a mean one costs a half-finished download on a
// full disk.
//
// Depots are filtered by config.oslist when the app declares one, because a
// Linux install must not be preflighted against the Windows depot's size.
func EstimateBytes(node *KV, platform string) int64 {
	depots, ok := node.Lookup("depots")
	if !ok {
		return 0
	}
	var total int64
	for _, d := range depots.Children {
		if !d.Block || !isAllDigits(d.Key) {
			// "branches", "baselanguages" and "overridescddb" live beside the
			// numeric depot ids and are not depots.
			continue
		}
		if platform != "" {
			if osList := d.Str("config", "oslist"); osList != "" && !listContains(osList, platform) {
				continue
			}
		}
		size, ok := d.Int("maxsize")
		if !ok {
			size, _ = d.Int("size")
		}
		if size > 0 {
			total += size
		}
	}
	return total
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func listContains(csv, want string) bool {
	for _, el := range strings.Split(csv, ",") {
		if strings.EqualFold(strings.TrimSpace(el), want) {
			return true
		}
	}
	return false
}

// BuildState is the comparison's answer.
type BuildState uint8

const (
	// BuildUnknown is the zero value: no comparison happened. UpdateAvailable
	// is false for it, so a caller that dropped CheckBuild's error cannot read
	// a failed check as a decision -- which is how a check that stopped working
	// becomes a month of nightlies reporting an update that does not exist, or
	// none that does.
	BuildUnknown BuildState = iota
	// BuildNotInstalled is "there is no local build", which is an install, not
	// an update. It is distinct from BuildStale because the operator-facing
	// sentence and the plan differ.
	BuildNotInstalled
	BuildCurrent
	BuildStale
	// BuildAhead is a local build NEWER than the branch's. It happens when an
	// instance was moved off a beta, or when the appinfo cache is still stale
	// despite the purge. It is reported and does not trigger an update: rolling
	// a live world backwards on a nightly timer is not something gvm does
	// without being asked.
	BuildAhead
	// BuildDiffers is "the two ids are not numbers and are not equal".
	BuildDiffers
)

func (s BuildState) String() string {
	switch s {
	case BuildUnknown:
		return "unknown"
	case BuildNotInstalled:
		return "not installed"
	case BuildCurrent:
		return "current"
	case BuildStale:
		return "stale"
	case BuildAhead:
		return "ahead of the branch"
	case BuildDiffers:
		return "differs"
	}
	return "build state(" + strconv.Itoa(int(s)) + ")"
}

// BuildCheck is one comparison of the installed build against the branch's.
type BuildCheck struct {
	Branch string
	Local  string
	Remote string
	State  BuildState
}

// UpdateAvailable reports whether there is a newer build to fetch. BuildAhead
// is deliberately not one.
func (b BuildCheck) UpdateAvailable() bool {
	return b.State == BuildStale || b.State == BuildDiffers || b.State == BuildNotInstalled
}

// Summary is the sentence the job log and the CLI print.
func (b BuildCheck) Summary() string {
	switch b.State {
	case BuildUnknown:
		return fmt.Sprintf("the build on branch %s could not be read, so nothing was compared",
			b.Branch)
	case BuildNotInstalled:
		return fmt.Sprintf("nothing is installed yet; branch %s publishes build %s",
			b.Branch, b.Remote)
	case BuildCurrent:
		return fmt.Sprintf("build %s is current on branch %s", b.Local, b.Branch)
	case BuildStale:
		return fmt.Sprintf("binary update %s -> %s available on branch %s",
			b.Local, b.Remote, b.Branch)
	case BuildAhead:
		return fmt.Sprintf("the installed build %s is NEWER than branch %s's %s: either this "+
			"instance came off a beta or Steam is still serving a cached appinfo",
			b.Local, b.Branch, b.Remote)
	default:
		return fmt.Sprintf("the installed build %q and branch %s's %q are not comparable numbers "+
			"and are not equal", b.Local, b.Branch, b.Remote)
	}
}

// CheckBuild compares an installed build to a published one.
//
// An empty remote is an error rather than "no update": the single most
// expensive outcome in this subsystem is a check that silently fails and
// reports "nothing to do" every night for a month.
func CheckBuild(local, remote, branch string) (BuildCheck, error) {
	if branch == "" {
		branch = DefaultBranch
	}
	local, remote = strings.TrimSpace(local), strings.TrimSpace(remote)
	b := BuildCheck{Branch: branch, Local: local, Remote: remote}
	if remote == "" {
		return b, fmt.Errorf("%w: branch %q", ErrNoRemoteBuild, branch)
	}
	if local == "" {
		b.State = BuildNotInstalled
		return b, nil
	}

	lv, lerr := strconv.ParseUint(local, 10, 64)
	rv, rerr := strconv.ParseUint(remote, 10, 64)
	switch {
	case lerr != nil || rerr != nil:
		if local == remote {
			b.State = BuildCurrent
		} else {
			b.State = BuildDiffers
		}
	case lv == rv:
		b.State = BuildCurrent
	case lv < rv:
		b.State = BuildStale
	default:
		b.State = BuildAhead
	}
	return b, nil
}
