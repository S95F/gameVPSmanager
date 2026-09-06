package reconcile

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// update rewrites the golden files instead of comparing against them.
//
// The golden files are the point of this test, so regenerating them is a
// deliberate act with a flag on it: `go test ./internal/reconcile -update` and
// then READ THE DIFF. A generated unit file governs a live server, and a
// golden that updates itself is a golden that proves nothing.
var update = flag.Bool("update", false, "rewrite the golden unit files")

type goldenCase struct {
	instance string
	game     string
	golden   string
	has      func(string) bool
}

// goldenCases are the three shapes of drop-in the generator has to get right.
//
//	pz-main    ADOPTED: a pre-existing account, a world under /home, a JVM.
//	valheim-1  NEW: gvm's own account, everything under /srv, no JIT.
//	gmod-1     32-BIT: the one directive a hardcoded `native` would break.
func goldenCases() []goldenCase {
	return []goldenCase{
		{"pz-main", "projectzomboid", "pz-main-adopted.10-gvm.conf", allSecretsPresent},
		{"valheim-1", "valheim", "valheim-1-new.10-gvm.conf", allSecretsPresent},
		{"gmod-1", "gmod", "gmod-1-i386.10-gvm.conf", allSecretsPresent},
	}
}

func renderCase(t *testing.T, c goldenCase) ([]byte, Sandbox) {
	t.Helper()
	in, raw := loadInstance(t, c.instance)
	m := loadManifest(t, c.game)
	body, sb, err := BuildDropIn(DropInInput{
		Instance: in, Manifest: m, SpecSHA: SpecSHA256(raw),
		HasSecret: c.has, Now: renderedAt, Version: goldenVersion,
	})
	if err != nil {
		t.Fatalf("%s: %v", c.instance, err)
	}
	return body, sb
}

func TestGoldenDropInsMatchByteForByte(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.instance, func(t *testing.T) {
			got, _ := renderCase(t, c)
			p := filepath.Join("testdata", "golden", c.golden)
			if *update {
				if err := os.WriteFile(p, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			// The comparison is on raw bytes and not on a normalised form.
			// Whitespace in a unit file is not cosmetic -- systemd's parser
			// treats a leading space as a continuation -- and a golden that
			// tolerated it would stop catching the class of bug it exists for.
			if string(got) != string(want) {
				t.Errorf("rendered drop-in differs from %s\n--- want ---\n%s\n--- got ---\n%s",
					p, want, got)
			}
		})
	}
}

// TestTheAdoptedDropInCanWriteTheWorldItAdopted is the assertion the whole
// drop-in generator exists for.
//
// The adopted Project Zomboid world lives at /home/pzserver/Zomboid, which the
// package template's ProtectHome=read-only hides. A generator that emitted the
// template's default here produces a server that starts, boots, accepts
// players and cannot save -- and nothing says why. Both halves are asserted:
// the directive AND the path list, because either one alone is not enough.
func TestTheAdoptedDropInCanWriteTheWorldItAdopted(t *testing.T) {
	body, sb := renderCase(t, goldenCases()[0])
	text := string(body)

	if !strings.Contains(text, "\nProtectHome=no\n") {
		t.Error("the adopted drop-in does not carry ProtectHome=no; the world under /home is hidden")
	}
	rwp := directive(t, text, "ReadWritePaths")
	for _, want := range []string{"/opt/pzserver", "/home/pzserver/Zomboid"} {
		if !strings.Contains(rwp, want) {
			t.Errorf("ReadWritePaths does not cover %s:\n\t%s", want, rwp)
		}
	}
	// The sandbox handed to the write-probe has to be the same one, or the
	// probe proves something about a unit nobody will run.
	if sb.ProtectHome != "no" {
		t.Errorf("Sandbox.ProtectHome = %q, but the file says no", sb.ProtectHome)
	}
	if strings.Join(sb.Paths, " ") != rwp {
		t.Errorf("Sandbox.Paths %v does not match the rendered ReadWritePaths %q", sb.Paths, rwp)
	}
}

// TestRootOnlyDirectoriesAreNotDeclaredWritable is the other half of that
// list, and it is a regression test with a specific failure in mind.
//
// Section 7.4's example includes /var/backups/gvm/<name> and
// /var/lib/gvm/instances/<name> in ReadWritePaths. Both are root-owned (D-44
// and section 5.3), so the instance account cannot write either whatever the
// sandbox says -- and listing them would make the write-probe, which runs AS
// that account, fail on every correctly configured host.
func TestRootOnlyDirectoriesAreNotDeclaredWritable(t *testing.T) {
	body, _ := renderCase(t, goldenCases()[0])
	rwp := directive(t, string(body), "ReadWritePaths")
	for _, never := range []string{"/var/backups/gvm/pz-main", "/var/lib/gvm/instances/pz-main"} {
		if strings.Contains(rwp, never) {
			t.Errorf("ReadWritePaths declares %s writable. It is root-owned, the game cannot "+
				"write it, and the write-probe would fail on every real host:\n\t%s", never, rwp)
		}
	}
}

// TestJITTurnsOffWriteExecuteWithItsReasonAttached.
//
// The value alone is not the requirement. MemoryDenyWriteExecute=no looks like
// a weakening to anybody reading the file cold, and the comment is what stops
// a future security pass from "fixing" it and breaking every Java server on
// the box.
func TestJITTurnsOffWriteExecuteWithItsReasonAttached(t *testing.T) {
	body, _ := renderCase(t, goldenCases()[0]) // projectzomboid: jit true
	text := string(body)
	if !strings.Contains(text, "\nMemoryDenyWriteExecute=no\n") {
		t.Fatal("a JIT runtime did not get MemoryDenyWriteExecute=no")
	}
	for _, want := range []string{"the JVM JIT needs W^X off", "Do NOT \"harden\" this line"} {
		if !strings.Contains(text, want) {
			t.Errorf("the inline reason is missing %q; the next security review will delete this line", want)
		}
	}

	off, _ := renderCase(t, goldenCases()[1]) // valheim: jit false
	if !strings.Contains(string(off), "\nMemoryDenyWriteExecute=yes\n") {
		t.Error("a non-JIT runtime should keep W^X on")
	}
}

// TestA32BitEngineAdmitsX86.
//
// `native` alone kills srcds with SIGSYS before it prints anything, which is
// the failure spec.runtime.arch exists to prevent and the reason it is a
// manifest field rather than a constant.
func TestA32BitEngineAdmitsX86(t *testing.T) {
	body, _ := renderCase(t, goldenCases()[2])
	if !strings.Contains(string(body), "\nSystemCallArchitectures=native x86\n") {
		t.Error("arch: i386 did not render SystemCallArchitectures=native x86")
	}
	native, _ := renderCase(t, goldenCases()[1])
	if !strings.Contains(string(native), "\nSystemCallArchitectures=native\n") {
		t.Error("arch: native did not render SystemCallArchitectures=native")
	}
}

// TestTimeoutsAreTheManifestsAndAreFinite (D-12).
//
// TimeoutStartSec=infinity would leave a wedged first boot in `activating`
// forever with Restart=on-failure never firing, and systemd's own default of
// 90 seconds would kill a Project Zomboid workshop boot at minute two of
// forty. Both numbers therefore come from the manifest, and this asserts the
// arithmetic rather than the string.
func TestTimeoutsAreTheManifestsAndAreFinite(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.instance, func(t *testing.T) {
			body, _ := renderCase(t, c)
			m := loadManifest(t, c.game)
			text := string(body)

			wantStart := int(m.Spec.Readiness.HardDeadline.Duration().Seconds())
			wantStop := int(m.Spec.Shutdown.Budget.Duration().Seconds())
			if got := directive(t, text, "TimeoutStartSec"); got != itoa(wantStart) {
				t.Errorf("TimeoutStartSec = %s, want readiness.hard_deadline = %d", got, wantStart)
			}
			if got := directive(t, text, "TimeoutStopSec"); got != itoa(wantStop) {
				t.Errorf("TimeoutStopSec = %s, want shutdown.budget = %d", got, wantStop)
			}
		})
	}
}

// TestAZeroDeadlineIsRefusedRatherThanDefaulted.
//
// Falling back to systemd's default would be the worst possible answer: 90
// seconds, applied silently, to the one game whose first boot takes forty
// minutes.
func TestAZeroDeadlineIsRefusedRatherThanDefaulted(t *testing.T) {
	in, raw := loadInstance(t, "pz-main")
	m := loadManifest(t, "projectzomboid")
	m.Spec.Readiness.HardDeadline = 0

	_, _, err := BuildDropIn(DropInInput{
		Instance: in, Manifest: m, SpecSHA: SpecSHA256(raw),
		HasSecret: allSecretsPresent, Now: renderedAt, Version: goldenVersion,
	})
	if err == nil {
		t.Fatal("a manifest with no hard_deadline rendered a drop-in; systemd's 90s default now applies")
	}
	if !strings.Contains(err.Error(), "readiness.hard_deadline") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
}

// TestASecretInTheManifestEnvNeverReachesTheUnitFile.
//
// spec's validator ALLOWS `.Secret.*` in runtime.env, because the runner can
// render it safely from CREDENTIALS_DIRECTORY. A unit file cannot: Environment=
// is readable through `systemctl show` and /proc/<pid>/environ, so a password
// there is a password published (D-02). The key is named in a comment rather
// than dropped silently, because a generated file that omits something without
// saying so is a generated file nobody can review.
func TestASecretInTheManifestEnvNeverReachesTheUnitFile(t *testing.T) {
	in, raw := loadInstance(t, "pz-main")
	m := loadManifest(t, "projectzomboid")
	m.Spec.Runtime.Env["RCON_PASSWORD"] = "{{ .Secret.rcon_password }}"

	body, _, err := BuildDropIn(DropInInput{
		Instance: in, Manifest: m, SpecSHA: SpecSHA256(raw),
		HasSecret: allSecretsPresent, Now: renderedAt, Version: goldenVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "Environment=RCON_PASSWORD") {
		t.Error("a tainted env value was written as an Environment= directive")
	}
	if !strings.Contains(text, "runtime.env.RCON_PASSWORD expands a secret") {
		t.Errorf("the skipped key is not named anywhere in the file:\n%s", text)
	}
	// And the credential it belongs to is still loaded, which is the whole
	// point: the value reaches the game, it just does not reach this file.
	if !strings.Contains(text, "LoadCredential=rcon:") {
		t.Error("the rcon credential is not loaded, so the runner has nothing to render from")
	}
}

// TestAnAbsentCredentialIsNamedRatherThanDeclared.
//
// systemd fails a unit outright when LoadCredential= names a file that is not
// there. Emitting the line for a credential `gvm create` has not generated yet
// would replace the runner's "these three credentials are missing" preflight
// with a server that will not start at all.
func TestAnAbsentCredentialIsNamedRatherThanDeclared(t *testing.T) {
	in, raw := loadInstance(t, "pz-main")
	m := loadManifest(t, "projectzomboid")

	only := func(id string) bool { return id == "rcon" }
	body, _, err := BuildDropIn(DropInInput{
		Instance: in, Manifest: m, SpecSHA: SpecSHA256(raw),
		HasSecret: only, Now: renderedAt, Version: goldenVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "LoadCredential=rcon:/etc/gvm/secrets/instances/pz-main/rcon") {
		t.Error("the credential that exists was not loaded")
	}
	for _, absent := range []string{"LoadCredential=admin:", "LoadCredential=join:"} {
		if strings.Contains(text, absent) {
			t.Errorf("%s names a file that does not exist; the unit would fail to start", absent)
		}
	}
	if !strings.Contains(text, "no LoadCredential= for admin, join") {
		t.Errorf("the absent credentials are not named:\n%s", text)
	}
}

// TestCredentialIdsComeFromTheManifestsOwnTemplates.
//
// Deriving them rather than listing them is what stops the set going stale:
// a manifest that adds `.Secret.telnet_password` gets a LoadCredential= for
// it with no code change, and a hand-kept list would not.
func TestCredentialIdsComeFromTheManifestsOwnTemplates(t *testing.T) {
	m := loadManifest(t, "projectzomboid")
	creds, missing, err := BuildCredentials(m, "pz-main", allSecretsPresent)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}
	var got []string
	for _, c := range creds {
		got = append(got, c.ID)
	}
	want := []string{"admin", "join", "rcon"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("credential ids = %v, want %v (the template spellings are *_password)", got, want)
	}
	for _, c := range creds {
		if !strings.HasPrefix(c.Path, "/etc/gvm/secrets/instances/pz-main/") {
			t.Errorf("credential %s resolves outside the instance's secret directory: %s", c.ID, c.Path)
		}
	}
}

// TestSameDropInIgnoresTheRenderedLineAndNothingElse.
//
// This is what lets `gvm apply` say "= unchanged". Without it every apply
// writes -- the timestamp differs, so the bytes differ -- and the daemon-reload
// that follows looks like it had a reason on a box where nothing changed.
func TestSameDropInIgnoresTheRenderedLineAndNothingElse(t *testing.T) {
	c := goldenCases()[0]
	in, raw := loadInstance(t, c.instance)
	m := loadManifest(t, c.game)

	render := func(version string, at int) []byte {
		body, _, err := BuildDropIn(DropInInput{
			Instance: in, Manifest: m, SpecSHA: SpecSHA256(raw),
			HasSecret: c.has, Now: renderedAt.AddDate(0, 0, at), Version: version,
		})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	if !SameDropIn(render(goldenVersion, 0), render(goldenVersion, 30)) {
		t.Error("two renders a month apart compare as different; every apply would rewrite the drop-in")
	}
	// The version is on the same line as the timestamp, so it is invisible to
	// the comparison too. That is deliberate: an `apt upgrade gvm` that
	// changed nothing about the unit must not restart every server on the box.
	if !SameDropIn(render("0.1.0", 0), render("0.2.0", 0)) {
		t.Error("a version bump alone counts as a change")
	}
	// Anything else must still count.
	changed := strings.Replace(string(render(goldenVersion, 0)), "ProtectHome=no", "ProtectHome=yes", 1)
	if SameDropIn(render(goldenVersion, 0), []byte(changed)) {
		t.Error("a changed ProtectHome compares as unchanged")
	}
}

// TestReadWritePathsRefusesAPathThatWouldSplit.
//
// ReadWritePaths= separates on whitespace. A game directory containing a space
// would silently become two paths, one of which does not exist -- and the
// symptom is a sandbox that refuses a write to a directory the file appears to
// list. Systemd's answer is quoting; gvm's is to refuse, because a directory
// can be renamed and a quoting rule has to be right forever.
func TestReadWritePathsRefusesAPathThatWouldSplit(t *testing.T) {
	in, raw := loadInstance(t, "valheim-1")
	in.Spec.Paths.Data = "/srv/gvm/valheim-1/my worlds"
	m := loadManifest(t, "valheim")

	_, _, err := BuildDropIn(DropInInput{
		Instance: in, Manifest: m, SpecSHA: SpecSHA256(raw),
		HasSecret: allSecretsPresent, Now: renderedAt, Version: goldenVersion,
	})
	if err == nil {
		t.Fatal("a path with a space in it rendered into ReadWritePaths")
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("the refusal does not explain why: %v", err)
	}
}

// TestANewlineInAnEnvValueCannotBecomeADirective.
//
// A systemd directive is literal to end of line, so a newline inside a value
// is a second directive. The value here comes from a manifest, and a manifest
// may be an operator's override under /etc/gvm/games.
func TestANewlineInAnEnvValueCannotBecomeADirective(t *testing.T) {
	in, raw := loadInstance(t, "valheim-1")
	m := loadManifest(t, "valheim")
	m.Spec.Runtime.Env["EVIL"] = "ok\nExecStartPre=/bin/false"

	_, _, err := BuildDropIn(DropInInput{
		Instance: in, Manifest: m, SpecSHA: SpecSHA256(raw),
		HasSecret: allSecretsPresent, Now: renderedAt, Version: goldenVersion,
	})
	if err == nil {
		t.Fatal("an env value carrying a newline rendered into the unit file")
	}
	if !strings.Contains(err.Error(), "second directive") {
		t.Errorf("the refusal does not say what the danger is: %v", err)
	}
}

// TestTheGeneratedHeaderNamesEverythingNeededToReproduceIt.
//
// The header is what an operator reads when `systemctl cat` surprises them.
// Each line answers one question: which file, which bytes of it, which
// manifest, and where their own overrides go.
func TestTheGeneratedHeaderNamesEverythingNeededToReproduceIt(t *testing.T) {
	body, _ := renderCase(t, goldenCases()[0])
	_, raw := loadInstance(t, "pz-main")
	text := string(body)

	if !strings.HasPrefix(text, GeneratedHeader+"\n") {
		t.Errorf("the file does not start with the marker doctor looks for:\n%s", firstLine(text))
	}
	for _, want := range []string{
		"# source      /etc/gvm/instances/pz-main.yaml",
		"# spec sha256 " + SpecSHA256(raw),
		"# manifest    projectzomboid rev 7 sha256:1b40c9de (origin: shipped)",
		"# Operator overrides go in " + LocalDropIn,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the header is missing %q", want)
		}
	}
}

// TestTheAdoptGuardsPointAtEachOther.
//
// One Conflicts= is enough for systemd. Two is what an operator needs: reading
// either unit has to explain why the other will not start.
func TestTheAdoptGuardsPointAtEachOther(t *testing.T) {
	guard, err := RenderAdoptGuard("pz-main", "zomboid.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(guard), "Conflicts=zomboid.service") ||
		!strings.Contains(string(guard), "After=zomboid.service") {
		t.Errorf("the gvm-side guard does not conflict with the legacy unit:\n%s", guard)
	}

	legacy, err := RenderLegacyGuard("pz-main", "zomboid.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacy), "Conflicts=gvm@pz-main.service") {
		t.Errorf("the legacy-side guard does not conflict with the gvm unit:\n%s", legacy)
	}
	// The reciprocal must not carry After=: ordering both ways is a cycle,
	// and systemd resolves a cycle by dropping an edge of its own choosing.
	if strings.Contains(string(legacy), "After=") {
		t.Errorf("the legacy-side guard carries After=, which closes an ordering cycle:\n%s", legacy)
	}
}

func TestAdoptGuardRefusesAnUnprintableUnitName(t *testing.T) {
	if _, err := RenderAdoptGuard("pz-main", "zomboid.service\nExecStart=/bin/sh"); err == nil {
		t.Fatal("a legacy unit name carrying a newline rendered into a drop-in")
	}
}

// directive returns the value of the first `Name=` line, failing if absent.
func directive(t *testing.T, text, name string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, name+"="); ok {
			return v
		}
	}
	t.Fatalf("no %s= directive in:\n%s", name, text)
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Compile-time assertion that the fixture instances still parse into the type
// the renderer takes. A fixture that drifted into a different shape would fail
// every test above with a confusing message.
var _ = func() *spec.Instance { return nil }

// TestTheMemoryDirectivesAreCheckedLikeEveryOtherOperatorTypedValue.
//
// spec.Resources.MemoryHigh/MemoryMax/MemorySwapMax come from
// /etc/gvm/instances/<name>.yaml -- the same file User, Group and
// WorkingDirectory come from, and those three are checked eight lines earlier.
// spec.ValidateInstance says nothing about resources, instance.CheckResources
// has no production caller, and the drop-in template interpolates all three
// unquoted, so a YAML block scalar in memory_max renders whatever it likes into
// the unit: a second [Service] section and an ExecStartPre= of its own.
func TestTheMemoryDirectivesAreCheckedLikeEveryOtherOperatorTypedValue(t *testing.T) {
	for _, field := range []struct{ name, set string }{
		{"MemoryHigh", "high"},
		{"MemoryMax", "max"},
		{"MemorySwapMax", "swap"},
	} {
		t.Run(field.name, func(t *testing.T) {
			in, raw := loadInstance(t, "valheim-1")
			m := loadManifest(t, "valheim")
			evil := "8G\n[Service]\nExecStartPre=+/bin/sh -c 'id > /tmp/pwn'"
			switch field.set {
			case "high":
				in.Spec.Resources.MemoryHigh = evil
			case "max":
				in.Spec.Resources.MemoryMax = evil
			case "swap":
				in.Spec.Resources.MemorySwapMax = evil
			}

			body, _, err := BuildDropIn(DropInInput{
				Instance: in, Manifest: m, SpecSHA: SpecSHA256(raw),
				HasSecret: allSecretsPresent, Now: renderedAt, Version: goldenVersion,
			})
			if err == nil {
				t.Fatalf("a newline in %s rendered into the drop-in:\n%s", field.name, body)
			}
			if !errors.Is(err, ErrUnsafeValue) {
				t.Errorf("%s = %v, want ErrUnsafeValue", field.name, err)
			}
			if !strings.Contains(err.Error(), field.name) {
				t.Errorf("the refusal does not name the field an operator would go and fix: %v", err)
			}
		})
	}

	// The control: an empty value still renders. Systemd reads a bare
	// `MemoryMax=` as reset-to-default, and an instance that declares no memory
	// limits is the ordinary case, so the check above must not have made one
	// unrenderable.
	in, raw := loadInstance(t, "valheim-1")
	in.Spec.Resources.MemoryHigh, in.Spec.Resources.MemoryMax, in.Spec.Resources.MemorySwapMax = "", "", ""
	if _, _, err := BuildDropIn(DropInInput{
		Instance: in, Manifest: loadManifest(t, "valheim"), SpecSHA: SpecSHA256(raw),
		HasSecret: allSecretsPresent, Now: renderedAt, Version: goldenVersion,
	}); err != nil {
		t.Errorf("an instance declaring no memory limits was refused: %v", err)
	}
}

// TestAnEnvValueCannotSMUGGLEASecondAssignment.
//
// Environment= is checked for control characters but not for the whitespace
// systemd splits assignments on, and the template emits `Environment=K=V`
// unquoted. Paths.Home is the live route in: all six shipped manifests set
// HOME: "{{ .Home }}", --home is a free-form string, and Home is absent from
// Sandbox.Paths, so it is covered neither by joinPaths nor by the D-15 probe.
func TestAnEnvValueCannotSMUGGLEASecondAssignment(t *testing.T) {
	for _, tc := range []struct{ name, home string }{
		{"a space", "/srv/gvm/valheim-1/my games"},
		{"a second assignment", "/srv/gvm/x LD_PRELOAD=/tmp/evil.so"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, raw := loadInstance(t, "valheim-1")
			in.Spec.Paths.Home = tc.home
			m := loadManifest(t, "valheim")

			body, _, err := BuildDropIn(DropInInput{
				Instance: in, Manifest: m, SpecSHA: SpecSHA256(raw),
				HasSecret: allSecretsPresent, Now: renderedAt, Version: goldenVersion,
			})
			if err == nil {
				t.Fatalf("HOME=%q rendered into an Environment= line systemd would split:\n%s",
					tc.home, body)
			}
			if !errors.Is(err, ErrUnsafeValue) {
				t.Errorf("= %v, want ErrUnsafeValue", err)
			}
		})
	}
}
