//go:build linux

package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/provider"
)

// The behavioural half of the chown seam, which needs real symlinks.
//
// It lives in a _linux file and not in fs_test.go because os.Symlink on Windows
// needs a privilege this project deliberately does not depend on (see the
// fakeShimFS comment in helpers_test.go), so the development machine asserts
// the seam's IDENTITY -- TestTheChownSeamDefaultsToLchown -- and this asserts
// what the seam does.
//
// The adversary is the one §18.1 names: post_install and post_content hooks run
// in the job process as ROOT, while the directory a newly created ancestor goes
// into is owned by the instance account (hostops gives DirInstall and DirData
// OwnedByInstance). So the account the hook is writing FOR can plant a link
// where the hook is about to create a directory, and an os.Chown there is root
// chowning a target the attacker chose.

// A symlink that is already there when the write is addressed is refused by the
// jail: instance.Jail.Resolve EvalSymlinks-es the existing prefix, and a prefix
// that leaves the root fails containment. This is the defence that already
// worked, pinned so the fix below cannot be mistaken for it.
func TestFSRefusesAPreExistingSymlinkedAncestor(t *testing.T) {
	e := newEnv(t, "palworld.yaml", nil)
	install, err := e.ctx.Fs.Path(RootInstall)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(e.root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(install, "Pal")); err != nil {
		t.Fatal(err)
	}
	err = e.ctx.Fs.MkdirAll(RootInstall, "Pal/Saved/Config", 0o750)
	if err == nil {
		t.Fatal("a write through a symlinked ancestor was allowed")
	}
	if !strings.Contains(err.Error(), "hook: path refused") {
		t.Errorf("the refusal did not come from the jail: %v", err)
	}
}

// And the window the jail cannot close: the swap happens AFTER the resolve.
//
// Jail.Resolve proves a fact about the path at check time and holds no
// descriptor, so it says nothing about the inode at use time. The injection
// point here is the chown of a level that was just created, which is the
// narrowest moment the per-level mkdirOwned leaves -- one syscall before the
// next level is created.
//
// What must hold afterwards: no chown lands on an inode that existed before the
// call. That is the arbitrary-chown primitive, and it is what os.Chown on a
// swapped-in link gave away -- root handing ownership of `outside`, or of
// whatever `outside` itself points at, to the instance account. os.Lchown plus
// one os.Mkdir per level leaves only inodes this call created, inside a
// directory the attacker already owns.
func TestFSChownNeverLandsOnAPreExistingInode(t *testing.T) {
	e := newEnv(t, "palworld.yaml", nil)
	install, err := e.ctx.Fs.Path(RootInstall)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(e.root, "outside")
	// Two pre-existing things the attacker would like root to hand over: the
	// directory itself, and a name the hook is about to create inside it.
	if err := os.MkdirAll(filepath.Join(outside, "Saved"), 0o755); err != nil {
		t.Fatal(err)
	}
	preexisting := map[string]bool{}
	for _, p := range []string{outside, filepath.Join(outside, "Saved")} {
		fi, serr := os.Lstat(p)
		if serr != nil {
			t.Fatal(serr)
		}
		preexisting[inodeKey(t, fi)] = true
	}

	owner, err := provider.NewUnixIdent("gvm-test", "gvm-test", 998, 998)
	if err != nil {
		t.Fatal(err)
	}
	swapped := false
	var chowned []string
	fsys, err := NewFS(e.ctx.Paths, owner, WithChown(func(p string, uid, gid int) error {
		chowned = append(chowned, p)
		// The real seam is os.Lchown, so record what IT would act on: the
		// link, never its target.
		fi, serr := os.Lstat(p)
		if serr != nil {
			t.Errorf("chown %s: the target does not exist: %v", p, serr)
			return nil
		}
		if preexisting[inodeKey(t, fi)] {
			t.Errorf("chown landed on %s, an inode that existed before this call: root has "+
				"handed ownership of a target the instance account chose (§18.1)", p)
		}
		if !swapped && filepath.Base(p) == "Pal" {
			// Exactly the TOCTOU the path-based jail cannot see: the level is
			// created and owned, and then it is not the same directory any more.
			swapped = true
			if rerr := os.Rename(p, p+".real"); rerr != nil {
				t.Fatal(rerr)
			}
			if lerr := os.Symlink(outside, p); lerr != nil {
				t.Fatal(lerr)
			}
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	// Either outcome is acceptable -- the creation may fail on the swapped-in
	// link, or it may proceed into the attacker's directory. What is not
	// acceptable is an ownership change reaching anything that was already
	// there.
	_ = fsys.MkdirAll(RootInstall, "Pal/Saved/Config/LinuxServer", 0o750)

	if !swapped {
		t.Fatal("the swap never happened, so this test asserted nothing")
	}
	// The first level is still owned, which is what makes the swap the only
	// thing under test rather than a write that never started.
	if len(chowned) == 0 || filepath.Base(chowned[0]) != "Pal" {
		t.Fatalf("chowns = %v, want the outermost created level first", chowned)
	}
	if !strings.HasPrefix(chowned[0], install) {
		t.Errorf("the first chown was %s, outside the install root %s", chowned[0], install)
	}
}

// inodeKey identifies a file by device and inode, so "the same inode under a
// different name" is answerable -- which is the whole question a swap raises.
//
// syscall is imported here and nowhere in this package's non-test files, which
// noexec_test.go's import gate asserts: a raw syscall belongs in
// internal/platform behind a build tag, and a test that only needs st_ino is
// not a reason to put one in a hook.
func inodeKey(t *testing.T, fi os.FileInfo) string {
	t.Helper()
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("FileInfo.Sys() is %T, not a *syscall.Stat_t", fi.Sys())
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
