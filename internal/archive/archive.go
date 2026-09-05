// Package archive is the one chokepoint every destructive path in gvm goes
// through (section 13.1).
//
// Phase 3 declares only Ref, because Ref is what makes archive-before-destroy a
// type rather than a review comment: instance.Destroy takes a *Ref, so a caller
// that has not archived has nothing to pass. The tarball format, creation,
// verification and restore arrive in Phase 5.
//
// This package deliberately does not import internal/instance. The destroy
// chokepoint lives there (instance.Destroy takes []instance.Jailed, per section
// 13.6 step 8), so an archive-side Destroy taking []instance.Jailed would close
// an import cycle between the two packages.
package archive

import "time"

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

// Ref is the receipt for an archive that exists on disk and has been verified.
//
// It is the token instance.Destroy demands. A caller holding one has, by
// construction, already written and checked the bytes it is about to delete.
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
