package hostops

import (
	"errors"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// TestFileKindsDeriveSection5Paths pins the derivation table against the
// filesystem layout, path by path.
//
// It is a table of literals on purpose. The production code derives every one
// of these through internal/instance so that section 5 is transcribed once;
// this test is the second opinion that says the transcription is right, and a
// second opinion computed the same way as the first is not one.
func TestFileKindsDeriveSection5Paths(t *testing.T) {
	const name = "pz-main"
	tests := []struct {
		kind   FileKind
		target fileTarget
		want   string
		mode   fs.FileMode
		group  string
		backup bool
	}{
		{FileDropIn, fileTarget{Instance: name},
			"/etc/systemd/system/gvm@pz-main.service.d/10-gvm.conf", 0o644, "", false},
		{FileAdoptGuard, fileTarget{Instance: name},
			"/etc/systemd/system/gvm@pz-main.service.d/20-adopt.conf", 0o644, "", false},
		{FileLegacyGuard, fileTarget{Instance: name, LegacyUnit: "zomboid.service"},
			"/etc/systemd/system/zomboid.service.d/20-gvm-adopt.conf", 0o644, "", false},
		{FileJobTimer, fileTarget{Instance: name, Job: "nightly"},
			"/etc/systemd/system/gvm-job@pz-main_nightly.timer", 0o644, "", false},
		{FileJobDesc, fileTarget{Instance: name, Job: "nightly"},
			"/etc/gvm/jobs/pz-main_nightly.json", 0o644, WebGroup, true},
		{FileInstanceSpec, fileTarget{Instance: name},
			"/etc/gvm/instances/pz-main.yaml", 0o640, WebGroup, true},
		{FileContentLock, fileTarget{Instance: name},
			"/etc/gvm/instances/pz-main.lock.json", 0o644, "", true},
		{FileManifestLock, fileTarget{Instance: name},
			"/etc/gvm/instances/pz-main.d/manifest.lock.yaml", 0o640, WebGroup, true},
		{FilePortsEcho, fileTarget{Instance: name},
			"/var/lib/gvm/instances/pz-main/ports.json", 0o640, "", false},
		// World-readable on purpose: the web tier has to be able to say "held
		// by an operator" rather than showing a stopped instance as down.
		{FileHold, fileTarget{Instance: name},
			"/run/gvm/pz-main/hold", 0o644, "", false},
	}

	seen := map[FileKind]bool{}
	for _, tc := range tests {
		d, ok := fileKinds[tc.kind]
		if !ok {
			t.Fatalf("%q is not a declared file kind", tc.kind)
		}
		seen[tc.kind] = true
		got, err := d.Path(tc.target)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if got != tc.want {
			t.Errorf("%s derived %s, want %s", tc.kind, got, tc.want)
		}
		if d.Mode.Perm() != tc.mode {
			t.Errorf("%s mode %04o, want %04o", tc.kind, d.Mode.Perm(), tc.mode)
		}
		if d.Group != tc.group {
			t.Errorf("%s group %q, want %q", tc.kind, d.Group, tc.group)
		}
		if d.Backup != tc.backup {
			t.Errorf("%s backup=%v, want %v", tc.kind, d.Backup, tc.backup)
		}
	}
	for _, k := range FileKinds() {
		if !seen[k] {
			t.Errorf("file kind %q has no row here; a kind whose path nobody asserted is "+
				"a root write nobody reviewed", k)
		}
	}
}

// TestNothingUnderTheUnitDirectoryKeepsABackup is a rule with a mechanism
// behind it.
//
// /etc/systemd/system is scanned by the service manager. A `.bak` beside a
// generated timer is litter in a directory another program parses, and gvm
// does not leave any. Nothing is lost: every generated unit file carries the
// spec sha256 in its header and re-renders from the spec.
func TestNothingUnderTheUnitDirectoryKeepsABackup(t *testing.T) {
	for _, k := range FileKinds() {
		d := fileKinds[k]
		p, err := d.Path(fileTarget{Instance: "pz-main", Job: "nightly", LegacyUnit: "zomboid.service"})
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if strings.HasPrefix(p, instance.SystemdUnitDir+"/") && d.Backup {
			t.Errorf("%s writes %s with a .bak beside it", k, p)
		}
	}
}

func TestDirKindsDeriveSection5Paths(t *testing.T) {
	const name = "pz-main"
	tests := []struct {
		kind     DirKind
		want     string
		mode     fs.FileMode
		group    string
		instOwns bool
	}{
		{DirSpecDir, "/etc/gvm/instances/pz-main.d", 0o750, WebGroup, false},
		{DirSecrets, "/etc/gvm/secrets/instances/pz-main", 0o700, "", false},
		{DirState, "/var/lib/gvm/instances/pz-main", 0o750, "", false},
		{DirSteamScripts, "/var/lib/gvm/instances/pz-main/steam", 0o750, "", false},
		{DirSnap, "/var/lib/gvm/instances/pz-main/snap", 0o750, "", false},
		{DirSteamHome, "/var/lib/gvm/cache/steam/homes/pz-main", 0o700, "", true},
		{DirLogs, "/var/log/gvm/pz-main", 0o750, "", true},
		{DirJobLogs, "/var/log/gvm/pz-main/jobs", 0o750, "", false},
		{DirBackups, "/var/backups/gvm/pz-main", 0o700, "", false},
		{DirSrv, "/srv/gvm/pz-main", 0o750, "", true},
		{DirJobs, "/etc/gvm/jobs", 0o750, WebGroup, false},
		// The two kinds whose path comes from the instance YAML rather than
		// from its name, so an adopted /opt/pzserver is located rather than
		// derived. want is what a MANAGED pz-main resolves to.
		{DirInstall, "/srv/gvm/pz-main/install", 0o750, "", true},
		{DirData, "/srv/gvm/pz-main/data", 0o750, "", true},
	}

	// A managed instance, for the spec-resolved kinds. DefaultSpecPaths is what
	// `gvm create` writes.
	managed := &spec.Instance{Spec: spec.InstanceSpec{Paths: instance.DefaultSpecPaths(name)}}
	managed.Metadata.Name = name

	seen := map[DirKind]bool{}
	for _, tc := range tests {
		d, ok := dirKinds[tc.kind]
		if !ok {
			t.Fatalf("%q is not a declared directory kind", tc.kind)
		}
		seen[tc.kind] = true
		got, ok := DirPathFor(tc.kind, managed)
		if !ok {
			t.Errorf("%s does not resolve for a managed instance", tc.kind)
		} else if got != tc.want {
			t.Errorf("%s derived %s, want %s", tc.kind, got, tc.want)
		}
		// A spec-resolved kind must NOT answer from the name alone: a plan that
		// derived /srv/gvm/pz-main/install for an adopted instance would name a
		// directory the verb does not touch.
		if _, byName := DirPath(tc.kind, name); byName == (d.SpecPath != nil) {
			t.Errorf("%s: DirPath by name returned ok=%v with SpecPath=%v", tc.kind,
				byName, d.SpecPath != nil)
		}
		if d.Mode.Perm() != tc.mode {
			t.Errorf("%s mode %04o, want %04o", tc.kind, d.Mode.Perm(), tc.mode)
		}
		if d.Group != tc.group {
			t.Errorf("%s group %q, want %q", tc.kind, d.Group, tc.group)
		}
		if d.OwnedByInstance != tc.instOwns {
			t.Errorf("%s OwnedByInstance=%v, want %v", tc.kind, d.OwnedByInstance, tc.instOwns)
		}
	}
	for _, k := range DirKinds() {
		if !seen[k] {
			t.Errorf("directory kind %q has no row here", k)
		}
	}
}

// TestBackupsDirectoryIsNotWorldReadable is D-44 as an assertion.
//
// install.sh made /var/backups world-readable and this is the one line
// deliberately not ported: a compromised workshop item running as the instance
// user must not be able to read every world the server has ever had.
func TestBackupsDirectoryIsNotWorldReadable(t *testing.T) {
	d := dirKinds[DirBackups]
	if d.Mode.Perm() != 0o700 || d.OwnedByInstance {
		t.Fatalf("%s is %04o owned-by-instance=%v; D-44 wants 0700 root:root",
			d.Path("pz-main"), d.Mode.Perm(), d.OwnedByInstance)
	}
	if s := dirKinds[DirSecrets]; s.Mode.Perm() != 0o700 || s.Group != "" {
		t.Fatalf("%s is %04o group %q; D-02 wants 0700 root:root",
			s.Path("pz-main"), s.Mode.Perm(), s.Group)
	}
}

// TestSecretsAreNotAFileKind guards the deliberate omission.
//
// A generic file.write that could carry a credential would put the plaintext
// into the request struct, and therefore into every socket frame, error string
// and params map that struct passes through. secrets.Value exists to make that
// impossible; a FileKind for it would undo it in one table row.
func TestSecretsAreNotAFileKind(t *testing.T) {
	for _, k := range FileKinds() {
		if strings.Contains(string(k), "secret") || strings.Contains(string(k), "key") {
			t.Errorf("file kind %q looks like a credential; secret writes get their own "+
				"verb and their own type", k)
		}
		d := fileKinds[k]
		p, err := d.Path(fileTarget{Instance: "pz-main", Job: "nightly", LegacyUnit: "zomboid.service"})
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if strings.HasPrefix(p, instance.SecretsRoot) {
			t.Errorf("file kind %q writes into %s", k, instance.SecretsRoot)
		}
	}
}

func TestLegacyUnitValidation(t *testing.T) {
	good := []string{"zomboid.service", "minecraft-1.service", "valheim_server.service", "srcds@1.service"}
	for _, u := range good {
		if _, err := validateLegacyUnit(u); err != nil {
			t.Errorf("%q was rejected: %v", u, err)
		}
	}
	// Every one of these, accepted, is a root-owned drop-in written somewhere
	// systemd or the operator did not expect.
	bad := []string{
		"",
		"zomboid",       // not a service
		"zomboid.timer", // the guard's content is a Conflicts= against a service
		"../../../etc/cron.d/x.service",
		"..zomboid.service",
		"/etc/systemd/system/x.service",
		"zomboid.service\n[Service]\nExecStart=/bin/sh.service",
		"gvm@pz-main.service", // gvm's own unit is not the legacy one
		"gvm-job@pz-main_nightly.service",
	}
	for _, u := range bad {
		if got, err := validateLegacyUnit(u); err == nil {
			t.Errorf("%q was accepted and would write into %s", u, got)
		} else if !errors.Is(err, ErrLegacyUnit) {
			t.Errorf("%q failed with %v, not ErrLegacyUnit", u, err)
		}
	}
}

// TestMkdirLeafRefusesAMissingParent is the reason MkdirAll is not used.
//
// MkdirAll would give every intermediate directory the leaf's mode, so
// ensuring /var/log/gvm/pz-main at 0750 on a host where systemd-tmpfiles had
// not run would silently create /var/log/gvm at 0750 where section 5.1 says
// 0755 -- a wrong mode on a shared directory, applied by the program that is
// supposed to be enforcing modes, with nothing anywhere saying so.
func TestMkdirLeafRefusesAMissingParent(t *testing.T) {
	f := newFakeFS() // nothing exists at all
	_, err := mkdirLeaf(f, "/var/log/gvm/pz-main", 0o750, "", "")
	if !errors.Is(err, ErrMissingParent) {
		t.Fatalf("got %v, want ErrMissingParent", err)
	}
	if len(f.trace) != 0 {
		t.Fatalf("refused and still touched the filesystem: %v", f.trace)
	}
	if !strings.Contains(err.Error(), "/var/log/gvm") {
		t.Errorf("the error does not name the missing directory: %v", err)
	}
}

// TestMkdirLeafChownsBeforeChmod encodes the setgid lesson.
//
// On Linux a chown clears the setgid and setuid bits, so a chmod that ran
// first is silently undone. `gvm run-prepare` already carries this for
// /run/gvm/<name>; none of the kinds here are setgid today, and doing it in
// the safe order anyway means the day one is added it works.
func TestMkdirLeafChownsBeforeChmod(t *testing.T) {
	f := newFakeFS("/srv/gvm")
	changed, err := mkdirLeaf(f, "/srv/gvm/pz-main", 0o750, "gvm-pz-main", "")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("creating a directory reported no change")
	}
	want := []string{
		"mkdir /srv/gvm/pz-main 0750",
		"chown /srv/gvm/pz-main gvm-pz-main:",
		"chmod /srv/gvm/pz-main 0750",
	}
	if strings.Join(f.trace, "|") != strings.Join(want, "|") {
		t.Fatalf("trace:\n\t%s\nwant:\n\t%s", strings.Join(f.trace, "\n\t"), strings.Join(want, "\n\t"))
	}
}

func TestMkdirLeafIsIdempotentAndReportsNoChange(t *testing.T) {
	f := newFakeFS("/srv/gvm")
	if _, err := mkdirLeaf(f, "/srv/gvm/pz-main", 0o750, "", ""); err != nil {
		t.Fatal(err)
	}
	f.trace = nil
	changed, err := mkdirLeaf(f, "/srv/gvm/pz-main", 0o750, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a directory that was already right reported a change; an apply that " +
			"counts its own previous run as work cannot print '= unchanged'")
	}
	// A wrong mode IS a change, and is corrected.
	f.files["/srv/gvm/pz-main"].mode = 0o777
	changed, err = mkdirLeaf(f, "/srv/gvm/pz-main", 0o750, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a directory with the wrong mode reported no change")
	}
	if got := f.files["/srv/gvm/pz-main"].mode.Perm(); got != 0o750 {
		t.Errorf("mode is %04o after the fix, want 0750", got)
	}
}

func TestMkdirLeafRefusesAFileWhereADirectoryBelongs(t *testing.T) {
	f := newFakeFS("/srv/gvm")
	f.files["/srv/gvm/pz-main"] = &fakeEntry{mode: 0o644}
	if _, err := mkdirLeaf(f, "/srv/gvm/pz-main", 0o750, "", ""); !errors.Is(err, ErrNotADirectory) {
		t.Fatalf("got %v, want ErrNotADirectory", err)
	}
}

// TestWriteFileSkipsAnIdenticalWrite matters because the reconciler reloads
// systemd when a unit file changed. Rewriting identical bytes bumps the mtime
// and makes the next reload look like it had a reason.
func TestWriteFileSkipsAnIdenticalWrite(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	d := fileKinds[FileDropIn]
	p, err := d.Path(fileTarget{Instance: "pz-main"})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("[Service]\nUser=pzserver\n")

	changed, err := writeFile(f, d, p, body)
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	f.trace = nil
	changed, err = writeFile(f, d, p, body)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("identical bytes reported a change")
	}
	for _, op := range f.trace {
		if strings.HasPrefix(op, "write ") {
			t.Errorf("identical bytes still went to disk: %s", op)
		}
	}

	// A wrong mode alone is enough to rewrite: a drop-in the web tier can
	// write is worse than one whose bytes are stale.
	f.files[p].mode = 0o666
	changed, err = writeFile(f, d, p, body)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a file with the wrong mode was left alone")
	}
	if got := f.wrote(t, p).mode.Perm(); got != 0o644 {
		t.Errorf("mode is %04o after the rewrite, want 0644", got)
	}
}

// TestWriteFileCreatesOnlyTheParentItOwns: a drop-in without its `.d`
// directory is not a thing, so file.write creates that one. It does not create
// /etc/systemd/system, which belongs to the distribution.
func TestWriteFileCreatesTheDropInDirectory(t *testing.T) {
	f := newFakeFS("/etc/systemd/system")
	d := fileKinds[FileDropIn]
	p, _ := d.Path(fileTarget{Instance: "pz-main"})
	if _, err := writeFile(f, d, p, []byte("x")); err != nil {
		t.Fatal(err)
	}
	dir := path.Dir(p)
	if e, ok := f.files[dir]; !ok || !e.dir {
		t.Fatalf("%s was not created", dir)
	} else if e.mode.Perm() != 0o755 {
		t.Errorf("%s is %04o, want 0755", dir, e.mode.Perm())
	}

	// The distribution's directory is not gvm's to create.
	g := newFakeFS()
	if _, err := writeFile(g, d, p, []byte("x")); !errors.Is(err, ErrMissingParent) {
		t.Fatalf("got %v, want ErrMissingParent", err)
	}
}

func TestWriteFileKeepsABackupOnlyWhereDeclared(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	spec := fileKinds[FileInstanceSpec]
	p, _ := spec.Path(fileTarget{Instance: "pz-main"})
	if _, err := writeFile(f, spec, p, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFile(f, spec, p, []byte("second")); err != nil {
		t.Fatal(err)
	}
	bak := f.wrote(t, p+".bak")
	if string(bak.data) != "first" {
		t.Errorf(".bak holds %q, want the previous version", bak.data)
	}
	if got := f.wrote(t, p).group; got != WebGroup {
		t.Errorf("group is %q, want %q so the dashboard can render the spec", got, WebGroup)
	}
}

// TestWriteFileReassertsOwnershipOnAnOtherwiseIdenticalFile.
//
// The three kinds that declare a group -- the instance spec, the manifest lock
// and a job descriptor -- are exactly the ones whose bytes CONVERGE on the
// first apply: `gvm apply` reads the spec off disk and writes those same bytes
// back, so the content comparison can never differ. That made "the write is
// skipped" and "the ownership is never re-asserted" the same sentence, and
// 0640 root:gvm is what lets gvm-web read a spec at all.
//
// The drift is reachable without any failure: a restore, an rsync or a hand
// edit. It is also reachable WITH one -- `gvm create` writes the spec with no
// preceding WebGroup directory step, so a box with no gvm group yet leaves a
// 0640 root:root file, and create refuses to re-run. Either way apply is the
// repair path, and the sibling directory kinds already self-heal this way in
// mkdirLeaf, which dirs.go advertises as "the mode and owner are re-asserted".
func TestWriteFileReassertsOwnershipOnAnOtherwiseIdenticalFile(t *testing.T) {
	for _, kind := range []FileKind{FileInstanceSpec, FileManifestLock, FileJobDesc} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFakeFS(controlPlaneDirs()...)
			// The manifest lock lives in the instance's .d directory, which an
			// earlier step of a real plan creates.
			f.mkdirs(instance.PathsFor("pz-main").SpecDir)
			d := fileKinds[kind]
			p, err := d.Path(fileTarget{Instance: "pz-main", Job: "nightly"})
			if err != nil {
				t.Fatal(err)
			}
			body := []byte("apiVersion: gvm/v1\n")
			if _, err := writeFile(f, d, p, body); err != nil {
				t.Fatal(err)
			}
			if got := f.files[p].group; got != WebGroup {
				t.Fatalf("after the first write the group is %q, want %q", got, WebGroup)
			}

			// The drift: same bytes, same mode, root:root.
			f.files[p].group = ""
			f.trace = nil

			changed, err := writeFile(f, d, p, body)
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range f.trace {
				if strings.HasPrefix(op, "write ") {
					t.Errorf("identical bytes still went to disk: %s", op)
				}
			}
			if got := f.files[p].group; got != WebGroup {
				t.Errorf("the group is still %q after an apply; nothing else in gvm repairs it, "+
					"so gvm-web can never read this file again", got)
			}
			_ = changed
		})
	}
}

// TestWriteFileReportsAFailedOwnershipRepair: a repair that could not happen
// must be an error, not a silent pass. `gvm apply` stops at the first failed
// step, and "the group could not be set" is the operator's cue to create the
// gvm group.
func TestWriteFileReportsAFailedOwnershipRepair(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	d := fileKinds[FileInstanceSpec]
	p, err := d.Path(fileTarget{Instance: "pz-main"})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("apiVersion: gvm/v1\n")
	if _, err := writeFile(f, d, p, body); err != nil {
		t.Fatal(err)
	}
	f.files[p].group = ""
	f.failChown = true

	if _, err := writeFile(f, d, p, body); err == nil {
		t.Fatal("a failed ownership repair on an otherwise-identical file was swallowed")
	}
}

// TestMkdirLeafDoesNotStripSetgidFromADirectoryThatIsAlreadyRight.
//
// mkdirLeaf chmods unconditionally, and `changed` is computed from
// fi.Mode().Perm(), which masks setgid off -- so an on-disk 2750 compares equal
// to a declared 0750 and is then chmodded to a plain 0750. The live directory
// that reaches this is /run/gvm/<name>: FileHold declares ParentMode 0o750 and
// no owner, `gvm run-prepare` sets that directory to setgid 2750
// gvm-<n>:gvm-console (D-09/D-10), and a hold is written while the runner is
// still alive. Losing the bit means the runner's next status.json lands in the
// wrong group and gvm-web gets EACCES.
func TestMkdirLeafDoesNotStripSetgidFromADirectoryThatIsAlreadyRight(t *testing.T) {
	f := newFakeFS("/run/gvm")
	const p = "/run/gvm/pz-main"
	// What run-prepare left: setgid, and the perms mkdirLeaf will be asked for.
	f.files[p] = &fakeEntry{dir: true, mode: fs.ModeSetgid | 0o750,
		owner: "gvm-pz-main", group: "gvm-console"}
	f.trace = nil

	changed, err := mkdirLeaf(f, p, 0o750, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a directory that already matches reported a change")
	}
	if len(f.trace) != 0 {
		t.Errorf("mkdirLeaf touched a directory that was already right: %v", f.trace)
	}
	// Asserted on the stored mode, not the trace: fakeFS formats its chmod line
	// with Perm(), which masks the very bit under test.
	if f.files[p].mode&fs.ModeSetgid == 0 {
		t.Fatalf("the setgid bit was stripped from %s; it is the whole mechanism of D-09/D-10, "+
			"and nothing records that the mode was altered", p)
	}
	if f.files[p].group != "gvm-console" {
		t.Errorf("the group became %q; mkdirLeaf was given no owner and must not invent one",
			f.files[p].group)
	}
}
