package instance

import (
	"context"
	"errors"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/archive"
)

// ErrNotImplemented is returned by the parts of Destroy that land in Phase 5.
// The signature is fixed now, because the signature is the contract: it is what
// makes every future caller obtain an archive first and prove its paths second.
var ErrNotImplemented = errors.New("instance: destroy is implemented in phase 5")

// Destroy removes an instance's data. It is the only function in gvm that does.
//
// The two arguments are the whole design:
//
//   - *archive.Ref is archive-before-destroy encoded as a type. A caller that
//     has not written and verified an archive has nothing to pass, and a nil
//     one is refused before anything is touched. This is rebuild.sh's ordering
//     invariant -- the archive is taken after content verification and before
//     deletion -- promoted from a comment in a shell script to a parameter.
//
//   - []Jailed is the destroy jail. Jailed's fields are unexported, so the
//     only way to obtain one is Jail.Resolve, which has already refused "..",
//     symlink escapes, world paths, ancestors of world paths, paths outside
//     the allow roots and paths across a mount. Destroy therefore cannot be
//     called with a string, and there is no argument to have about whether the
//     caller checked: it could not have got here otherwise.
//
// It PANICS rather than returning an error when the receipt is missing or
// unverified, per section 13.1. Reaching it that way is a programming error,
// not an operating condition: a panic inside a job is recovered, audited and
// reported, whereas a returned error is something a future caller can route
// around with an `if err != nil { log }`. The disk preflight belongs to the
// step that produces the Ref (section 13.6 step 7), which is the last moment
// at which refusing costs nothing.
func Destroy(ctx context.Context, ref *archive.Ref, paths []Jailed) error {
	if ref == nil {
		panic("instance: Destroy called with a nil archive ref; nothing is destroyed " +
			"before it is archived (section 13.1)")
	}
	if !ref.Verified() {
		panic(fmt.Sprintf("instance: Destroy called with an unverified archive ref %q; "+
			"a truncated tarball is exactly the failure archive-before-destroy prevents", ref.Path))
	}
	if len(paths) == 0 {
		return fmt.Errorf("%w: no paths; a destroy that removes nothing must not report success",
			ErrJail)
	}
	for i, p := range paths {
		if !p.Valid() {
			panic(fmt.Sprintf("instance: Destroy called with a zero Jailed at index %d; "+
				"the only legitimate source of a Jailed is Jail.Resolve", i))
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrNotImplemented
}
