package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Source records where a stored secret's value came from. It is the provenance
// a `.source` sidecar holds, and it is the whole of what `secret list` and
// doctor INST-013 report about origin.
//
// It is a closed set of three because each one changes what gvm is allowed to
// do to the value later (rule 11): a generated value may be rotated freely, an
// operator value is never rotated behind the operator's back, and an adopted
// value is the live server's own credential and gvm neither generates nor
// rotates it (Phase 50's cutover writes it, and never touches it again).
type Source string

const (
	// SourceGenerated: gvm minted the value with GeneratePassword.
	SourceGenerated Source = "generated"
	// SourceOperator: a human supplied the value, by prompt, file or pipe.
	SourceOperator Source = "operator"
	// SourceAdopted: the value is the live server's own credential, imported by
	// cutover (Phase 50). gvm never generates or rotates it.
	SourceAdopted Source = "adopted"
)

// sourceSuffix is the sidecar's filename suffix. It carries a '.', which no
// secret name may contain (nameRE), so a sidecar can never collide with a
// secret and Store.List — which skips every entry that is not a valid secret
// name — never reports one as a secret.
const sourceSuffix = ".source"

// ErrSource is the sentinel a bad or missing provenance record wraps.
var ErrSource = errors.New("secrets: invalid source record")

// ValidateSource reports whether src is one of the three known origins.
func ValidateSource(src Source) error {
	switch src {
	case SourceGenerated, SourceOperator, SourceAdopted:
		return nil
	default:
		return fmt.Errorf("%w: %q is not %q, %q or %q",
			ErrSource, src, SourceGenerated, SourceOperator, SourceAdopted)
	}
}

// sourcePath is the sidecar file for a ref: the secret's path plus ".source".
func (s *Store) sourcePath(ref Ref) (string, error) {
	p, err := s.Path(ref)
	if err != nil {
		return "", err
	}
	return p + sourceSuffix, nil
}

// WriteSource records where ref's value came from, atomically and at 0600
// root:root — the same mode as the secret, because the provenance is as
// sensitive to tamper as the value: a `.source` quietly flipped from "operator"
// to "generated" is how `apply` would be tricked into rotating a credential a
// human set by hand.
func (s *Store) WriteSource(ref Ref, src Source) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := ValidateSource(src); err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	if err := s.mkdirAll(dirRel(ref.Scope, ref.Key)); err != nil {
		return err
	}
	p, err := s.sourcePath(ref)
	if err != nil {
		return err
	}
	if err := spec.WriteFileAtomicNoBackup(p, []byte(string(src)+"\n"), FileMode); err != nil {
		return fmt.Errorf("secrets: write source %s: %w", ref, err)
	}
	return chownRoot(p)
}

// Source reads ref's provenance. ok is false when no sidecar exists, which is
// the state of a secret set by an older gvm or written by hand: absent is not
// an error, it is "origin unknown", and the caller reports it as such rather
// than guessing one of the three.
func (s *Store) Source(ref Ref) (src Source, ok bool, err error) {
	p, perr := s.sourcePath(ref)
	if perr != nil {
		return "", false, perr
	}
	b, rerr := os.ReadFile(p)
	if errors.Is(rerr, fs.ErrNotExist) {
		return "", false, nil
	}
	if rerr != nil {
		return "", false, fmt.Errorf("secrets: read source %s: %w", ref, rerr)
	}
	got := Source(trimOneEOL(string(b)))
	if err := ValidateSource(got); err != nil {
		return "", false, fmt.Errorf("secrets: source %s: %w", ref, err)
	}
	return got, true, nil
}

// DeleteSource removes a secret's provenance sidecar, idempotently. It is for
// the same cleanup paths as Delete: destroying an instance removes both.
func (s *Store) DeleteSource(ref Ref) error {
	p, err := s.sourcePath(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("secrets: delete source %s: %w", ref, err)
	}
	return nil
}
