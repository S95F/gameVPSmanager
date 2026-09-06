package reconcile

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// renderedIni is a config file rendered from a manifest, complete with the
// taint the renderer reports.
//
// It is built through spec.RenderFile rather than assembled by hand, because
// the property under test is that the DIFFER uses what the RENDERER already
// knows. A hand-built RenderedFile would let this test pass while the two
// disagreed about which keys are secret.
func renderedIni(t *testing.T) spec.RenderedFile {
	t.Helper()
	r := spec.NewRenderer(spec.RenderData{
		Name: "pz-main", Game: "projectzomboid",
		Install: "/opt/pzserver", Data: "/home/pzserver/Zomboid",
		Port: spec.PortValues{"game": 16261, "rcon": 27015},
		Var:  spec.VarValues{"public_name": "MadGaymers.club", "max_players": "8"},
		Secret: spec.SecretValues{
			"rcon_password": "hunter2-correct-horse",
			"join_password": "letmein-please-9",
		},
		Phase: spec.PhaseNormal,
	})
	f, err := r.RenderFile(spec.ConfigFile{
		Name:   "server_ini",
		Path:   "{{ .Data }}/Server/servertest.ini",
		Format: "ini",
		Keys: map[string]string{
			"PublicName":   "{{ .Var.public_name }}",
			"MaxPlayers":   "{{ .Var.max_players }}",
			"DefaultPort":  "{{ .Port.game }}",
			"RCONPort":     "{{ .Port.rcon }}",
			"RCONPassword": "{{ .Secret.rcon_password }}",
			"Password":     "{{ .Secret.join_password }}",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestARenderedSecretLineAppearsMaskedAndNeverInPlaintext.
//
// `gvm apply --diff` is printed to a terminal, copied into a bug report and
// pasted into a chat window. A password that survives to that point has been
// disclosed regardless of what the file's permissions were.
func TestARenderedSecretLineAppearsMaskedAndNeverInPlaintext(t *testing.T) {
	f := renderedIni(t)
	m := NewMask().AddFile(f)

	before := []byte("PublicName=old name\nRCONPassword=" + f.Keys["RCONPassword"] + "\nMaxPlayers=4\n")
	after := []byte("PublicName=" + f.Keys["PublicName"] + "\nRCONPassword=" + f.Keys["RCONPassword"] +
		"\nMaxPlayers=" + f.Keys["MaxPlayers"] + "\n")

	out := Unified("servertest.ini", before, after, m)

	if strings.Contains(out, f.Keys["RCONPassword"]) {
		t.Fatalf("the rendered RCON password is in the diff in plaintext:\n%s", out)
	}
	if !strings.Contains(out, "RCONPassword="+spec.Redacted) {
		t.Errorf("the masked line is not RCONPassword=%s:\n%s", spec.Redacted, out)
	}
	// The non-secret changes still have to be readable, or the diff is not
	// worth printing.
	if !strings.Contains(out, "MadGaymers.club") || !strings.Contains(out, "MaxPlayers=8") {
		t.Errorf("masking swallowed the changes an operator needs to see:\n%s", out)
	}
}

// TestARotatedSecretStillShowsAsAChangedLine.
//
// Masking happens on the way OUT rather than before the comparison. Masking
// first would make the two versions of the line compare equal and a credential
// rotation would leave no trace in the plan at all.
func TestARotatedSecretStillShowsAsAChangedLine(t *testing.T) {
	m := NewMask().AddKey("RCONPassword").AddValue("old-secret-value").AddValue("new-secret-value")

	out := Unified("servertest.ini",
		[]byte("A=1\nRCONPassword=old-secret-value\nB=2\n"),
		[]byte("A=1\nRCONPassword=new-secret-value\nB=2\n"), m)

	if strings.Contains(out, "secret-value") {
		t.Fatalf("a secret value survived into the diff:\n%s", out)
	}
	if !strings.Contains(out, "- RCONPassword="+spec.Redacted) ||
		!strings.Contains(out, "+ RCONPassword="+spec.Redacted) {
		t.Errorf("the rotation is invisible; the plan would show no change:\n%s", out)
	}
}

// TestTheValueScanCatchesASecretUnderAKeyNobodyDeclared.
//
// The key-based mask is the mechanism that is supposed to work. The value scan
// is the net under it, for the case a secret reached a file through a path
// this package did not render -- a game writing its own config, an operator
// pasting a password into a comment.
func TestTheValueScanCatchesASecretUnderAKeyNobodyDeclared(t *testing.T) {
	m := NewMask().AddValue("hunter2-correct-horse")
	out := Unified("notes.txt",
		[]byte("nothing here\n"),
		[]byte("# reminder: the rcon password is hunter2-correct-horse\n"), m)
	if strings.Contains(out, "hunter2-correct-horse") {
		t.Errorf("a known secret value appeared in a line with no key:\n%s", out)
	}
	if !strings.Contains(out, spec.Redacted) {
		t.Errorf("nothing was masked at all:\n%s", out)
	}
}

// TestTheValueScanIgnoresValuesTooShortToBeDistinctive.
//
// Masking every occurrence of a two-character value would produce a diff
// nobody can read, and an unreadable diff is one an operator approves without
// looking -- which is worse than a diff that shows too much.
func TestTheValueScanIgnoresValuesTooShortToBeDistinctive(t *testing.T) {
	m := NewMask().AddValue("ab")
	out := Unified("f", []byte("about\n"), []byte("abandon\n"), m)
	if strings.Contains(out, spec.Redacted) {
		t.Errorf("a %d-character value was used as a mask:\n%s", 2, out)
	}
}

// TestMaskingIsCaseInsensitiveOnTheKey.
//
// Ini dialects disagree about case, and a differ that masked `Password` while
// printing `password` would be worse than one that masked neither: it would
// look like it was working.
func TestMaskingIsCaseInsensitiveOnTheKey(t *testing.T) {
	m := NewMask().AddKey("Password")
	if got := m.Line("password = swordfish-in-the-clear"); strings.Contains(got, "swordfish") {
		t.Errorf("a lowercased key was not masked: %q", got)
	}
	if got := m.Line("PASSWORD: swordfish-in-the-clear"); strings.Contains(got, "swordfish") {
		t.Errorf("an uppercased yaml key was not masked: %q", got)
	}
}

// TestMaskingLeavesTheKeyAndItsSpacingAlone.
//
// A mask that reformatted the line would turn a one-line change into two, and
// every masked line in a diff would look like it had moved.
func TestMaskingLeavesTheKeyAndItsSpacingAlone(t *testing.T) {
	m := NewMask().AddKey("RCONPassword")
	for _, c := range []struct{ in, want string }{
		{"RCONPassword=x-secret", "RCONPassword=" + spec.Redacted},
		{"RCONPassword = x-secret", "RCONPassword = " + spec.Redacted},
		{"  RCONPassword:   x-secret", "  RCONPassword:   " + spec.Redacted},
	} {
		if got := m.Line(c.in); got != c.want {
			t.Errorf("Line(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestALineWithNoAssignmentIsLeftAlone. A section header is not a key.
func TestALineWithNoAssignmentIsLeftAlone(t *testing.T) {
	m := NewMask().AddKey("Password")
	for _, l := range []string{"[Service]", "# a comment about Password", ""} {
		if got := m.Line(l); got != l {
			t.Errorf("Line(%q) = %q, want it untouched", l, got)
		}
	}
}

// TestANilMaskPrintsEverything.
//
// The default has to be readable, not redacted: a file with no secrets in it
// diffed through a nil Mask is the common case, and a solid block of dots
// would be the wrong answer for it.
func TestANilMaskPrintsEverything(t *testing.T) {
	var m *Mask
	out := Unified("f", []byte("A=1\n"), []byte("A=2\n"), m)
	if !strings.Contains(out, "A=2") {
		t.Errorf("a nil mask redacted an ordinary line:\n%s", out)
	}
}

// TestIdenticalFilesProduceNoDiff, so `= unchanged` prints nothing under it.
func TestIdenticalFilesProduceNoDiff(t *testing.T) {
	b := []byte("A=1\nB=2\n")
	if out := Unified("f", b, b, NewMask()); out != "" {
		t.Errorf("identical files produced a diff:\n%s", out)
	}
}

// TestTheDiffShowsContextAroundAChangeInTheMiddle.
//
// Three lines either side is diff(1)'s default and is what lets a reviewer see
// which section of an ini file a change landed in.
func TestTheDiffShowsContextAroundAChangeInTheMiddle(t *testing.T) {
	old := []byte("a\nb\nc\nd\nOLD\ne\nf\ng\n")
	next := []byte("a\nb\nc\nd\nNEW\ne\nf\ng\n")
	out := Unified("f", old, next, NewMask())

	if !strings.Contains(out, "- OLD") || !strings.Contains(out, "+ NEW") {
		t.Fatalf("the change is not in the diff:\n%s", out)
	}
	for _, ctx := range []string{"  b", "  c", "  d", "  e", "  f", "  g"} {
		if !strings.Contains(out, ctx+"\n") {
			t.Errorf("context line %q is missing:\n%s", ctx, out)
		}
	}
	// The first line is four away from the change and must NOT be printed, or
	// the "unified" diff is a whole-file dump.
	if strings.Contains(out, "\n  a\n") {
		t.Errorf("context extends further than three lines:\n%s", out)
	}
}

// TestAHugeDiffDegradesToWholeBlockReplacement rather than allocating.
//
// A diff is something an HTTP handler can trigger, and an unbounded quadratic
// allocation reached from one is a denial of service with extra steps.
func TestAHugeDiffDegradesToWholeBlockReplacement(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 3000; i++ {
		a.WriteString("left ")
		a.WriteString(itoa(i))
		a.WriteString("\n")
		b.WriteString("right ")
		b.WriteString(itoa(i))
		b.WriteString("\n")
	}
	out := Unified("big", []byte(a.String()), []byte(b.String()), NewMask())
	if !strings.Contains(out, "- left 0") || !strings.Contains(out, "+ right 0") {
		t.Errorf("the fallback did not report the block as replaced:\n%s", truncate(out, 400))
	}
}

// TestTheDiffNamesBothSidesHonestly. "on disk" and "would be written" rather
// than a/b, because the reader is an operator and not a patch tool.
func TestTheDiffNamesBothSidesHonestly(t *testing.T) {
	out := Unified("/etc/systemd/system/gvm@pz-main.service.d/10-gvm.conf",
		[]byte("A=1\n"), []byte("A=2\n"), NewMask())
	if !strings.HasPrefix(out, "--- /etc/systemd/system/gvm@pz-main.service.d/10-gvm.conf (on disk)\n") {
		t.Errorf("the diff header does not name the file on disk:\n%s", out)
	}
	if !strings.Contains(out, "(would be written)") {
		t.Errorf("the diff header does not say the new side is a proposal:\n%s", out)
	}
}
