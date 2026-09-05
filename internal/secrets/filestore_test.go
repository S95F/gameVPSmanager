package secrets

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// newTestStore roots a store one level below a temp dir, so that the root's
// own creation is exercised and the root's parent already exists.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "secrets"))
}

func TestWriteThenReadRoundTripsTheExactValue(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	const pw = "Xk4mQ9pLzR2vT7wB"

	if err := s.Write(ref, New(pw)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Read(ref)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !Equal(got, New(pw)) {
		t.Error("Read returned a different secret than Write stored")
	}

	// The bytes on disk matter as much as the round trip: systemd hands this
	// file to the unit verbatim through LoadCredential=, so a trailing
	// newline gvm added would become part of the password the game receives
	// -- and gvm's own Read would trim it back off and report success.
	p, _ := s.Path(ref)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading the file back: %v", err)
	}
	if string(raw) != pw {
		t.Errorf("file holds %q, want exactly %q with no trailing newline", raw, pw)
	}
}

func TestRotationLeavesNoCopyOfThePreviousSecret(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	const old = "OldPassword1234"

	if err := s.Write(ref, New(old)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Write(ref, New("NewPassword5678")); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// spec.WriteFileAtomic keeps a .bak, which is right for an instance spec
	// and catastrophic for a credential: the rotated-away password would sit
	// beside the live one at 0600, forever, and land in every backup.
	var leaked []string
	root := s.Root()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), old) {
			rel, _ := filepath.Rel(root, p)
			leaked = append(leaked, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the store: %v", err)
	}
	if len(leaked) != 0 {
		t.Errorf("the previous secret survives the rotation in %v; Write must go through "+
			"spec.WriteFileAtomicNoBackup", leaked)
	}
	if _, err := os.Stat(filepath.Join(root, "instances", "pz-main", "rcon"+spec.BackupSuffix)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a %s file exists beside the secret", spec.BackupSuffix)
	}
}

func TestReadTrimsExactlyOneTrailingLineEnding(t *testing.T) {
	// The operator sets a secret with `echo hunter2 > join` or edits it in
	// vi. Both leave a newline that the game would otherwise receive as part
	// of the password -- producing a server nobody can join and a credential
	// that looks correct in every tool that prints it.
	cases := []struct {
		name    string
		on_disk string
		want    string
		wantErr error
	}{
		{"no newline", "hunter2", "hunter2", nil},
		{"one newline", "hunter2\n", "hunter2", nil},
		{"crlf", "hunter2\r\n", "hunter2", nil},
		{"two newlines", "hunter2\n\n", "", ErrMultiline},
		{"embedded newline", "hunter2\nmore\n", "", ErrMultiline},
		{"only a newline", "\n", "", ErrEmpty},
		{"empty file", "", "", ErrEmpty},
		{"leading space is kept", " hunter2", " hunter2", nil},
		{"a bare cr is not a line ending", "hunter2\r", "", ErrMultiline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ref := InstanceRef("pz-main", "join")
			if err := s.mkdirAll(dirRel(ref.Scope, ref.Key)); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			p, _ := s.Path(ref)
			if err := os.WriteFile(p, []byte(tc.on_disk), 0o600); err != nil {
				t.Fatalf("seeding the file: %v", err)
			}

			got, err := s.Read(ref)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Read = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got.Reveal() != tc.want {
				t.Errorf("Read = %q, want %q", got.Reveal(), tc.want)
			}
		})
	}
}

func TestWriteRefusesAValueTheFileFormatCannotHold(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	cases := map[string]struct {
		in   string
		want error
	}{
		"empty":          {"", ErrEmpty},
		"newline":        {"two\nlines", ErrMultiline},
		"trailing eol":   {"hunter2\n", ErrMultiline},
		"carriage retur": {"hunter2\r", ErrMultiline},
		"nul":            {"hunter\x002", ErrMultiline},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.Write(ref, New(tc.in))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Write(%q) = %v, want %v", tc.in, err, tc.want)
			}
			// And the message must be about the ref, never about the value.
			if strings.Contains(err.Error(), tc.in) && tc.in != "" {
				t.Errorf("the refusal quoted the secret: %v", err)
			}
			if has, _ := s.Has(ref); has {
				t.Error("a refused value still created the file")
			}
		})
	}
}

func TestARefusedRewriteLeavesThePreviousSecretIntact(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	const good = "GoodPassword99"
	if err := s.Write(ref, New(good)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Write(ref, New("broken\nvalue")); err == nil {
		t.Fatal("Write accepted a multiline value")
	}
	got, err := s.Read(ref)
	if err != nil || got.Reveal() != good {
		t.Errorf("after a refused rewrite the secret is %q, %v; want the previous value. "+
			"Validation has to happen before the file is touched, not after.", got.Reveal(), err)
	}
}

func TestWriteRefusesATraversalAndTouchesNothing(t *testing.T) {
	root := t.TempDir()
	s := NewStore(filepath.Join(root, "secrets"))
	bad := []Ref{
		InstanceRef("pz-main", "../../../shadow"),
		InstanceRef("../../etc", "rcon"),
		{Scope: ScopeHost, Key: "../instances/pz-main", Name: "rcon"},
		{Scope: "instance/../host", Key: "pz-main", Name: "rcon"},
	}
	for _, ref := range bad {
		if err := s.Write(ref, New("x")); err == nil {
			t.Errorf("Write(%+v) succeeded", ref)
		} else if !errors.Is(err, ErrRef) {
			t.Errorf("Write(%+v) = %v, which does not wrap ErrRef", ref, err)
		}
	}
	// Nothing anywhere under the temp root, not even the store root: a
	// rejected ref must not have side effects on the way to being rejected.
	var made []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != root {
			made = append(made, p)
		}
		return nil
	})
	if len(made) != 0 {
		t.Errorf("a refused write created %v", made)
	}
}

func TestListSkipsWhatIsNotASecret(t *testing.T) {
	s := newTestStore(t)
	inst := "pz-main"
	for _, n := range []string{"rcon", "admin", "join"} {
		if err := s.Write(InstanceRef(inst, n), New("value-"+n)); err != nil {
			t.Fatalf("Write %s: %v", n, err)
		}
	}
	dir := filepath.Join(s.Root(), "instances", inst)
	// The leftovers of an interrupted write, which spec's atomic writer
	// deliberately does not clean up, plus a stray backup and a directory.
	for _, junk := range []string{".rcon.tmp2841582", "rcon.bak", "README", "RCON"} {
		if err := os.WriteFile(filepath.Join(dir, junk), []byte("x"), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", junk, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "steam"), 0o700); err != nil {
		t.Fatalf("seeding a subdirectory: %v", err)
	}

	got, err := s.List(ScopeInstance, inst)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"admin", "join", "rcon"}
	if len(got) != len(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	for i, ref := range got {
		if ref.Name != want[i] {
			t.Errorf("List[%d] = %q, want %q", i, ref.Name, want[i])
		}
		if ref.Scope != ScopeInstance || ref.Key != inst {
			t.Errorf("List[%d] = %+v, want scope %q key %q", i, ref, ScopeInstance, inst)
		}
	}
}

func TestListOfAnInstanceWithNoSecretsIsEmptyNotAnError(t *testing.T) {
	s := newTestStore(t)
	got, err := s.List(ScopeInstance, "never-created")
	if err != nil || got != nil {
		t.Errorf("List = %v, %v; a missing directory and an empty one are the same state", got, err)
	}
	if _, err := s.List(ScopeInstance, "NOT A NAME"); err == nil {
		t.Error("List accepted an illegal instance name")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	if err := s.Write(ref, New("value")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for i := range 3 {
		if err := s.Delete(ref); err != nil {
			t.Fatalf("Delete #%d: %v; destroy and rotate both run against a set that may "+
				"already be partly gone", i+1, err)
		}
	}
	if has, err := s.Has(ref); has || err != nil {
		t.Errorf("Has after Delete = %v, %v", has, err)
	}
	if _, err := s.Read(ref); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read after Delete = %v, want something that unwraps to fs.ErrNotExist", err)
	}
}

func TestTheStoreRefusesToCreateItsRootsParent(t *testing.T) {
	// /etc/gvm is 0750 root:gvm so the web tier can read specs. An
	// os.MkdirAll that created it on the way to /etc/gvm/secrets would create
	// it 0700 root:root, and the web tier would lose every instance page on
	// the box with nothing to point at.
	s := NewStore(filepath.Join(t.TempDir(), "etc-gvm", "secrets"))
	err := s.Write(InstanceRef("pz-main", "rcon"), New("value"))
	if err == nil {
		t.Fatal("Write created the store root's parent")
	}
	if !strings.Contains(err.Error(), "parent directory does not exist") {
		t.Errorf("Write = %v, want a diagnostic naming the missing parent", err)
	}
}

func TestModesOnDiskAreSection5s(t *testing.T) {
	if runtime.GOOS != "linux" {
		// The rest of this file is the portable half. This assertion is the
		// syscall-dependent one: Windows synthesises 0666/0444 from the
		// read-only attribute, so asserting it there would test nothing.
		t.Skip("unix permission bits are only meaningful on linux; this runs for real on CI")
	}
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	if err := s.Write(ref, New("value")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	p, _ := s.Path(ref)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != FileMode {
		t.Errorf("secret file is %04o, want %04o", fi.Mode().Perm(), FileMode)
	}
	for _, dir := range []string{s.Root(), filepath.Join(s.Root(), "instances"), filepath.Dir(p)} {
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != DirMode {
			t.Errorf("%s is %04o, want %04o", dir, di.Mode().Perm(), DirMode)
		}
	}
}

func TestStatSaysWhetherItCouldCheckTheMode(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	if err := s.Write(ref, New("value")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := s.Stat(ref)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len("value")) {
		t.Errorf("Size = %d, want %d", info.Size, len("value"))
	}
	wantChecked := runtime.GOOS == "linux"
	if info.ModeChecked != wantChecked {
		t.Errorf("ModeChecked = %v on %s, want %v", info.ModeChecked, runtime.GOOS, wantChecked)
	}
	if info.ModeChecked != OwnershipEnforced {
		t.Errorf("ModeChecked = %v but OwnershipEnforced = %v; the two build tags disagree",
			info.ModeChecked, OwnershipEnforced)
	}
	if info.ModeChecked && info.Mode != FileMode {
		t.Errorf("Mode = %04o, want %04o", info.Mode, FileMode)
	}
}

func TestStatAndReadRefuseSomethingThatIsNotAFile(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	if err := s.mkdirAll(dirRel(ref.Scope, ref.Key)); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p, _ := s.Path(ref)
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatalf("seeding a directory where the secret goes: %v", err)
	}
	if _, err := s.Read(ref); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Read of a directory = %v, want ErrNotRegular", err)
	}
	if _, err := s.Stat(ref); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Stat of a directory = %v, want ErrNotRegular", err)
	}
	if has, err := s.Has(ref); has || err == nil {
		t.Errorf("Has of a directory = %v, %v; want false and an error rather than a "+
			"silent yes that Read then contradicts", has, err)
	}
}

func TestInsecureModeIsExactlyTheGroupAndWorldBits(t *testing.T) {
	// This is the predicate behind the master-key refusal and behind doctor's
	// HOST-006, and it is pure, so it is tested on every platform even though
	// the modes it judges only exist on one.
	cases := map[fs.FileMode]bool{
		0o400: false, 0o600: false, 0o700: false, 0o000: false,
		0o440: true, 0o404: true, 0o640: true, 0o644: true, 0o660: true,
		0o604: true, 0o620: true, 0o777: true, 0o001: true, 0o010: true,
	}
	for perm, want := range cases {
		if got := InsecureMode(perm); got != want {
			t.Errorf("InsecureMode(%04o) = %v, want %v", perm, got, want)
		}
	}
	// Type bits must not fool it: a mode carrying ModeDir is still judged on
	// its permission bits alone.
	if InsecureMode(fs.ModeDir | 0o700) {
		t.Error("InsecureMode was confused by the directory type bit")
	}
}

func TestMasterKeyRoundTripsAndRejectsAWrongSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "master.key")

	k := NewMasterKey()
	if k.Value.Len() != MasterKeySize {
		t.Fatalf("NewMasterKey is %d bytes, want %d", k.Value.Len(), MasterKeySize)
	}
	if k.ModeChecked {
		t.Error("a key that was never read from a file claims its mode was checked")
	}
	if err := WriteMasterKey(p, k); err != nil {
		t.Fatalf("WriteMasterKey: %v", err)
	}
	got, err := LoadMasterKey(p)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if !Equal(got.Value, k.Value) {
		t.Error("the key did not survive the round trip")
	}
	if got.ModeChecked != (runtime.GOOS == "linux") {
		t.Errorf("ModeChecked = %v on %s", got.ModeChecked, runtime.GOOS)
	}

	// The trap this size check exists for: raw key material is binary, so one
	// key in 256 legitimately ends in 0x0a. Trimming a trailing newline the
	// way a game secret is trimmed would silently shorten those and produce a
	// box that cannot decrypt what the previous boot encrypted.
	withEOL := filepath.Join(dir, "echoed.key")
	if err := os.WriteFile(withEOL, append([]byte(k.Value.Reveal()), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadMasterKey(withEOL)
	if err == nil {
		t.Fatal("LoadMasterKey accepted a key with a trailing newline")
	}
	if !strings.Contains(err.Error(), "newline") {
		t.Errorf("LoadMasterKey = %v; the diagnostic should name the newline, because "+
			"`echo` is how the operator got here", err)
	}

	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(short); err == nil {
		t.Error("LoadMasterKey accepted a 9-byte key")
	}
	if _, err := LoadMasterKey(filepath.Join(dir, "absent.key")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("LoadMasterKey of a missing file = %v, want fs.ErrNotExist", err)
	}
}

func TestLoadMasterKeyRefusesAGroupOrWorldReadableFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "master.key")
	if err := WriteMasterKey(p, NewMasterKey()); err != nil {
		t.Fatalf("WriteMasterKey: %v", err)
	}

	if runtime.GOOS != "linux" {
		// Off Linux the check degrades rather than crashing, and the
		// degradation is REPORTED: a caller that treats this key as vetted
		// has ignored ModeChecked. That is the assertion here.
		k, err := LoadMasterKey(p)
		if err != nil {
			t.Fatalf("LoadMasterKey: %v", err)
		}
		if k.ModeChecked || OwnershipEnforced {
			t.Errorf("ModeChecked=%v OwnershipEnforced=%v on %s: this platform cannot report "+
				"unix permission bits and must not claim it checked them",
				k.ModeChecked, OwnershipEnforced, runtime.GOOS)
		}
		t.Skip("the refusal itself needs real permission bits; it runs for real on linux CI")
	}

	// A 0644 master.key is a silent downgrade of every TOTP seed on the box:
	// decryption keeps working perfectly and nothing anywhere reports a
	// problem.
	for _, perm := range []fs.FileMode{0o644, 0o640, 0o604, 0o660, 0o444} {
		if err := os.Chmod(p, perm); err != nil {
			t.Fatal(err)
		}
		_, err := LoadMasterKey(p)
		if !errors.Is(err, ErrInsecureMode) {
			t.Errorf("LoadMasterKey of a %04o key = %v, want ErrInsecureMode", perm, err)
		}
	}
	for _, perm := range []fs.FileMode{0o400, 0o600} {
		if err := os.Chmod(p, perm); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadMasterKey(p); err != nil {
			t.Errorf("LoadMasterKey of a %04o key = %v, want it accepted", perm, err)
		}
	}
}
