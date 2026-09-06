package archive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Target is one path that has been PROVEN safe to delete.
//
// It is an interface rather than a concrete type because the proving happens
// in internal/instance, whose Jail.Resolve is the only thing that can produce
// an instance.Jailed -- and instance imports this package for Ref, so a
// concrete parameter here would close an import cycle. instance.Jailed
// satisfies this interface without either package naming the other, which
// keeps the guarantee where it belongs: a caller still cannot hand Destroy a
// string, because a string does not implement it.
type Target interface {
	// Path is the absolute, symlink-resolved path.
	Path() string
	// Root is the allow root the path was proven to live under.
	Root() string
	// Rel is the path relative to that root, which is what a plan or an
	// audit record should print.
	Rel() string
	// Valid reports whether the value came from a real proof rather than
	// from a zero composite literal.
	Valid() bool
}

// DestroyResult is what was removed, for the audit record and for the
// "old world: 2.4 GB" line rebuild prints when it is done.
type DestroyResult struct {
	Paths []string  `json:"paths"`
	Bytes int64     `json:"bytes"`
	Files int       `json:"files"`
	Dirs  int       `json:"dirs"`
	At    time.Time `json:"at"`
	// Archive is the receipt that authorised the deletion. It is recorded so
	// that "what was destroyed" and "where it went" are one record and cannot
	// drift apart in a log.
	Archive string `json:"archive"`
}

// Destroy removes data, and it is the only function in this package that does.
//
// It PANICS rather than returning an error when the receipt is missing or
// unverified. That is deliberate and it is the whole design of section 13.1:
//
//   - A nil Ref means the caller never archived. A verified Ref means the
//     caller archived AND read the bytes back. Reaching this function without
//     one is not an operating condition that a well-written caller might hit;
//     it is a bug in the ordering of a job, and the ordering is the safety
//     property.
//
//   - A returned error is something a future caller can route around with
//     `if err != nil { log.Warn(...) }` and carry on. There is no version of
//     "continue anyway" that is correct here, so the language should not offer
//     one. A panic inside a job is recovered, audited and reported (section
//     13.3), so the operator learns about the bug and the world survives it.
//
// Every Target must be valid for the same reason: the zero value of a proven
// path is the one forgery another package can write, and a zero Target names
// no path, so acting on the rest of the list while ignoring it would be a
// destroy over a plan nobody approved.
func Destroy(ctx context.Context, ref *Ref, targets []Target) (*DestroyResult, error) {
	if ref == nil {
		panic("archive: Destroy called with a nil archive ref; nothing is destroyed " +
			"before it is archived (section 13.1)")
	}
	if !ref.Verified() {
		panic(fmt.Sprintf("archive: Destroy called with an unverified archive ref %q; "+
			"a truncated tarball is exactly the failure archive-before-destroy prevents", ref.Path))
	}
	for i, t := range targets {
		if t == nil || !t.Valid() {
			panic(fmt.Sprintf("archive: Destroy called with an unproven target at index %d; "+
				"the only legitimate source of one is a destroy jail's Resolve", i))
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("archive: destroy set is empty; a destroy that removes nothing " +
			"must not report success")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	res := &DestroyResult{Archive: ref.Path, At: time.Now().UTC()}
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// Measured BEFORE the delete, because afterwards there is nothing to
		// measure and the operator still has to be told what went.
		files, dirs, bytes, err := Measure(t.Path())
		if err != nil {
			return res, fmt.Errorf("archive: measure %s before destroying it: %w", t.Path(), err)
		}
		if err := os.RemoveAll(t.Path()); err != nil {
			return res, fmt.Errorf("archive: remove %s: %w", t.Path(), err)
		}
		res.Paths = append(res.Paths, t.Path())
		res.Files += files
		res.Dirs += dirs
		res.Bytes += bytes
	}
	return res, nil
}

// Measure walks a path and totals it. An absent path is zero, not an error: a
// destroy set rendered from a manifest glob can name a path this install never
// created, and refusing the whole destroy over it would push the caller into
// pre-filtering the list, which is where a path that should have been removed
// gets dropped.
//
// It is exported because the destroy PREFLIGHT needs the same number before
// anything is removed -- instance.DestroyPreflight has to answer "will this
// still be undoable afterwards" -- and two walkers that disagreed about
// whether a symlink counts would make the plan and the receipt disagree.
func Measure(root string) (files, dirs int, bytes int64, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			dirs++
			return nil
		}
		files++
		if fi, ierr := d.Info(); ierr == nil && fi.Mode().IsRegular() {
			bytes += fi.Size()
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, 0, nil
	}
	return files, dirs, bytes, err
}
