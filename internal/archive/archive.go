// Package archive is the one chokepoint every destructive path in gvm goes
// through (section 13.1).
//
// Ref is what makes archive-before-destroy a type rather than a review
// comment: Destroy takes a *Ref, so a caller that has not archived has nothing
// to pass, and a Ref whose VerifiedAt is zero is not a receipt for anything.
//
// This package deliberately does not import internal/instance. The typed
// destroy front door lives there (instance.Destroy takes []instance.Jailed,
// per section 13.6 step 8), so an archive-side Destroy taking []instance.Jailed
// would close an import cycle between the two packages. Destroy here takes the
// Target interface instead, which instance.Jailed satisfies without either
// package knowing about the other.
package archive

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is the .gvm/manifest.json schema this build writes.
//
// A reader refuses a schema it does not know rather than guessing, because the
// whole point of the manifest is that an operator can restore from the tarball
// with no database, no gvm install and no notes -- and a half-understood
// manifest is worse than an honest refusal to read one.
const SchemaVersion = 1

// The two metadata members, and the promise about where they sit.
const (
	// ManifestMember is the FIRST member of every gvm archive, and the
	// compressor is flushed immediately after it, so the head manifest is
	// readable from the first PeekBytes of the file. `gvm restore --list`
	// against a 2.4 GB world therefore costs one short read rather than a
	// full decompression.
	ManifestMember = ".gvm/manifest.json"

	// MembersMember is the per-member digest table, written second.
	//
	// Section 13.1 draws members[] inside manifest.json. It cannot live there
	// and also honour the 8 KB promise above: a Project Zomboid world is 8412
	// files, which is roughly 1.5 MB of JSON. Splitting the table out is the
	// only way to have both, and the head manifest names it, sizes it and
	// carries its sha256 so the split cannot go unnoticed by a reader.
	MembersMember = ".gvm/members.json"

	// MetaPrefix is the reserved namespace inside an archive. Nothing from a
	// source tree may land under it, because a member called
	// ".gvm/manifest.json" arriving from a world directory would be a
	// self-describing archive describing itself falsely.
	MetaPrefix = ".gvm/"
)

// PeekBytes is how much of the compressed file PeekManifest is allowed to read.
//
// It is a budget, not a hint: Create refuses to finish an archive whose head
// manifest cannot be read back within it, so the promise is enforced by the
// writer instead of hoped for by the reader.
const PeekBytes = 8 << 10

// maxHeadJSON bounds the head manifest before compression.
//
// The check has to be on the uncompressed bytes because it runs before the
// compressor sees them, and an uncompressed bound implies the compressed one:
// deflate's worst case is a stored block, a fraction of a percent over the
// input. The slack between this and PeekBytes covers the tar header, the gzip
// header and that fraction, with room left over.
const maxHeadJSON = 6 << 10

// Ext is the extension every gvm archive carries. Kept identical to the bash
// suite's, so an operator's existing `ls /var/backups` habits still work.
const Ext = ".tar.gz"

// Stamp is the timestamp format in an archive's filename. It is backup.sh's
// `date +%Y%m%d-%H%M%S`, in UTC, so a box that changes timezone does not
// produce a directory listing that sorts out of order.
const Stamp = "20060102-150405"

// IndexFile is the DB-free listing beside the archives (index.go).
const IndexFile = "archives.index.json"

// The package's refusals. They are separate sentinels because the operator's
// next move differs for each.
var (
	// ErrNoManifest means the archive carries no .gvm/manifest.json. It is
	// not fatal: a bash-suite tarball restores through the legacy path, with
	// reduced verification and a warning.
	ErrNoManifest = errors.New("archive: no .gvm/manifest.json")
	// ErrManifestNotFirst means a .gvm/manifest.json exists but is not the
	// first member, so the 8 KB promise does not hold for this file.
	ErrManifestNotFirst = errors.New("archive: .gvm/manifest.json is not the first member")
	// ErrSchema is a manifest from a future gvm.
	ErrSchema = errors.New("archive: unsupported manifest schema")
	// ErrCorrupt means the bytes in the archive do not match what its own
	// manifest says they should be. This is the failure archive-before-destroy
	// exists to catch.
	ErrCorrupt = errors.New("archive: content does not match its manifest")
	// ErrManifestDrift is a game-manifest sha256 mismatch between the archive
	// and the instance it is being restored onto (section 13.2).
	ErrManifestDrift = errors.New("archive: game manifest differs from the one recorded in the archive")
	// ErrUnsafeMember is a member path that would extract outside its root:
	// an absolute path, a "..", or a symlink pointing out of the tree.
	ErrUnsafeMember = errors.New("archive: member path would extract outside its root")
	// ErrNoFreshArchive is retention's refusal to prune without a successful
	// create to prune after (section 13.2).
	ErrNoFreshArchive = errors.New("archive: prune requires a freshly written and verified archive")
	// ErrHeadTooLarge means the head manifest would not be readable from the
	// first PeekBytes, which would silently break DB-free restore.
	ErrHeadTooLarge = errors.New("archive: head manifest exceeds the peek budget")
	// ErrDrifted means a source file changed between the digest pass and the
	// write pass. The freeze lease (D-31) is what normally prevents it.
	ErrDrifted = errors.New("archive: a source file changed while it was being archived")
	// ErrArchiveExists means an archive of this instance, class and second is
	// already on disk. Filename() has one-second resolution and no nonce, so
	// two creates inside one second name the same file; promoting over it
	// would unlink a good archive with nothing recorded anywhere. It is a
	// distinct sentinel so a nightly job can tell "you already have this
	// second's archive" from a write failure, and log rather than page.
	ErrArchiveExists = errors.New("archive: an archive with this name already exists")
)

// Class decides what retention may do with an archive.
//
// It is a class rather than a bool because prune must be able to tell "last
// night's nightly, delete it when the newer one lands" from "the world as it
// stood the moment before a rebuild deleted it" -- and only one of those is
// ever safe to reclaim automatically (section 13.1, D-43's sibling rule that
// prune never touches preserve).
type Class string

const (
	// ClassRoutine is a scheduled backup. Retention prunes these.
	ClassRoutine Class = "routine"
	// ClassPreserve is taken before something irreversible: a rebuild, an
	// adopt cutover, a repair. Retention never prunes these, not even with
	// --force.
	ClassPreserve Class = "preserve"
)

// Valid reports whether c is one of the two classes retention knows.
//
// An unrecognised class must never be treated as routine by default: that is
// the one direction in which a mistake deletes a world.
func (c Class) Valid() bool { return c == ClassRoutine || c == ClassPreserve }

// Ref is the receipt for an archive that exists on disk and has been verified.
//
// It is the token Destroy demands. A caller holding one has, by construction,
// already written and checked the bytes it is about to delete.
type Ref struct {
	// Path is the absolute path of the tarball.
	Path string
	// SHA256 is over the whole file, hex-encoded.
	SHA256 string
	// Bytes is the archive's size on disk, for the "old world: 2.4 GB" line
	// rebuild prints when it is done.
	Bytes int64
	// Instance is the instance the archive was taken from.
	Instance string
	// Class decides whether retention may ever reclaim it.
	Class Class
	// Reason is the human sentence recorded in the audit log and in the
	// archive's own manifest ("pre-rebuild", "adopt-pre-cutover").
	Reason string
	// CreatedAt is when the tarball finished writing.
	CreatedAt time.Time
	// VerifiedAt is when its contents were last checked against the
	// per-member digests. The zero value means "never", and a Ref that has
	// never been verified is not a receipt for anything: a truncated tarball
	// is exactly the failure archive-before-destroy exists to prevent.
	VerifiedAt time.Time
}

// Verified reports whether this Ref may authorize a destroy.
func (r *Ref) Verified() bool {
	return r != nil && r.Path != "" && !r.VerifiedAt.IsZero()
}

// IsPreserve reports whether retention must leave this archive alone forever.
func (r *Ref) IsPreserve() bool { return r != nil && r.Class == ClassPreserve }

// Filename is the name Create writes an archive under.
//
// Instance first, then a UTC timestamp, then the class: a plain `ls` in
// /var/backups/gvm/<name> is therefore already in chronological order, and the
// class is visible without opening anything -- which is what an operator wants
// at 2am when deciding what they are allowed to delete by hand.
func Filename(instance string, class Class, at time.Time) string {
	return instance + "-" + at.UTC().Format(Stamp) + "-" + string(class) + Ext
}

// ParseFilename reads back what Filename wrote.
//
// ok is false for anything else in the directory, including a bash-suite
// tarball. That is deliberate and load-bearing for retention: prune must be
// able to tell an archive gvm wrote from one it merely found, because deleting
// somebody else's backups on a schedule is not a feature.
func ParseFilename(name string) (instance string, at time.Time, class Class, ok bool) {
	base, cut := strings.CutSuffix(name, Ext)
	if !cut {
		return "", time.Time{}, "", false
	}
	// Split from the right: an instance name may contain "-", the stamp and
	// the class may not (D-45 permits "-" in instance names, and the class
	// enum has none).
	dash := strings.LastIndexByte(base, '-')
	if dash < 0 {
		return "", time.Time{}, "", false
	}
	cl := Class(base[dash+1:])
	rest := base[:dash]
	if !cl.Valid() {
		return "", time.Time{}, "", false
	}
	// The stamp itself contains one "-", so two more cuts are needed.
	dash = strings.LastIndexByte(rest, '-')
	if dash < 0 {
		return "", time.Time{}, "", false
	}
	dash2 := strings.LastIndexByte(rest[:dash], '-')
	if dash2 < 0 {
		return "", time.Time{}, "", false
	}
	ts, err := time.ParseInLocation(Stamp, rest[dash2+1:], time.UTC)
	if err != nil {
		return "", time.Time{}, "", false
	}
	inst := rest[:dash2]
	if inst == "" {
		return "", time.Time{}, "", false
	}
	return inst, ts, cl, true
}

// clockOf returns now, defaulting to the wall clock.
//
// Every entry point in this package takes an injectable clock for the same
// reason: retention, the index and the archive filenames are all decided by
// time, and a test that has to sleep to reach a boundary is a test that will
// eventually be flaky and then be deleted.
func clockOf(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

// octalMode renders a permission bit set the way the manifest records it.
func octalMode(perm uint32) string { return fmt.Sprintf("%04o", perm&0o7777) }
