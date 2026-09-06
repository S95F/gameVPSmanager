package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Index is archives.index.json: the listing beside the archives themselves.
//
// It exists because `gvm restore --list` and `gvm backup prune` must work with
// no database configured at all (section 14.6), and because peeking twenty
// tarballs to answer "what have I got" is twenty file opens that a single
// 4 KB read can replace. It is a CACHE and never a source of truth: Reindex
// rebuilds it from the archives, the archives are never rebuilt from it, and
// an entry whose file has vanished is dropped rather than believed.
type Index struct {
	Schema    int       `json:"schema"`
	Instance  string    `json:"instance"`
	UpdatedAt time.Time `json:"updated_at"`
	Entries   []Entry   `json:"entries"`
}

// Entry is one archive as the index remembers it.
type Entry struct {
	File   string `json:"file"` // basename, so the directory can be moved
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
	// ModTime and Bytes together are the cache key: Reindex only re-hashes a
	// file whose size or mtime has changed, which is what keeps a nightly
	// reindex from reading every archive on the box.
	ModTime time.Time `json:"mtime"`

	Instance   string    `json:"instance,omitempty"`
	Class      Class     `json:"class,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	VerifiedAt time.Time `json:"verified_at,omitempty"`
	Members    int       `json:"members,omitempty"`

	// Legacy marks an archive with no manifest -- a bash-suite tarball, or
	// anything else somebody dropped in the directory. Retention leaves these
	// alone entirely.
	Legacy bool `json:"legacy,omitempty"`
	// Unreadable records why an archive could not be peeked, so a corrupt
	// file shows up in a listing instead of silently vanishing from it.
	Unreadable string `json:"unreadable,omitempty"`
}

// Ref rebuilds the receipt shape from an index entry, for a caller that wants
// to verify an archive it found rather than one it just wrote.
//
// VerifiedAt is carried across, so an entry the index has never seen verified
// produces a Ref that cannot authorize a destroy -- which is correct: the
// index is a cache, and a cache must not be able to vouch for bytes.
func (e Entry) Ref(dir string) *Ref {
	return &Ref{
		Path:       filepath.Join(dir, e.File),
		SHA256:     e.SHA256,
		Bytes:      e.Bytes,
		Instance:   e.Instance,
		Class:      e.Class,
		Reason:     e.Reason,
		CreatedAt:  e.CreatedAt,
		VerifiedAt: e.VerifiedAt,
	}
}

// ReindexOptions tunes a rescan.
type ReindexOptions struct {
	// Rehash forces the whole-file digest to be recomputed even for files the
	// index already knows at the same size and mtime.
	Rehash bool
	Now    func() time.Time
	OnNote func(string)
}

// IndexPath is where the index for a directory lives.
func IndexPath(dir string) string { return filepath.Join(dir, IndexFile) }

// LoadIndex reads the index, returning an empty one when there is none.
//
// A missing index is not an error: a freshly provisioned box has archives
// before it has an index, and a restore run from a USB stick has neither.
func LoadIndex(dir string) (*Index, error) {
	b, err := os.ReadFile(IndexPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return &Index{Schema: SchemaVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var ix Index
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil, fmt.Errorf("archive: parse %s: %w", IndexPath(dir), err)
	}
	if ix.Schema != SchemaVersion {
		return nil, fmt.Errorf("%w: index schema %d, this build reads %d", ErrSchema, ix.Schema, SchemaVersion)
	}
	return &ix, nil
}

// Save writes the index atomically.
//
// WriteFileAtomicNoBackup, not WriteFileAtomic: a .bak beside the archives
// would be a second file in a directory whose contents retention globs, and
// nothing wants "archives.index.json.bak" to be mistaken for an archive or to
// linger with a stale listing.
func (ix *Index) Save(dir string) error {
	ix.Schema = SchemaVersion
	b, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return spec.WriteFileAtomicNoBackup(IndexPath(dir), b, ArchiveFileMode)
}

// Find returns the entry for a basename.
func (ix *Index) Find(file string) (Entry, bool) {
	for _, e := range ix.Entries {
		if e.File == file {
			return e, true
		}
	}
	return Entry{}, false
}

// Reindex rescans a directory and rewrites archives.index.json.
//
// It peeks each archive's head manifest rather than reading it whole, so a
// directory of twenty 2.4 GB nightlies is scanned in twenty short reads plus
// however many whole-file digests actually changed.
func Reindex(ctx context.Context, dir string, o ReindexOptions) (*Index, error) {
	now := clockOf(o.Now)
	prev, err := LoadIndex(dir)
	if err != nil {
		// A corrupt index must not stop a reindex: rebuilding it is exactly
		// the repair, and refusing to would leave the operator with a broken
		// cache and no way to replace it.
		if o.OnNote != nil {
			o.OnNote(fmt.Sprintf("discarding unreadable %s: %v", IndexPath(dir), err))
		}
		prev = &Index{Schema: SchemaVersion}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	ix := &Index{Schema: SchemaVersion, UpdatedAt: now().UTC()}
	for _, de := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, Ext) || strings.HasPrefix(name, ".") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(dir, name)
		e := Entry{
			File:    name,
			Bytes:   fi.Size(),
			ModTime: fi.ModTime().UTC().Truncate(time.Second),
		}
		if old, ok := prev.Find(name); ok && !o.Rehash &&
			old.Bytes == e.Bytes && old.ModTime.Equal(e.ModTime) {
			e.SHA256 = old.SHA256
			e.VerifiedAt = old.VerifiedAt
		}

		man, perr := PeekManifestFile(full)
		switch {
		case perr == nil:
			e.Instance = man.Instance.Name
			e.Class = man.Class
			e.Reason = man.Reason
			e.CreatedAt = man.CreatedAt
			if man.MemberTable != nil {
				e.Members = man.MemberTable.Count
			}
			if ix.Instance == "" {
				ix.Instance = man.Instance.Name
			}
		case errors.Is(perr, ErrNoManifest):
			e.Legacy = true
			// The filename is all a legacy tarball offers, and backup.sh's
			// stamp is not in it in a form this can parse. mtime is the only
			// honest ordering key.
			e.CreatedAt = e.ModTime
		default:
			e.Unreadable = perr.Error()
			e.CreatedAt = e.ModTime
			if o.OnNote != nil {
				o.OnNote(fmt.Sprintf("%s could not be read: %v", name, perr))
			}
		}
		if e.SHA256 == "" {
			sum, herr := hashFile(ctx, full)
			if herr != nil {
				e.Unreadable = herr.Error()
			} else {
				e.SHA256 = sum
			}
		}
		ix.Entries = append(ix.Entries, e)
	}

	// Newest first: that is the order every listing wants and the order
	// retention walks.
	sort.Slice(ix.Entries, func(i, j int) bool {
		if !ix.Entries[i].CreatedAt.Equal(ix.Entries[j].CreatedAt) {
			return ix.Entries[i].CreatedAt.After(ix.Entries[j].CreatedAt)
		}
		return ix.Entries[i].File > ix.Entries[j].File
	})
	if err := ix.Save(dir); err != nil {
		return nil, err
	}
	return ix, nil
}

// Note records a verification against the index, so `gvm backup list` can show
// when each archive was last read back rather than only when it was written.
func (ix *Index) Note(ref *Ref) {
	if ref == nil {
		return
	}
	base := filepath.Base(ref.Path)
	for i := range ix.Entries {
		if ix.Entries[i].File == base {
			ix.Entries[i].VerifiedAt = ref.VerifiedAt
			if ref.SHA256 != "" {
				ix.Entries[i].SHA256 = ref.SHA256
			}
			return
		}
	}
}
