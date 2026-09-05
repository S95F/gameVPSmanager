package pz

import (
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// maxModInfoDepth bounds how deep a mod.info may be nested inside a workshop
// item, matching the find -maxdepth 5 the bash used. Workshop items nest as
// <id>/mods/<ModName>/mod.info, and B42 items add a build folder on top of
// that; anything deeper is not a mod layout PZ recognises.
const maxModInfoDepth = 5

// ModInfo is one mod.info file found inside a workshop item.
type ModInfo struct {
	// Path is the mod.info location, relative to the workshop item directory.
	Path string
	// ID is the internal mod id, the value that belongs in Mods=.
	ID string
	// Name is the human-readable mod name, for display only.
	Name string
	// B42Score is how strongly this looks like a Build 42 variant.
	B42Score int
}

var (
	reModInfoID   = regexp.MustCompile(`(?i)^[ \t]*id[ \t]*=(.*)$`)
	reModInfoName = regexp.MustCompile(`(?i)^[ \t]*name[ \t]*=(.*)$`)
	reVersionMin  = regexp.MustCompile(`(?i)^[ \t]*versionMin[ \t]*=[ \t]*4[2-9]`)
)

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

// b42Score rates how strongly a mod.info looks like a Build 42 variant.
//
// Many workshop items ship a B41 and a B42 build side by side, each with its
// own mod.info and its own internal id. Enabling the B41 id on a B42 server
// is the silent-failure case this scoring exists to avoid.
//
//	3 = explicit: it lives in a 42/ folder, or declares versionMin 42+
//	1 = B42-style layout: it ships common/ or 42/ media directories
//	0 = no evidence either way
func b42Score(fsys fs.FS, infoPath string, data []byte) int {
	dir := path.Dir(infoPath)
	if path.Base(dir) == "42" {
		return 3
	}
	for _, line := range strings.Split(string(data), "\n") {
		if reVersionMin.MatchString(line) {
			return 3
		}
	}
	for _, sub := range []string{"42", "common"} {
		if st, err := fs.Stat(fsys, path.Join(dir, sub)); err == nil && st.IsDir() {
			return 1
		}
	}
	return 0
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
// files.
//
// Roots are searched in order and the first one holding an item wins, because
// the server writes workshop content to different locations depending on
// version and both may exist on the same box.
func Resolve(roots []WorkshopRoot, workshopIDs []string) *Resolution {
	res := &Resolution{WorkshopIDs: append([]string(nil), workshopIDs...)}
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

		infos := findModInfos(fsys)
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
			if in.B42Score > best {
				best = in.B42Score
			}
		}
		for _, in := range infos {
			if in.B42Score == best {
				item.ModIDs = append(item.ModIDs, in.ID)
			} else {
				item.Dropped = append(item.Dropped, in.ID)
			}
		}

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
func findModInfos(fsys fs.FS) []ModInfo {
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
		if d.Name() != "mod.info" {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		info := ParseModInfo(data)
		if info.ID == "" {
			return nil // no id means nothing usable for Mods=
		}
		info.Path = p
		info.B42Score = b42Score(fsys, p, data)
		out = append(out, info)
		return nil
	})

	// WalkDir is already lexical, but sorting explicitly keeps the output
	// stable if the walk is ever swapped for something else. Resolution order
	// decides which duplicate id wins, so it must not drift.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
