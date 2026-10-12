package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/provider"
)

// Digest is the install seam's digest type, reused rather than redeclared.
//
// An alias, not a second type: the blob store that hands out verified bytes
// takes provider.Digest, and a content.Digest that had to be converted at the
// seam would be two parsers one release apart from disagreeing about what
// "sha256:deadbeef" means. The parser is the thing that stops a truncated,
// brute-forceable digest from being accepted as a 256-bit commitment, and
// there is exactly one of it.
type Digest = provider.Digest

// Digest algorithms, from the same place for the same reason.
const (
	AlgoSHA256     = provider.AlgoSHA256
	AlgoSHA512     = provider.AlgoSHA512
	AlgoSHA1       = provider.AlgoSHA1
	AlgoTreeSHA256 = provider.AlgoTreeSHA256
)

// ParseDigest canonicalises a digest string or explains why it is not one.
func ParseDigest(d Digest) (Digest, error) { return provider.ParseDigest(d) }

// Tree is the verified description of an ArtifactTree: a directory gvm did not
// download and cannot hand to the blob store.
type Tree struct {
	// Digest is "tree-sha256:…" over the canonical listing below.
	Digest Digest
	// Bytes is the sum of the regular members' sizes.
	Bytes int64
	// Members is how many entries the listing has. Non-emptiness is half of
	// what section 12.1 means by verifying a tree, and it is the positive
	// signal rebuild.sh's NOT_DOWNLOADED check was looking for.
	Members int
}

// Empty reports the condition that makes a workshop item "not downloaded yet"
// rather than "downloaded": the directory exists and holds nothing.
func (t Tree) Empty() bool { return t.Members == 0 }

// treeSeparator is the field separator of the canonical listing. NUL, because
// it is the one byte that cannot occur in a path on either platform: a comma or
// a tab would let a crafted filename forge a listing line and so forge a tree
// digest.
const treeSeparator = "\x00"

// DigestTree computes the tree-sha256 of root inside fsys.
//
// The listing is canonical: every entry on its own line, as
//
//	relpath \0 size \0 sha256hex \n
//
// in lexical path order, with forward slashes. Lexical order comes from
// fs.WalkDir and is re-asserted here, because a digest whose value depended on
// directory iteration order would differ between the machine that recorded it
// and the machine that checks it, and the mismatch would read as tampering.
//
// A non-regular member (a symlink, a device node, a socket that a mod author
// managed to publish) is recorded as
//
//	relpath \0 -1 \0 <type letter>[:<target>] \n
//
// rather than skipped. Skipping it would mean a tree could gain or lose a
// symlink without its digest moving, and a symlink is exactly the member worth
// noticing: it is how a tree points at bytes outside itself. The target is
// included when the filesystem can report it, so repointing a link at
// /etc/shadow moves the digest too.
func DigestTree(ctx context.Context, fsys fs.FS, root string) (Tree, error) {
	var t Tree
	if fsys == nil {
		return t, fmt.Errorf("%w: no filesystem to digest %q in", ErrInvalidSchema, root)
	}
	root = cleanFSPath(root)

	type member struct {
		rel  string
		line string
		size int64
	}
	var members []member

	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unlike a resolve, a digest cannot shrug off an unreadable
			// subtree: a tree digest taken over the part that happened to be
			// readable is a digest of something else, and it would verify
			// against nothing.
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		rel := relFSPath(root, p)

		if !d.Type().IsRegular() {
			members = append(members, member{
				rel: rel,
				line: rel + treeSeparator + "-1" + treeSeparator +
					nonRegularID(fsys, p, d.Type()) + "\n",
			})
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		sum, err := hashFile(ctx, fsys, p)
		if err != nil {
			return err
		}
		members = append(members, member{
			rel:  rel,
			line: rel + treeSeparator + fmt.Sprint(info.Size()) + treeSeparator + sum + "\n",
			size: info.Size(),
		})
		return nil
	})
	if err != nil {
		return Tree{}, fmt.Errorf("digest tree %q: %w", root, err)
	}

	sort.Slice(members, func(i, j int) bool { return members[i].rel < members[j].rel })

	h := sha256.New()
	for _, m := range members {
		_, _ = io.WriteString(h, m.line)
		t.Bytes += m.size
	}
	t.Members = len(members)
	t.Digest = Digest(AlgoTreeSHA256 + ":" + hex.EncodeToString(h.Sum(nil)))
	return t, nil
}

// VerifyTree is the cheap check and the deep check, in that order.
//
// deep false stops after presence and non-emptiness, which is what
// `acquisition.verify.dir_non_empty` asks for and what every sync does: the
// server downloaded these bytes and the question is whether they arrived. deep
// true recomputes the full digest and compares it, which is `gvm content verify
// --deep` and is minutes of IO over a 6 GB workshop tree.
//
// want empty with deep true is not an error: there is nothing recorded to
// compare against, so the computed digest is returned for recording.
func VerifyTree(ctx context.Context, fsys fs.FS, root string, want Digest, deep bool) (Tree, error) {
	present, err := TreePresent(fsys, root)
	if err != nil {
		return Tree{}, err
	}
	if !present {
		return Tree{}, fmt.Errorf("%w: %q is not a directory, so nothing has been acquired "+
			"into it yet", ErrUnaccounted, root)
	}
	if !deep {
		t, err := treeShallow(ctx, fsys, root)
		if err != nil {
			return Tree{}, err
		}
		if t.Empty() {
			return t, fmt.Errorf("%w: %q exists but is empty", ErrUnaccounted, root)
		}
		return t, nil
	}

	t, err := DigestTree(ctx, fsys, root)
	if err != nil {
		return Tree{}, err
	}
	if t.Empty() {
		return t, fmt.Errorf("%w: %q exists but is empty", ErrUnaccounted, root)
	}
	if strings.TrimSpace(string(want)) == "" {
		return t, nil
	}
	canon, err := ParseDigest(want)
	if err != nil {
		return t, err
	}
	if canon != t.Digest {
		return t, fmt.Errorf("%w: %q recorded %s and now hashes to %s", ErrDigest, root,
			canon.Short(), t.Digest.Short())
	}
	return t, nil
}

// TreePresent reports whether root is a directory. A missing root is not an
// error: the server writes workshop content to different locations depending on
// its version and a manifest legitimately lists several.
func TreePresent(fsys fs.FS, root string) (bool, error) {
	if fsys == nil {
		return false, fmt.Errorf("%w: no filesystem to look for %q in", ErrInvalidSchema, root)
	}
	st, err := fs.Stat(fsys, cleanFSPath(root))
	switch {
	case err == nil:
		return st.IsDir(), nil
	case isNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("stat %q: %w", root, err)
	}
}

// treeShallow counts and sizes members without hashing any of them.
func treeShallow(ctx context.Context, fsys fs.FS, root string) (Tree, error) {
	var t Tree
	root = cleanFSPath(root)
	err := fs.WalkDir(fsys, root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		t.Members++
		if info, err := d.Info(); err == nil {
			t.Bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return Tree{}, fmt.Errorf("scan tree %q: %w", root, err)
	}
	return t, nil
}

// hashFile streams one member through sha256. Streamed rather than read whole:
// a workshop item can contain a 400 MB texture pack, and reading it into
// memory to hash it would make `verify --deep` an OOM on a 2 GB VPS.
func hashFile(ctx context.Context, fsys fs.FS, p string) (string, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, newCtxReader(ctx, f)); err != nil {
		return "", fmt.Errorf("hash %q: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ctxReader makes a multi-gigabyte io.Copy cancellable. Without it, a
// `verify --deep` that the operator ctrl-C'd would keep reading until the tree
// ran out.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func newCtxReader(ctx context.Context, r io.Reader) io.Reader { return &ctxReader{ctx: ctx, r: r} }

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// nonRegularID identifies a non-regular member in the listing.
//
// For a symlink it appends the target when the filesystem can report one,
// because the type letter alone would let a link be repointed at another
// tree's bytes -- or at /etc/shadow -- without the digest moving. A
// filesystem that cannot report targets degrades to the letter rather than
// failing: the member is still committed to, which is the property that
// matters.
func nonRegularID(fsys fs.FS, p string, mode fs.FileMode) string {
	letter := typeLetter(mode)
	if mode&fs.ModeSymlink == 0 {
		return letter
	}
	target, err := fs.ReadLink(fsys, p)
	if err != nil {
		return letter
	}
	return letter + ":" + target
}

// typeLetter names a non-regular member's kind in one byte, so the canonical
// listing commits to it without embedding a Go-version-dependent mode string.
func typeLetter(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "L"
	case m&fs.ModeDevice != 0:
		return "D"
	case m&fs.ModeNamedPipe != 0:
		return "P"
	case m&fs.ModeSocket != 0:
		return "S"
	case m&fs.ModeDir != 0:
		return "d"
	}
	return "?"
}

// cleanFSPath normalises a path for an fs.FS, whose paths are always
// slash-separated, unrooted and never "".
func cleanFSPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.Trim(p, "/")
	if p == "" {
		return "."
	}
	return path.Clean(p)
}

// relFSPath is the member path recorded in the listing: relative to the tree
// root, so a digest recorded under one absolute root verifies under another.
// That matters because a manifest declares two workshop roots and the server
// may use either.
func relFSPath(root, p string) string {
	if root == "." {
		return p
	}
	if p == root {
		return path.Base(p)
	}
	return strings.TrimPrefix(p, root+"/")
}

// isNotExist is "this root is not on this box", which is a normal answer and
// not a failure: a manifest declares several workshop roots because the server
// writes to different ones depending on its version.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
