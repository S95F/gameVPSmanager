package archive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// Manifest is .gvm/manifest.json: everything a restore needs to know about an
// archive without a database, a gvm install or the box it came from.
//
// Members is NOT serialised into the head member -- see MembersMember for why
// -- but it is part of this type, so a caller that reads an archive gets one
// value with everything in it rather than two that can disagree.
type Manifest struct {
	Schema     int       `json:"schema"`
	GVMVersion string    `json:"gvm_version"`
	GVMCommit  string    `json:"gvm_commit"`
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  Creator   `json:"created_by"`
	Instance   Subject   `json:"instance"`
	Class      Class     `json:"class"`
	Reason     string    `json:"reason"`

	// SpecYAML is the instance definition with secret VALUES redacted, so the
	// archive is self-describing enough to rebuild the instance on a different
	// box. Secret values travel in etcgvm/secrets/... members only, and only
	// when SecretsIncluded is true.
	SpecYAML string `json:"spec_yaml,omitempty"`
	// ContentLock is a verbatim copy of <name>.lock.json (D-03).
	ContentLock json.RawMessage `json:"content_lock,omitempty"`

	// WorldFingerprint is the summary form: counts, bytes, newest mtime and a
	// digest, for forensics. The path SET that Contains needs is deliberately
	// not here (see Fingerprint.Entries).
	WorldFingerprint *Fingerprint `json:"world_fingerprint,omitempty"`

	// Roots maps each member-path prefix back to the base directory it was
	// taken from. backup.sh could `tar -C $DATA_DIR` for one base and had no
	// way to also carry /etc/gvm; prefixes let one archive span several bases
	// while restore stays unambiguous.
	Roots []Root `json:"roots"`

	// MemberTable names the second metadata member and pins its size, count
	// and digest, so a reader that has only the head can still tell whether
	// the table it later reads is the one this manifest describes.
	MemberTable *MemberTable `json:"member_table,omitempty"`

	SecretsIncluded         bool     `json:"secrets_included"`
	SecretsRedactedFromSpec []string `json:"secrets_redacted_from_spec,omitempty"`

	// Members is populated by ReadArchiveManifest, never by PeekManifest.
	Members []Member `json:"-"`
}

// Creator records who asked for the archive, in the same shape as an audit
// actor. User is a pointer because "no user" (a timer, the root CLI) and "the
// empty user" are different facts and a restore forensics trail must not
// conflate them.
type Creator struct {
	Kind   string  `json:"kind"`
	User   *string `json:"user"`
	Reason string  `json:"reason,omitempty"`
}

// Subject is the instance the archive was taken from.
//
// GameManifestSHA256 and Revision are here because restoring a 2026 world onto
// a 2027 manifest whose shutdown ladder changed is a thing restore must be able
// to warn about (D-17, section 13.2).
type Subject struct {
	Name                 string            `json:"name"`
	Game                 string            `json:"game"`
	GameManifestSHA256   string            `json:"game_manifest_sha256,omitempty"`
	GameManifestRevision int               `json:"game_manifest_revision,omitempty"`
	UnixUser             string            `json:"unix_user,omitempty"`
	Paths                map[string]string `json:"paths,omitempty"`
}

// Root is one base directory and the member-path prefix it was stored under.
type Root struct {
	Prefix string `json:"prefix"`
	Base   string `json:"base"`
}

// MemberTable describes .gvm/members.json from inside the head manifest.
type MemberTable struct {
	Path   string `json:"path"`
	Count  int    `json:"count"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Member kinds. They are strings rather than tar type flags so the manifest
// stays readable by `jq` on a box that has no gvm on it.
const (
	KindFile    = "file"
	KindDir     = "dir"
	KindSymlink = "symlink"
)

// Member is one entry in the archive, with the digest that makes
// stream-verified restore and `gvm backup verify` possible. backup.sh had no
// integrity story at all; this is the whole of the difference.
type Member struct {
	Path  string    `json:"path"` // prefixed, slash-separated, relative
	Kind  string    `json:"kind"`
	Size  int64     `json:"size"`
	Mode  string    `json:"mode"` // "0600", octal, as text
	UID   int       `json:"uid"`
	GID   int       `json:"gid"`
	MTime time.Time `json:"mtime"`
	// SHA256 is set for regular files only. A directory has no contents to
	// hash and a symlink's contents are its target, which Link records.
	SHA256 string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"`
}

// Perm decodes the recorded mode. An unparseable mode is reported as 0, which
// every consumer treats as "leave the platform default alone" rather than as
// "make it inaccessible": a restore that produces unreadable files is a restore
// that did not work.
func (m Member) Perm() fs.FileMode {
	if m.Mode == "" {
		return 0
	}
	v, err := strconv.ParseUint(m.Mode, 8, 32)
	if err != nil {
		return 0
	}
	return fs.FileMode(v) & fs.ModePerm
}

// memberFile is the on-disk shape of .gvm/members.json. It is an object rather
// than a bare array so the file can gain a field later without every reader
// having to guess which shape it is looking at.
type memberFile struct {
	Schema  int      `json:"schema"`
	Count   int      `json:"count"`
	Members []Member `json:"members"`
}

// Validate rejects a manifest that cannot be acted on.
//
// It is called on read, not only on write: the file may have come from another
// box, another gvm version, or a text editor.
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrNoManifest)
	}
	var errs []error
	// Backward-compatible across releases (gvm-self-upgrade-uninstall-10): an
	// archive written by an OLDER gvm is still readable — its schema is at or
	// below this build's, and gvm upgrades it on read. Only a NEWER schema is
	// refused, with the one thing the operator can do about it, because this
	// build genuinely does not know the newer shape and guessing at it could
	// mis-read a world's own backup.
	switch {
	case m.Schema > SchemaVersion:
		errs = append(errs, fmt.Errorf("%w: this archive uses manifest schema %d and this gvm reads "+
			"up to %d; install a newer gvm to read it", ErrSchema, m.Schema, SchemaVersion))
	case m.Schema < 1:
		errs = append(errs, fmt.Errorf("%w: manifest schema %d is not a valid version", ErrSchema, m.Schema))
	}
	if m.Instance.Name == "" {
		errs = append(errs, errors.New("manifest: instance.name is empty; the archive cannot say what it is a backup of"))
	}
	if !m.Class.Valid() {
		errs = append(errs, fmt.Errorf("manifest: class %q is neither %q nor %q; retention "+
			"must never guess, because guessing wrong deletes a pre-rebuild world",
			m.Class, ClassRoutine, ClassPreserve))
	}
	if m.CreatedAt.IsZero() {
		errs = append(errs, errors.New("manifest: created_at is zero; retention orders by it"))
	}
	seen := make(map[string]bool, len(m.Roots))
	for _, r := range m.Roots {
		if r.Base == "" {
			errs = append(errs, fmt.Errorf("manifest: root %q has no base", r.Prefix))
		}
		if r.Prefix != "" && !strings.HasSuffix(r.Prefix, "/") {
			errs = append(errs, fmt.Errorf("manifest: root prefix %q does not end in \"/\", so "+
				"member paths under it are ambiguous", r.Prefix))
		}
		if seen[r.Prefix] {
			errs = append(errs, fmt.Errorf("manifest: root prefix %q appears twice", r.Prefix))
		}
		seen[r.Prefix] = true
	}
	return errors.Join(errs...)
}

// RootFor returns the root whose prefix owns a member path.
//
// The longest matching prefix wins, so "etcgvm/secrets/" nested inside
// "etcgvm/" would resolve to the more specific one rather than to whichever
// happened to be listed first.
func (m *Manifest) RootFor(memberPath string) (Root, bool) {
	best, ok := Root{}, false
	for _, r := range m.Roots {
		if r.Prefix == "" || strings.HasPrefix(memberPath, r.Prefix) {
			if !ok || len(r.Prefix) > len(best.Prefix) {
				best, ok = r, true
			}
		}
	}
	return best, ok
}

// marshalHead renders the head member: everything except the digest table.
func (m *Manifest) marshalHead() ([]byte, error) {
	// Indented, because the DB-free restore story is an operator running
	// `tar -xOf world.tar.gz .gvm/manifest.json` on a box with no jq.
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	b = append(b, '\n')
	if len(b) > maxHeadJSON {
		return nil, fmt.Errorf("%w: head manifest is %d bytes, budget is %d; the oversized "+
			"field is almost always spec_yaml or content_lock, and shrinking it is what keeps "+
			"`gvm restore --list` a short read instead of a full decompression",
			ErrHeadTooLarge, len(b), maxHeadJSON)
	}
	return b, nil
}

// PeekManifest reads the head manifest from at most PeekBytes of a compressed
// archive stream.
//
// This is the function the 8 KB promise is about. It never decompresses past
// the first member, so `gvm restore --list` over twenty nightly archives costs
// twenty short reads.
func PeekManifest(r io.Reader) (*Manifest, error) {
	m, err := readHead(io.LimitReader(r, PeekBytes))
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		// Distinguishable from "no manifest at all", because the operator's
		// next move differs: this one is a gvm archive whose head grew, and
		// ReadArchiveManifest still reads it.
		return nil, fmt.Errorf("%w: the head manifest does not fit in the first %d bytes",
			ErrHeadTooLarge, PeekBytes)
	}
	return m, err
}

// readHead decodes the first tar member of a gzip stream as a manifest.
func readHead(r io.Reader) (*Manifest, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("archive: gunzip: %w", err)
	}
	tr := tar.NewReader(zr)
	hdr, err := tr.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: the archive is empty", ErrNoManifest)
		}
		return nil, err
	}
	if hdr.Name != ManifestMember {
		return nil, fmt.Errorf("%w: first member is %q", ErrNoManifest, hdr.Name)
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return &m, nil
}

// PeekManifestFile is PeekManifest against a path, which is what every caller
// outside a test actually has.
func PeekManifestFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return PeekManifest(f)
}

// ReadArchiveManifest reads the head manifest AND the digest table.
//
// It decompresses only as far as the second member, so it is still cheap
// against a large archive -- but it is not the PeekBytes promise, and callers
// that only need to list an archive should use PeekManifest.
func ReadArchiveManifest(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("archive: gunzip %s: %w", path, err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	hdr, err := tr.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: %s is empty", ErrNoManifest, path)
		}
		return nil, err
	}
	if hdr.Name != ManifestMember {
		return nil, fmt.Errorf("%w: first member of %s is %q", ErrNoManifest, path, hdr.Name)
	}
	head, err := io.ReadAll(tr)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(head, &m); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, path, err)
	}
	if m.MemberTable == nil {
		return &m, nil
	}

	hdr, err = tr.Next()
	if err != nil {
		return nil, fmt.Errorf("%w: %s names a member table it does not contain: %v",
			ErrCorrupt, path, err)
	}
	if hdr.Name != m.MemberTable.Path {
		return nil, fmt.Errorf("%w: %s: second member is %q, manifest names %q",
			ErrCorrupt, path, hdr.Name, m.MemberTable.Path)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		return nil, err
	}
	if got := hexSHA256(body); got != m.MemberTable.SHA256 {
		return nil, fmt.Errorf("%w: %s: member table digest is %s, manifest records %s",
			ErrCorrupt, path, got, m.MemberTable.SHA256)
	}
	var mf memberFile
	if err := json.Unmarshal(body, &mf); err != nil {
		return nil, fmt.Errorf("%w: %s: member table: %v", ErrCorrupt, path, err)
	}
	if mf.Count != len(mf.Members) || mf.Count != m.MemberTable.Count {
		return nil, fmt.Errorf("%w: %s: member table holds %d entries, declares %d, manifest records %d",
			ErrCorrupt, path, len(mf.Members), mf.Count, m.MemberTable.Count)
	}
	// A table naming one path twice cannot describe a tar stream unambiguously.
	// The check is here rather than in Validate because Verify records a
	// Validate failure and then carries on regardless -- and what it carries on
	// into is a `want` map that silently keeps only the last of the pair, and a
	// spot selection whose bound is the entry count while its result is keyed by
	// path. Validate already refuses a duplicate ROOT prefix for the same
	// reason.
	seen := make(map[string]bool, len(mf.Members))
	for _, mem := range mf.Members {
		if seen[mem.Path] {
			return nil, fmt.Errorf("%w: %s: the member table lists %q twice",
				ErrCorrupt, path, mem.Path)
		}
		seen[mem.Path] = true
	}
	m.Members = mf.Members
	return &m, nil
}
