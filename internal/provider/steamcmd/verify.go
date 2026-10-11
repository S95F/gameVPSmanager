package steamcmd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// D-32: the post-download check that pays for removing the nightly `validate`.
//
// `update.sh` ran `app_update 380870 validate` every night, and that validate
// is what silently repaired half-finished downloads. gvm cannot keep it --
// validate overwrites files the operator customised, and Valve says so
// explicitly -- so `validate_on` containing `update` is a load-time error and
// this check is mandatory instead: after EVERY install and update, and on every
// doctor run.
//
// A failure here is never auto-remediated with `validate`. It sets
// NEEDS_REPAIR, the server starts anyway, and the operator is told to run
// `gvm update <n> --repair`, which is the only path that emits validate, takes
// a preserve archive first, and says which files it is about to risk.
//
// Every failure gets its OWN error, because each one means something different
// and tells the operator to do something different:
//
//	no appmanifest    nothing was ever installed here, or the install tree moved
//	two appmanifests  two installs are fighting over one tree
//	StateFlags != 4   Steam itself says the app is not fully installed
//	download short    the download stopped early: steamcmd said Success and the
//	                  tree is half a gigabyte light
//	stage short       the bytes arrived and were never committed into place
//	sentinel          the tree exists and the thing that launches the game does not
//	wrong build       steamcmd exited without an error and moved nothing, or Steam
//	                  itself still has an update outstanding for this app
//	undeclared check  the tree may be fine; it is the PINNED MANIFEST that cannot
//	                  be trusted, so the remedy is `gvm apply` and not `--repair`
//
// A shared "verification failed" would tell them none of that.

// The verification refusals.
var (
	ErrNoAppManifest    = errors.New("steamcmd: no appmanifest for this app in the install tree")
	ErrManyAppManifests = errors.New(
		"steamcmd: more than one appmanifest for this app in the install tree")
	ErrStateFlags      = errors.New("steamcmd: Steam does not report this app as fully installed")
	ErrBytesDownloaded = errors.New("steamcmd: the download did not finish")
	ErrBytesStaged     = errors.New("steamcmd: the download was not fully staged into place")
	ErrSentinel        = errors.New("steamcmd: the manifest's install sentinel is not usable")
	ErrTreeUnreadable  = errors.New("steamcmd: the install tree could not be read")
	// ErrWrongBuild is the installed build not being the build this run was
	// authorized to fetch. Its own error because every other failure here is
	// about a tree that is damaged, and this one is about a tree that is intact
	// and was never touched.
	ErrWrongBuild = errors.New(
		"steamcmd: the installed build is not the build this run was authorized to fetch")
	// ErrBuildID is an appmanifest whose build id is not a build id. Its own
	// error because it does not mean the download failed: it means something
	// that was not Steam wrote this file, which is a different investigation
	// and the one D-32 exists to surface.
	ErrBuildID = errors.New("steamcmd: the appmanifest's buildid is not a build id")
	// ErrUndeclaredCheck is a pinned manifest that does not declare one of the
	// two mandatory D-32 checks. Its own error because the remedy is unlike
	// every other failure here: nothing may be wrong with the tree, and what
	// has to be fixed is the pin.
	ErrUndeclaredCheck = errors.New(
		"steamcmd: the pinned manifest does not declare a mandatory D-32 check")
)

// VerifyRequest is one verification.
//
// Tree is an fs.FS rooted at the install directory rather than a path, which is
// what makes every rule in this file testable on the Windows development
// machine: the real driver passes os.DirFS over the jailed install path, and
// the tests pass an in-memory tree whose modes they choose. Windows has no
// execute bit, so the sentinel's +x rule could not otherwise be exercised until
// it ran on the box it matters on.
type VerifyRequest struct {
	Tree  fs.FS
	AppID int
	// Branch is recorded in the facts, not checked: the appmanifest does not
	// name the branch it came from.
	Branch   string
	Sentinel spec.Sentinel
	// WantBuild is the build this run was authorized to fetch, or "" for "not
	// asserted" -- which is every doctor run and every IntentVerify, where
	// there is no plan and so no authorization to compare against.
	//
	// Without it, "we downloaded everything" and "we downloaded nothing" are
	// the same answer: the appmanifest is self-consistent either way, and
	// SteamCMD can print Success and exit 0 having moved nothing. The
	// comparison is MONOTONIC rather than an equality, because Steam can
	// publish a newer build during a multi-hour 65 GB download and failing
	// that run would recommend `--repair`, which overwrites customised files.
	WantBuild string
	// Verify is the manifest's declaration. appmanifest and byte_parity are
	// MANDATORY for every steamcmd manifest, so an undeclared one is a
	// FAILURE -- recorded as "not declared", which keeps "passed", "failed"
	// and "not asked" three distinguishable states. The distinguishability
	// lives in the Problem sentence and in ErrUndeclaredCheck, never in a nil:
	// with both flags off the only surviving evidence is the single-ACF guard
	// and the sentinel, which is exactly the "is the directory non-empty"
	// standard D-32 exists to reject. The schema refuses a manifest without
	// them; a pin written by an older gvm never had to carry them, and
	// spec.LoadPinned does not re-run the checker.
	//
	// The 0-or-more-than-one appmanifest guard is not optional and single_acf
	// does not switch it off: the StateFlags and byte checks are undefined
	// without exactly one file to read, and a manifest cannot opt out of
	// arithmetic.
	Verify spec.Verify
}

// Report is what verification found, whether or not it passed.
//
// It is returned even on failure, so the job record carries the numbers that
// explain the verdict rather than just the verdict. Nothing in it is derived
// twice: provider.SentinelResult holds the sentinel facts and OK() turns them
// into the sentence, exactly as the seam intends.
type Report struct {
	AppID    int
	Branch   string
	ACFPath  string   // the one appmanifest, as a slash path inside the tree
	ACFPaths []string // every match, when there was not exactly one
	Manifest *AppManifest
	Sentinel provider.SentinelResult
	// WantBuild is echoed from the request so the job record says what the
	// installed build was compared against, or "" when nothing was asserted.
	WantBuild string
	// Problems are the operator-facing sentences, in check order.
	Problems []string
	// Checked is false only when nothing could be looked at at all.
	Checked bool

	errs []error
}

// Err is the joined distinct failures, or nil.
func (r *Report) Err() error { return errors.Join(r.errs...) }

// Failed reports whether anything is wrong.
func (r *Report) Failed() bool { return r != nil && len(r.errs) > 0 }

// Version is the installed build, as the provider seam spells it.
func (r *Report) Version() provider.Version {
	if r == nil || r.Manifest == nil {
		return provider.Version{}
	}
	return provider.Version{
		ID:    r.Manifest.BuildID,
		Label: r.Manifest.Name,
		At:    r.Manifest.LastUpdated,
	}
}

// Integrity fills the seam's D-32 record.
//
// Facts carry the numbers as strings and Problems the failures, per the seam's
// contract. Checked:false with a Reason is how "nobody looked" stays
// distinguishable from "nothing was wrong".
func (r *Report) Integrity() provider.IntegrityResult {
	if r == nil {
		return provider.IntegrityResult{
			Checked: false,
			Reason:  "verification did not run",
		}
	}
	if !r.Checked {
		reason := "the install tree could not be read, so nothing was checked"
		if len(r.Problems) > 0 {
			reason = r.Problems[0]
		}
		return provider.IntegrityResult{Checked: false, Reason: reason, Problems: r.Problems}
	}

	facts := map[string]string{
		"appid":       fmt.Sprint(r.AppID),
		"appmanifest": r.ACFPath,
		"hand_edits": "not derivable: SteamCMD publishes no per-file digests, and the only way to " +
			"find a hand edit is `validate`, which overwrites it (D-32)",
	}
	if r.Branch != "" {
		facts["branch"] = r.Branch
	}
	if r.WantBuild != "" {
		facts["want_buildid"] = r.WantBuild
	}
	if m := r.Manifest; m != nil {
		facts["state_flags"] = StateDescription(m.StateFlags)
		facts["buildid"] = m.BuildID
		if m.BuildIDSuspect {
			// A fact about the refusal, not the refused value: these facts are
			// printed and stored verbatim.
			facts["buildid"] = "refused: not a plausible build id"
		}
		if m.TargetBuildID != "" {
			facts["target_buildid"] = m.TargetBuildID
		}
		facts["bytes_downloaded"] = fmt.Sprintf("%d/%d", m.BytesDownloaded, m.BytesToDownload)
		facts["bytes_staged"] = fmt.Sprintf("%d/%d", m.BytesStaged, m.BytesToStage)
		facts["size_on_disk"] = spec.FormatBytes(m.SizeOnDisk)
		if !m.LastUpdated.IsZero() {
			facts["last_updated"] = m.LastUpdated.Format("2006-01-02T15:04:05Z")
		}
	}
	return provider.IntegrityResult{Checked: true, Problems: r.Problems, Facts: facts}
}

// Verify performs the whole D-32 check.
//
// The error is the joined set of distinct sentinels above; the Report is always
// returned, even on failure.
func Verify(req VerifyRequest) (*Report, error) {
	r := &Report{AppID: req.AppID, Branch: req.Branch, WantBuild: req.WantBuild}
	if req.Tree == nil {
		r.fail(fmt.Errorf("%w: no install tree was given to verify", ErrTreeUnreadable),
			"no install tree was given to verify")
		return r, r.Err()
	}

	matches, walkErr := findAppManifests(req.Tree, req.AppID)
	if walkErr != nil {
		r.fail(fmt.Errorf("%w: %v", ErrTreeUnreadable, walkErr),
			"the install tree could not be walked: "+walkErr.Error())
		// Keep going. A tree that cannot be walked still has a sentinel path
		// worth stat-ing, and "the directory is absent" is a better message
		// when it says both halves.
	}
	r.ACFPaths = matches
	r.Checked = walkErr == nil

	switch len(matches) {
	case 0:
		r.fail(fmt.Errorf("%w: looked for %s under the install tree", ErrNoAppManifest,
			appManifestName(req.AppID)),
			fmt.Sprintf("%s is absent: Steam has no record of installing app %d here, so either "+
				"nothing was ever installed in this tree or the install path changed",
				appManifestName(req.AppID), req.AppID))
	case 1:
		r.ACFPath = matches[0]
		r.readManifest(req)
	default:
		r.fail(fmt.Errorf("%w: %s", ErrManyAppManifests, strings.Join(matches, ", ")),
			fmt.Sprintf("%d copies of %s are in this tree (%s): two installs are sharing one "+
				"install directory and will overwrite each other. Give each instance its own "+
				"install path, delete the stray copy, and re-run the install",
				len(matches), appManifestName(req.AppID), strings.Join(matches, ", ")))
	}

	r.Sentinel = sentinelResult(req.Tree, req.Sentinel)
	if err := r.Sentinel.OK(); err != nil {
		r.fail(fmt.Errorf("%w: %v", ErrSentinel, err), "sentinel: "+err.Error())
	}
	return r, r.Err()
}

// readManifest runs checks 2, 3, 4 and 6 of section 13.5's list.
func (r *Report) readManifest(req VerifyRequest) {
	data, err := fs.ReadFile(req.Tree, r.ACFPath)
	if err != nil {
		r.fail(fmt.Errorf("%w: read %s: %v", ErrNoAppManifest, r.ACFPath, err),
			fmt.Sprintf("%s could not be read: %v", r.ACFPath, err))
		return
	}
	m, err := ParseAppManifest(bytes.NewReader(data))
	if err != nil {
		r.fail(fmt.Errorf("%w: %s: %v", ErrNoAppManifest, r.ACFPath, err),
			fmt.Sprintf("%s is not a readable appmanifest (%v), so Steam's own record of this "+
				"install is unusable", r.ACFPath, err))
		return
	}
	r.Manifest = m

	if m.AppID != req.AppID {
		// The filename and the contents disagree, which means something copied
		// an appmanifest around. Reported as the StateFlags failure's sibling
		// rather than silently trusting either number.
		r.fail(fmt.Errorf("%w: %s declares appid %d", ErrNoAppManifest, r.ACFPath, m.AppID),
			fmt.Sprintf("%s declares appid %d, not %d: this file was copied here from another "+
				"install", r.ACFPath, m.AppID, req.AppID))
		return
	}

	r.checkBuild(req, m)

	if req.Verify.AppManifest {
		switch {
		case !m.HasStateFlags:
			r.fail(fmt.Errorf("%w: %s carries no StateFlags", ErrStateFlags, r.ACFPath),
				fmt.Sprintf("%s carries no StateFlags key, so Steam's own verdict on this "+
					"install is unavailable", r.ACFPath))
		case m.StateFlags != StateInstalled:
			r.fail(fmt.Errorf("%w: StateFlags is %s, and only %d is fully installed",
				ErrStateFlags, StateDescription(m.StateFlags), StateInstalled),
				fmt.Sprintf("StateFlags is %s. Only %d means fully installed with nothing "+
					"outstanding; this install is not usable as it stands. Run "+
					"`gvm update <instance> --repair`, which validates and takes an archive first",
					StateDescription(m.StateFlags), StateInstalled))
		}
	} else {
		// The rule this file already applies to the sentinel one screen lower
		// ("a verification that cannot tell FAILS. It never passes"), applied
		// to the other two mandatory checks.
		r.fail(fmt.Errorf("%w: verify.appmanifest", ErrUndeclaredCheck),
			"StateFlags was not checked: the pinned manifest does not declare "+
				"verify.appmanifest, so Steam's own verdict on this install was never read. "+
				"The schema requires this flag; re-run `gvm apply <instance>` to re-validate "+
				"and re-pin the manifest (D-32)")
	}

	if req.Verify.ByteParity {
		r.checkBytes(m)
	} else {
		r.fail(fmt.Errorf("%w: verify.byte_parity", ErrUndeclaredCheck),
			"the byte pairs were not checked: the pinned manifest does not declare "+
				"verify.byte_parity, so a download that stopped half a gigabyte short would "+
				"pass. The schema requires this flag; re-run `gvm apply <instance>` to "+
				"re-validate and re-pin the manifest (D-32)")
	}
}

// checkBuild is the sixth rule: did the build actually move?
//
// Two independent questions. The first compares the installed build against the
// build the plan was authorized to fetch, and is asked only when a plan said
// so. The second reads Steam's OWN marker -- TargetBuildID, which Steam writes
// when an update begins and leaves equal to buildid when one finishes -- and is
// asked on every path, including the doctor run where there is no WantBuild.
//
// Both exist because without them the appmanifest is self-consistent whether
// the download transferred 65 GB or nothing at all: steamcmd can print Success
// and exit 0 having moved nothing, and a deterministic cause (an unreadable
// runscript, a bad ReadWritePaths derivation, a re-exec that drops +runscript)
// then makes every nightly stop the server, report success and restart it on
// the old build.
func (r *Report) checkBuild(req VerifyRequest, m *AppManifest) {
	if m.BuildIDSuspect {
		// A FIXED sentence, never the refused bytes: the whole reason this is
		// a failure is that the bytes are hostile, and quoting them into the
		// verdict line is the thing being prevented. The raw value stays in
		// the file and in m.Root for anyone investigating with a hex dump.
		r.fail(fmt.Errorf("%w: %s", ErrBuildID, r.ACFPath),
			fmt.Sprintf("%s records a buildid that is not a plausible build id (Steam writes a "+
				"short run of digits): something other than Steam wrote this file. The install "+
				"tree is writable by the instance account, so a compromised workshop item is "+
				"the first thing to rule out; the raw value is in the file and is deliberately "+
				"not repeated here", r.ACFPath))
		// The comparisons below both read a build id, and neither has one.
		return
	}

	// Gated on Installed() because StateFlags already carries "update
	// required": reporting the same outstanding update as two failures would
	// make neither of them tell the operator anything the other did not.
	if m.Installed() && !m.TargetSettled() {
		r.fail(fmt.Errorf("%w: %s records TargetBuildID %s against buildid %s",
			ErrWrongBuild, r.ACFPath, m.TargetBuildID, m.BuildID),
			fmt.Sprintf("Steam still has an update outstanding for this app: the appmanifest "+
				"calls the install complete on build %s and records %s as the build it was "+
				"moving to. An update was started and did not land; run "+
				"`gvm update <instance> --repair`, which takes an archive first (D-32)",
				m.BuildID, m.TargetBuildID))
	}

	if req.WantBuild == "" {
		return
	}
	chk, err := CheckBuild(m.BuildID, req.WantBuild, req.Branch)
	if err != nil {
		// An empty WantBuild is the only thing CheckBuild refuses, and the
		// caller has already been excluded above.
		r.fail(fmt.Errorf("%w: %v", ErrWrongBuild, err),
			"the installed build could not be compared with the build this run was "+
				"authorized to fetch: "+err.Error())
		return
	}
	switch chk.State {
	case BuildCurrent:
	case BuildAhead:
		// Steam published again during the download. buildid.go already treats
		// a local build newer than the branch's as reportable rather than
		// wrong, and failing it here would set NEEDS_REPAIR and recommend
		// `--repair`, which validates and overwrites customised files.
		r.Problems = append(r.Problems, fmt.Sprintf("the installed build %s is newer than the "+
			"%s this run planned for: Steam published again while the download was running",
			m.BuildID, req.WantBuild))
	case BuildDiffers:
		// Neither id came from gvm's own arithmetic, so there is nothing to
		// conclude beyond recording both.
		r.Problems = append(r.Problems, fmt.Sprintf("the installed build %q and the planned %q "+
			"are not comparable numbers and are not equal", m.BuildID, req.WantBuild))
	default:
		r.fail(fmt.Errorf("%w: %s is installed and this run was authorized to fetch %s",
			ErrWrongBuild, m.BuildID, req.WantBuild),
			fmt.Sprintf("the install is still on build %s and this run was authorized to fetch "+
				"%s: steamcmd exited without reporting an error and moved nothing. Nothing was "+
				"downloaded, so starting the server leaves it on the old build; check that the "+
				"runscript at the path the job wrote is readable by the instance account and "+
				"re-run the update", m.BuildID, req.WantBuild))
	}
}

// checkBytes is the pair of checks that catch "steamcmd said Success and the
// install is half a gigabyte short".
//
// The two pairs are separate failures because they fail at different points:
// BytesDownloaded short means the transfer stopped, BytesStaged short means the
// bytes arrived and were never moved into place -- usually a disk that filled
// during the commit, which leaves a tree that looks complete and is not.
func (r *Report) checkBytes(m *AppManifest) {
	if !m.HasDownloadPair {
		r.fail(fmt.Errorf("%w: %s carries no BytesToDownload/BytesDownloaded pair",
			ErrBytesDownloaded, r.ACFPath),
			fmt.Sprintf("%s carries no BytesToDownload/BytesDownloaded pair, so the download "+
				"cannot be shown to have finished. An absent number is not a passing check",
				r.ACFPath))
	} else if m.BytesDownloaded != m.BytesToDownload {
		r.fail(fmt.Errorf("%w: BytesDownloaded %d != BytesToDownload %d",
			ErrBytesDownloaded, m.BytesDownloaded, m.BytesToDownload),
			fmt.Sprintf("the download is short: %s of %s arrived (%d of %d bytes). SteamCMD can "+
				"print Success and exit 0 after this, which is why gvm reads the appmanifest "+
				"instead of the exit code",
				spec.FormatBytes(m.BytesDownloaded), spec.FormatBytes(m.BytesToDownload),
				m.BytesDownloaded, m.BytesToDownload))
	}

	if !m.HasStagePair {
		r.fail(fmt.Errorf("%w: %s carries no BytesToStage/BytesStaged pair",
			ErrBytesStaged, r.ACFPath),
			fmt.Sprintf("%s carries no BytesToStage/BytesStaged pair, so the staging step "+
				"cannot be shown to have finished", r.ACFPath))
	} else if m.BytesStaged != m.BytesToStage {
		r.fail(fmt.Errorf("%w: BytesStaged %d != BytesToStage %d",
			ErrBytesStaged, m.BytesStaged, m.BytesToStage),
			fmt.Sprintf("the staging step is short: %s of %s was committed into place (%d of %d "+
				"bytes). The bytes were downloaded and not moved into the tree, which usually "+
				"means the disk filled during the commit",
				spec.FormatBytes(m.BytesStaged), spec.FormatBytes(m.BytesToStage),
				m.BytesStaged, m.BytesToStage))
	}
}

func (r *Report) fail(err error, problem string) {
	r.errs = append(r.errs, err)
	r.Problems = append(r.Problems, problem)
}

// sentinelResult fills the seam's record for check 5: the manifest-declared
// file that must exist afterwards, and must be executable when the manifest
// says so (PZ: start-server.sh -- exactly install.sh line 110's check).
//
// It FILLS provider.SentinelResult rather than deciding anything, because the
// verdict logic belongs to the seam: internal/cli has a pre-provider stopgap of
// the same shape and a third copy of the rules is how the three answers start
// disagreeing.
func sentinelResult(tree fs.FS, s spec.Sentinel) provider.SentinelResult {
	rel := path.Clean(strings.TrimPrefix(strings.ReplaceAll(s.Path, `\`, "/"), "./"))
	out := provider.SentinelResult{
		Declared:       strings.TrimSpace(s.Path) != "",
		Rel:            rel,
		WantExecutable: s.Executable,
		MinBytes:       s.MinBytes,
	}
	// A path that escapes the tree is already a load-time error on the
	// manifest, so this is the defensive half: an unresolvable sentinel leaves
	// Declared false, which OK() reads as "gvm cannot tell" -- and a
	// verification that cannot tell FAILS. It never passes.
	if !out.Declared || !fs.ValidPath(rel) {
		out.Declared = out.Declared && fs.ValidPath(rel)
		return out
	}
	fi, err := fs.Stat(tree, rel)
	if err != nil {
		return out
	}
	out.Present = true
	out.IsDir = fi.IsDir()
	out.Size = fi.Size()
	out.Executable = fi.Mode().Perm()&0o111 != 0
	return out
}

func appManifestName(appid int) string {
	return fmt.Sprintf("appmanifest_%d.acf", appid)
}

// CanonicalACF is where Steam puts the appmanifest. Observe reads this path
// directly: it runs every thirty seconds and must not walk a 65 GB tree.
func CanonicalACF(appid int) string {
	return path.Join("steamapps", appManifestName(appid))
}

// findAppManifests looks for every copy of this app's appmanifest.
//
// Zero and more-than-one are both real, recurring failure modes, so the search
// cannot stop at the first hit. Two directories are skipped: steamapps/workshop
// holds third-party content, and an item that happened to ship a copy of this
// app's manifest would otherwise be reported as a second install; and
// steamapps/downloading is SteamCMD's own staging area, whose contents are
// mid-flight by definition.
//
// The walk does not follow symlinks (fs.WalkDir never does), which is what
// keeps a symlinked mods directory from taking the search into somebody's home.
func findAppManifests(tree fs.FS, appid int) ([]string, error) {
	want := appManifestName(appid)
	skip := map[string]bool{
		"steamapps/workshop":    true,
		"steamapps/downloading": true,
	}
	var out []string
	err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			// A subdirectory gvm cannot read is a finding, not a reason to
			// abandon the search: the appmanifest may still be elsewhere, and a
			// partial answer here would be reported as "no appmanifest", which
			// means something different.
			return nil
		}
		if d.IsDir() {
			if skip[p] {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(d.Name(), want) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	// Canonical first, so the one that gets read is the one Steam wrote.
	canon := CanonicalACF(appid)
	for i, p := range out {
		if p == canon && i != 0 {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out, nil
}
