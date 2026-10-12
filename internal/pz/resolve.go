package pz

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// maxModInfoDepth bounds how deep a mod.info may be nested inside a workshop
// item, matching the find -maxdepth 5 the bash used. Workshop items nest as
// <id>/mods/<ModName>/mod.info, and B42 items add a build folder on top of
// that; anything deeper is not a mod layout PZ recognises.
const maxModInfoDepth = 5

// maxModInfoBytes bounds one mod.info read.
//
// A real mod.info is a few hundred bytes; this is three orders of magnitude
// above that, so nothing legitimate is clipped. The bound exists because the
// declared read roots live inside the instance's own install and data trees,
// which ARCHITECTURE 18.1's first row hands outright to a compromised workshop
// item -- so `mod.info` is a name third-party bytes get to choose. The same
// reasoning as provider/steamcmd's maxKVBytes: a parser that will allocate
// whatever it is handed is a denial of service with a friendly name. Here the
// bytes are re-materialised twice more as strings inside the parse, so the real
// cost is a multiple of the file.
const maxModInfoBytes = 1 << 20

// Build is the Project Zomboid major build a resolve targets.
//
// It exists because an instance may legitimately be a Build 41 server: the
// scoring below answers "which variant inside this item is for MY build", and
// that question has no answer until the build is named. Resolve keeps
// targeting B42, which is what every existing caller means.
type Build int

const (
	BuildB41 Build = 41
	BuildB42 Build = 42
)

func (b Build) String() string { return strconv.Itoa(int(b)) }

// ParseBuild decodes a `resolver_vars.target_build` value.
//
// It returns an error and never a default. A typo'd target that fell through
// to B42 on a B41 server would enable the B42 variant of every dual-build
// item, which is the silent-omission failure this file exists to prevent --
// except pointing the other way.
func ParseBuild(s string) (Build, error) {
	switch strings.TrimSpace(s) {
	case "41":
		return BuildB41, nil
	case "42":
		return BuildB42, nil
	}
	return 0, fmt.Errorf("pz: %q is not a target build (want 41 or 42)", s)
}

// ModInfo is one mod.info file found inside a workshop item.
type ModInfo struct {
	// Path is the mod.info location, relative to the workshop item directory.
	Path string
	// ID is the internal mod id, the value that belongs in Mods=.
	ID string
	// Name is the human-readable mod name, for display only.
	Name string
	// BuildScore is how strongly this looks like a variant for the TARGET
	// build. Comparable only within one item and one target.
	BuildScore int
	// BuildMarker names what produced the score ("42-dir",
	// "versionMin>=42", "common-layout", "no-build-marker"), so a caller can
	// record WHY an id was kept or dropped. A surprising inclusion months
	// later is otherwise unexplainable without re-running the scan.
	BuildMarker string
}

var (
	reModInfoID    = regexp.MustCompile(`(?i)^[ \t]*id[ \t]*=(.*)$`)
	reModInfoName  = regexp.MustCompile(`(?i)^[ \t]*name[ \t]*=(.*)$`)
	reVersionMin   = regexp.MustCompile(`(?i)^[ \t]*versionMin[ \t]*=[ \t]*4[2-9]`)
	reVersionMin41 = regexp.MustCompile(`(?i)^[ \t]*versionMin[ \t]*=[ \t]*41`)
)

// buildMarkers describes, per target build, what the files have to say for a
// variant to count as explicitly that build's.
//
// B42's entry reproduces the original b42Score exactly: a 42/ directory or
// versionMin 42+ is explicit, and a 42/ or common/ media directory is the
// weaker layout hint. B41's entry deliberately does NOT credit common/: a
// common/ plus 42/ split is the B42 restructure, and giving it a point on a
// B41 target would promote the B42 variant on a B41 server.
var buildMarkers = map[Build]struct {
	dir        string
	versionMin *regexp.Regexp
	layout     []string
}{
	BuildB42: {dir: "42", versionMin: reVersionMin, layout: []string{"42", "common"}},
	BuildB41: {dir: "41", versionMin: reVersionMin41, layout: []string{"41"}},
}

// ParseModInfo extracts the fields gvm needs from a mod.info.
//
// Only the first assignment of each key is honoured, matching grep -m1: some
// mod.info files repeat keys, and the game takes the first.
func ParseModInfo(data []byte) ModInfo {
	var info ModInfo
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if info.ID == "" {
			if m := reModInfoID.FindStringSubmatch(line); m != nil {
				info.ID = strings.TrimSpace(m[1])
				continue
			}
		}
		if info.Name == "" {
			if m := reModInfoName.FindStringSubmatch(line); m != nil {
				info.Name = strings.TrimSpace(m[1])
			}
		}
	}
	return info
}

// buildScore rates how strongly a mod.info looks like a variant for target.
//
// Many workshop items ship a B41 and a B42 build side by side, each with its
// own mod.info and its own internal id. Enabling the wrong one is the
// silent-failure case this scoring exists to avoid: the item downloads, the
// mod never loads, and nothing is logged.
//
//	3 = explicit: it lives in a <target>/ folder, or declares versionMin for it
//	1 = target-style layout: it ships the directories that build splits into
//	0 = no evidence either way
//
// The second return value names the marker that produced the score, so a
// caller can record it as evidence instead of asserting a conclusion it cannot
// show its work for.
func buildScore(fsys fs.FS, infoPath string, data []byte, target Build) (int, string) {
	m, ok := buildMarkers[target]
	if !ok {
		// An unknown target scores everything zero, which makes every item
		// ambiguous and therefore loudly warned about. Silently falling back
		// to B42 would be the one outcome nobody could see.
		return 0, "unknown-target"
	}

	dir := path.Dir(infoPath)
	if path.Base(dir) == m.dir {
		return 3, m.dir + "-dir"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if m.versionMin.MatchString(line) {
			return 3, "versionMin>=" + m.dir
		}
	}
	for _, sub := range m.layout {
		if st, err := fs.Stat(fsys, path.Join(dir, sub)); err == nil && st.IsDir() {
			return 1, sub + "-layout"
		}
	}
	return 0, "no-build-marker"
}

// ItemStatus is the resolution outcome for one workshop item.
type ItemStatus string

const (
	// StatusOK means exactly one mod id was resolved with positive evidence.
	StatusOK ItemStatus = "ok"
	// StatusMulti means the item legitimately contains several mods, all kept.
	StatusMulti ItemStatus = "multi"
	// StatusCheck means no build marker was found anywhere in the item, so
	// every id inside it was enabled and a B41 variant may have slipped in.
	StatusCheck ItemStatus = "check"
	// StatusMissing means the item has not been downloaded yet.
	StatusMissing ItemStatus = "missing"
	// StatusEmpty means the folder exists but contains no usable mod.info.
	StatusEmpty ItemStatus = "empty"
)

// ItemResult is the per-item resolution report.
type ItemResult struct {
	WorkshopID string
	Status     ItemStatus
	// ModIDs are the internal ids selected for Mods=.
	ModIDs []string
	// Dropped are ids rejected as lower-scoring build variants, kept for the
	// report so a surprising exclusion can be explained.
	Dropped []string
	// Dir is where the item was found, for diagnostics.
	Dir string
	// Score is the winning build score inside this item, and Markers are the
	// markers the winners carried. Reported rather than inferred from Status:
	// "ok" covers both an explicit versionMin and a layout hint, and those are
	// different strengths of evidence when someone asks months later why an id
	// is in Mods=.
	Score   int
	Markers []string
}

// Resolution is the whole sync-mods report.
type Resolution struct {
	Items []ItemResult
	// ModIDs is the deduplicated, load-ordered list for Mods=.
	ModIDs []string
	// WorkshopIDs is every id from mods.txt, in order, for WorkshopItems=.
	// Items that failed to download are deliberately still listed: that is
	// what tells the server to retry the download on its next boot.
	WorkshopIDs []string
	Missing     []string
	Ambiguous   []string
	// Target is the build this resolution scored against, recorded so a report
	// can say which question was asked.
	Target Build
}

// ModsLine renders the Mods= value.
//
// Both PZ lists are semicolon-separated. A comma silently breaks the list
// with no error, which is a popular way to disable every mod at once by
// accident.
func (r *Resolution) ModsLine() string { return strings.Join(r.ModIDs, ";") }

// WorkshopItemsLine renders the WorkshopItems= value.
func (r *Resolution) WorkshopItemsLine() string { return strings.Join(r.WorkshopIDs, ";") }

// WorkshopRoot is one directory that may contain downloaded workshop items,
// each in a subdirectory named after its numeric id.
type WorkshopRoot struct {
	// Name identifies the root in diagnostics, normally its absolute path.
	Name string
	FS   fs.FS
}

// Resolve maps workshop ids to internal mod ids by reading the downloaded
// files, targeting Build 42.
//
// Roots are searched in order and the first one holding an item wins, because
// the server writes workshop content to different locations depending on
// version and both may exist on the same box.
func Resolve(roots []WorkshopRoot, workshopIDs []string) *Resolution {
	return ResolveFor(roots, workshopIDs, BuildB42)
}

// ResolveFor is Resolve against a named target build.
//
// Variant selection stays purely file-based: the highest-scoring variants
// inside one item win and the losers land in Dropped. target only decides
// which build's markers count, so a B41 instance asks the same question about
// its own files rather than being handed B42's answer.
func ResolveFor(roots []WorkshopRoot, workshopIDs []string, target Build) *Resolution {
	res := &Resolution{WorkshopIDs: append([]string(nil), workshopIDs...), Target: target}
	seen := map[string]bool{}

	for _, wid := range workshopIDs {
		item := ItemResult{WorkshopID: wid}

		fsys, dir, ok := findItem(roots, wid)
		if !ok {
			item.Status = StatusMissing
			res.Missing = append(res.Missing, wid)
			res.Items = append(res.Items, item)
			continue
		}
		item.Dir = dir

		infos := findModInfos(fsys, target)
		if len(infos) == 0 {
			item.Status = StatusEmpty
			res.Missing = append(res.Missing, wid)
			res.Items = append(res.Items, item)
			continue
		}

		// Keep only the best-scoring candidates. A dual-build item collapses
		// to its B42 variant, while an item genuinely holding two B42 mods
		// keeps both.
		best := 0
		for _, in := range infos {
			if in.BuildScore > best {
				best = in.BuildScore
			}
		}
		markers := map[string]bool{}
		for _, in := range infos {
			if in.BuildScore == best {
				item.ModIDs = append(item.ModIDs, in.ID)
				if in.BuildMarker != "" && !markers[in.BuildMarker] {
					markers[in.BuildMarker] = true
					item.Markers = append(item.Markers, in.BuildMarker)
				}
			} else {
				item.Dropped = append(item.Dropped, in.ID)
			}
		}
		item.Score = best
		sort.Strings(item.Markers)

		switch {
		case best == 0:
			item.Status = StatusCheck
			res.Ambiguous = append(res.Ambiguous, wid)
		case len(item.ModIDs) > 1:
			item.Status = StatusMulti
		default:
			item.Status = StatusOK
		}

		for _, id := range item.ModIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			res.ModIDs = append(res.ModIDs, id)
		}
		res.Items = append(res.Items, item)
	}

	return res
}

func findItem(roots []WorkshopRoot, id string) (fs.FS, string, bool) {
	for _, root := range roots {
		st, err := fs.Stat(root.FS, id)
		if err != nil || !st.IsDir() {
			continue
		}
		sub, err := fs.Sub(root.FS, id)
		if err != nil {
			continue
		}
		return sub, path.Join(root.Name, id), true
	}
	return nil, "", false
}

// findModInfos walks a workshop item for mod.info files, in a stable order.
func findModInfos(fsys fs.FS, target Build) []ModInfo {
	var out []ModInfo

	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree must not abort the whole item
		}
		if d.IsDir() {
			if strings.Count(p, "/")+1 > maxModInfoDepth {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// `mod.info` is a name third-party bytes choose, under a tree the
			// instance account owns (ARCHITECTURE 18.1). A FIFO named mod.info
			// blocks the open forever, which holds the job lease with the
			// server stopped -- a hang rather than a failure, and the one
			// outcome the shutdown ladder cannot recover from. A symlink named
			// mod.info is a read of a file the item did not ship. Same check,
			// and the same reason, as steamcmd/buildid.go's walk of a
			// Steam-written tree.
			return nil
		}
		if d.Name() != "mod.info" {
			return nil
		}
		data, err := readBounded(fsys, p)
		if err != nil {
			return nil
		}
		info := ParseModInfo(data)
		if info.ID == "" {
			return nil // no id means nothing usable for Mods=
		}
		info.Path = p
		info.BuildScore, info.BuildMarker = buildScore(fsys, p, data, target)
		out = append(out, info)
		return nil
	})

	// WalkDir is already lexical, but sorting explicitly keeps the output
	// stable if the walk is ever swapped for something else. Resolution order
	// decides which duplicate id wins, so it must not drift.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// readBounded reads one mod.info, refusing anything over maxModInfoBytes.
//
// It reads the bound PLUS ONE and compares, rather than trusting a LimitReader
// that stops exactly at the bound: a reader that stops at the limit returns a
// truncated file and no error, and a truncated mod.info parses into a plausible
// ModInfo with whatever id happened to fit. The same reasoning
// content/fetch's bodied reader records.
//
// An item whose only mod.info is skipped resolves to no ids, which the content
// resolver already renders as `modinfo:no-usable-mod-info`, so the skip is
// reported rather than silent.
func readBounded(fsys fs.FS, p string) ([]byte, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxModInfoBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxModInfoBytes {
		return nil, fmt.Errorf("pz: %s is over %d bytes, which is not a mod.info", p,
			maxModInfoBytes)
	}
	return data, nil
}
