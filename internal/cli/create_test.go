package cli

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The Milestone A line, as a test.
//
// `gvm create valheim-1 --game valheim --var public_name=Test` must produce a
// VALID instance YAML with a UUIDv7 uid and a materialised port set, and it
// must touch nothing else on the host. The last clause is the one worth
// asserting mechanically: create is deliberately not apply, and a create that
// quietly made a user or a directory would make the file stop being a
// reviewable proposal.
func TestCreateWritesTheInstanceAndNothingElse(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "",
		"create", "valheim-1", "--game", "valheim", "--var", "public_name=Test")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	}

	specPath := instance.PathsFor("valheim-1").Spec
	if got := h.FS.writes; len(got) != 1 || got[0] != specPath {
		t.Fatalf("create wrote %v; it must write exactly %s and nothing else", got, specPath)
	}
	if len(h.FS.dirs) != 0 {
		t.Errorf("create made directories: %v", h.FS.dirs)
	}
	// The one chown is the spec file's own 0640 root:gvm, which is part of
	// writing it (section 5). Anything else would be create reaching outside
	// the file it is allowed to produce.
	for _, c := range h.FS.chowns {
		if !strings.HasPrefix(c, specPath+" ") {
			t.Errorf("create chowned something other than the file it wrote: %s", c)
		}
	}
	if len(h.Systemd.calls) != 0 {
		t.Errorf("create talked to systemd: %v", h.Systemd.calls)
	}
	if len(h.Users.have) != 0 {
		t.Errorf("create made an account: %v", h.Users.have)
	}

	in := mustReadInstance(t, h, "valheim-1")
	if _, err := spec.ParseUID(string(in.Metadata.UID)); err != nil {
		t.Fatalf("metadata.uid: %v", err)
	}
	// Version 7 specifically: the id sorts chronologically, and every consumer
	// downstream (the mirror's index, an archive listing) is built on that.
	if v := string(in.Metadata.UID)[14]; v != '7' {
		t.Errorf("uid %q is not a v7 (version nibble %q)", in.Metadata.UID, v)
	}
	if in.Spec.Game != "valheim" || in.Spec.Manifest.SHA256 == "" {
		t.Errorf("the manifest pin was not recorded: %+v", in.Spec.Manifest)
	}
	if !in.Spec.ManagedInstall {
		t.Error("a created instance must be managed; managed_install:false is the adoption flag")
	}
	if in.Spec.User != instance.UserName("valheim-1") {
		t.Errorf("user = %q", in.Spec.User)
	}

	// Materialised, and all three of Valheim's ports: the third is unused by
	// the game and must still be held, or another instance gets handed a port
	// inside the range Valheim reserves.
	for _, name := range []string{"game", "query", "spare"} {
		if in.Spec.Ports[name] == 0 {
			t.Errorf("port %q was not materialised: %v", name, in.Spec.Ports)
		}
	}
	if in.Spec.Ports["game"] != 2456 {
		t.Errorf("game port = %d on an empty host, want the manifest default 2456",
			in.Spec.Ports["game"])
	}
	if in.Spec.Resources.MemoryMax == "" {
		t.Error("resources were not computed")
	}
	if !strings.Contains(out, "nothing else on this host changed") {
		t.Errorf("the output does not say what was NOT done:\n%s", out)
	}
}

// A second instance of the same game shifts by the manifest's own stride, as a
// GROUP: Valheim's three ports must stay contiguous or the server is reachable
// and unjoinable.
func TestASecondInstanceShiftsByTheManifestStride(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=One")
	seedInstance(t, h, "valheim-2", "valheim", "--var", "public_name=Two")

	one := mustReadInstance(t, h, "valheim-1")
	two := mustReadInstance(t, h, "valheim-2")
	if two.Spec.Ports["game"] != one.Spec.Ports["game"]+10 {
		t.Errorf("second instance game port = %d, want %d (stride 10)",
			two.Spec.Ports["game"], one.Spec.Ports["game"]+10)
	}
	if two.Spec.Ports["query"] != two.Spec.Ports["game"]+1 {
		t.Errorf("the group did not shift as a unit: %v", two.Spec.Ports)
	}
	if one.Metadata.UID == two.Metadata.UID {
		t.Error("two instances share a uid")
	}
}

// A required var with no default is refused, and the refusal names the flag
// that would fix it rather than the schema field it came from.
func TestCreateRefusesAMissingRequiredVar(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "create", "valheim-1", "--game", "valheim")
	if code == 0 {
		t.Fatal("create succeeded with no public_name")
	}
	if !strings.Contains(errb, "--var public_name=") {
		t.Errorf("the refusal does not name the fix:\n%s", errb)
	}
	if len(h.FS.writes) != 0 {
		t.Errorf("a refused create still wrote %v", h.FS.writes)
	}
}

// An undeclared --var is refused rather than carried into the file. A value
// nothing renders would sit in the YAML doing nothing, and the operator would
// believe they had configured something.
func TestCreateRefusesAnUndeclaredVar(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "create", "valheim-1", "--game", "valheim",
		"--var", "public_name=Test", "--var", "publik_name=Typo")
	if code == 0 {
		t.Fatal("create accepted a variable the manifest does not declare")
	}
	if !strings.Contains(errb, "publik_name") {
		t.Errorf("the refusal does not name the offending variable:\n%s", errb)
	}
}

// Creating over an existing definition is refused. The YAML is the server;
// overwriting it silently is how an operator loses a port set and a uid at
// once, and the uid is the one thing that cannot be reconstructed.
func TestCreateNeverOverwrites(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=One")
	before := mustReadInstance(t, h, "valheim-1")

	code, _, errb := runWith(t, h, "", "create", "valheim-1", "--game", "valheim",
		"--var", "public_name=Two")
	if code == 0 {
		t.Fatal("create overwrote an existing instance")
	}
	if !strings.Contains(errb, "already exists") {
		t.Errorf("stderr = %s", errb)
	}
	if after := mustReadInstance(t, h, "valheim-1"); after.Metadata.UID != before.Metadata.UID {
		t.Error("the existing definition was modified")
	}
}

// --dry-run prints the document and writes nothing, which is what makes it
// safe to use to find out what create WOULD do.
func TestCreateDryRunWritesNothing(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "", "create", "valheim-1", "--game", "valheim",
		"--var", "public_name=Test", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if len(h.FS.writes) != 0 {
		t.Errorf("--dry-run wrote %v", h.FS.writes)
	}
	if !strings.Contains(out, "kind: Instance") {
		t.Errorf("the document was not printed:\n%s", out)
	}
}

func TestParseCreateArgsRejectsRepeatedVars(t *testing.T) {
	env := &Env{Stderr: &strings.Builder{}}
	_, err := parseCreateArgs(env, []string{"v1", "--game", "valheim",
		"--var", "public_name=A", "--var", "public_name=B"})
	if err == nil {
		t.Fatal("a repeated --var was accepted; last-wins would silently drop a typo")
	}
}

// The instance name is validated before it reaches a path. Every derived path
// in section 5 is built from it, and an unvalidated one is a path traversal
// with extra steps.
func TestParseCreateArgsValidatesTheName(t *testing.T) {
	env := &Env{Stderr: &strings.Builder{}}
	for _, bad := range []string{"../etc", "UPPER", "a", strings.Repeat("x", 40)} {
		if _, err := parseCreateArgs(env, []string{bad, "--game", "valheim"}); err == nil {
			t.Errorf("%q was accepted as an instance name", bad)
		}
	}
}

// TestBackupKeepIsDeterministic.
//
// m.Spec.Jobs is a Go map, so ranging it is randomized. A manifest may
// legitimately recommend two backup-action jobs -- a nightly and a weekly with
// a different retention -- while an instance carries ONE spec.backup.keep, so
// the code has to choose; it just must not choose by dice. Two `gvm create`
// runs against the same manifest have to write the same YAML, and nothing else
// constrains this: recommended_jobs has no manifest-side validation, and
// /etc/gvm/games overrides are a documented, supported way to author one.
//
// A single call passes by chance, so this loops.
func TestBackupKeepIsDeterministic(t *testing.T) {
	m := &spec.Manifest{}
	m.Spec.Jobs = spec.RecommendedJobs{
		"backup": {At: "04:30", Keep: 14},
		"weekly": {Action: "backup", At: "05:30", Keep: 4},
		"update": {At: "06:30", Keep: 99},
	}
	first := backupKeep(m)
	for range 200 {
		if got := backupKeep(m); got != first {
			t.Fatalf("backupKeep returned %d and then %d for the same manifest; the retention "+
				"written into an instance YAML depends on Go's map iteration order", first, got)
		}
	}

	// And it prefers a job whose resolved action is actually a backup, rather
	// than whichever keep-bearing job it met first.
	if first != 14 && first != 4 {
		t.Errorf("backupKeep = %d, want the keep of a backup-action job (14 or 4), not of the "+
			"update job that merely declares one", first)
	}

	// The floor still applies when no job declares a retention at all.
	empty := &spec.Manifest{}
	empty.Spec.Jobs = spec.RecommendedJobs{"update": {At: "06:30"}}
	if got := backupKeep(empty); got != 7 {
		t.Errorf("backupKeep with no keep declared = %d, want the 7-archive floor", got)
	}
}

// TestCreateRefusesAUserItWouldNotThenUse.
//
// --user writes spec.user, but buildInstance hard-codes managed_install: true,
// and every managed consumer derives the account from the NAME instead:
// accountAction creates gvm-<instance>, hostops.userEnsure ensures gvm-<instance>,
// while dirEnsure chowns the instance's directories to spec.user and the drop-in
// renders User=<spec.user>. So the plan line, the account that gets created, the
// chown target and the unit's User= all disagree, and the apply dies part-way
// through when the named account does not exist.
//
// A foreign account is the ADOPTED case (managed_install: false, section 5), and
// `gvm create` writes nothing that predates anything.
func TestCreateRefusesAUserItWouldNotThenUse(t *testing.T) {
	h := newTestHost(t)
	before := len(h.FS.writes)

	code, _, errb := runWith(t, h, "", "create", "valheim-1", "--game", "valheim",
		"--var", "public_name=Test", "--user", "pzserver")
	if code == 0 {
		t.Fatal("gvm create accepted a --user it would then not run as")
	}
	if !strings.Contains(errb, "adopt") {
		t.Errorf("the refusal does not point at the command that DOES take an existing "+
			"account:\n%s", errb)
	}
	if len(h.FS.writes) != before {
		t.Errorf("a refused create wrote %v", h.FS.writes[before:])
	}

	// The control: the same command without --user still works, so the refusal
	// is about the flag and not about the fixture.
	if code, _, errb := runWith(t, h, "", "create", "valheim-1", "--game", "valheim",
		"--var", "public_name=Test"); code != 0 {
		t.Fatalf("create without --user: exit %d: %s", code, errb)
	}
	if got := mustReadInstance(t, h, "valheim-1").Spec.User; got != instance.UserName("valheim-1") {
		t.Errorf("spec.user = %q, want the derived account", got)
	}
}
