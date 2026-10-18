package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/secrets"
)

// secretHostFor is a test host with the secret store pointed at a temp
// directory and the audit logger wired to the memory sink, so a test can assert
// both the stored value and the audit record a `gvm secret` verb produced.
func secretHostFor(t *testing.T) (*testHost, *secrets.Store) {
	t.Helper()
	h := newTestHost(t)
	store := secrets.NewStore(filepath.Join(t.TempDir(), "secrets"))
	h.HostEnv.Secrets = store
	h.HostEnv.Audit = audit.New(h.Audit)
	return h, store
}

func mustReadSecret(t *testing.T, store *secrets.Store, ref secrets.Ref) secrets.Value {
	t.Helper()
	v, err := store.Read(ref)
	if err != nil {
		t.Fatalf("store.Read(%s): %v", ref, err)
	}
	return v
}

func TestSecretSetInstanceFromStdin(t *testing.T) {
	h, store := secretHostFor(t)
	const pw = "hunter2xyz"

	code, out, errb := runWith(t, h, pw+"\n", "secret", "set", "pz1", "join_password")
	if code != 0 {
		t.Fatalf("set exited %d: %s", code, errb)
	}
	// The value never appears in the command's own output.
	if strings.Contains(out, pw) {
		t.Errorf("set printed the plaintext value:\n%s", out)
	}
	if !strings.Contains(out, "join_password") || !strings.Contains(out, "source: operator") {
		t.Errorf("set output missing name/source:\n%s", out)
	}
	if !strings.Contains(out, "sudo gvm restart pz1") {
		t.Errorf("set output missing the restart effect line:\n%s", out)
	}

	ref := secrets.InstanceRef("pz1", "join_password")
	if got := mustReadSecret(t, store, ref); !secrets.Equal(got, secrets.New(pw)) {
		t.Error("stored value does not match what was piped in")
	}
	if src, ok, _ := store.Source(ref); !ok || src != secrets.SourceOperator {
		t.Errorf("source = %q (ok=%v), want operator", src, ok)
	}

	// The audit trail records the write, with scope/name/source and never the
	// value — a secret.write row must exist, and no record may carry the
	// plaintext in any field.
	assertSecretAudit(t, h, audit.SecretWrite, "join_password", pw)
}

func TestSecretSetGenerate(t *testing.T) {
	h, store := secretHostFor(t)
	code, out, errb := runWith(t, h, "", "secret", "set", "pz1", "rcon_password", "--generate", "--length", "24")
	if code != 0 {
		t.Fatalf("set --generate exited %d: %s", code, errb)
	}
	if !strings.Contains(out, "source: generated") {
		t.Errorf("a generated secret should say so:\n%s", out)
	}
	ref := secrets.InstanceRef("pz1", "rcon_password")
	if got := mustReadSecret(t, store, ref); got.Len() != 24 {
		t.Errorf("generated value is %d bytes, want 24", got.Len())
	}
	if src, _, _ := store.Source(ref); src != secrets.SourceGenerated {
		t.Errorf("source = %q, want generated", src)
	}
}

func TestSecretSetHostKeepsOnlyFileRules(t *testing.T) {
	h, store := secretHostFor(t)
	// A CurseForge-style key has a '$', which the instance ban refuses; as a
	// host secret it keeps only the file-format rules and is stored.
	const key = "$2a$10$abcdefghijklmnopqrstuv"
	code, _, errb := runWith(t, h, key+"\n", "secret", "set", "--host", "curseforge")
	if code != 0 {
		t.Fatalf("host set exited %d: %s", code, errb)
	}
	if got := mustReadSecret(t, store, secrets.HostRef("curseforge")); !secrets.Equal(got, secrets.New(key)) {
		t.Error("host key did not round-trip")
	}
}

func TestSecretSetInstanceRejectsBannedChar(t *testing.T) {
	h, store := secretHostFor(t)
	code, out, errb := runWith(t, h, "a;b\n", "secret", "set", "pz1", "join_password")
	if code != 5 {
		t.Fatalf("a banned-char value exited %d, want 5:\n%s", code, errb)
	}
	// Nothing was written.
	if has, _ := store.Has(secrets.InstanceRef("pz1", "join_password")); has {
		t.Error("a refused set still wrote a file")
	}
	// The refusal explains the character and never echoes the value.
	if strings.Contains(errb, "a;b") || strings.Contains(out, "a;b") {
		t.Errorf("the refusal echoed the value:\n%s%s", out, errb)
	}
	if !strings.Contains(errb, "semicolon") {
		t.Errorf("the refusal should name the offending character:\n%s", errb)
	}
}

func TestSecretSetRejectsEmptyValue(t *testing.T) {
	h, _ := secretHostFor(t)
	code, _, _ := runWith(t, h, "\n", "secret", "set", "pz1", "join_password")
	if code != 5 {
		t.Fatalf("an empty value exited %d, want 5", code)
	}
}

func TestSecretList(t *testing.T) {
	h, _ := secretHostFor(t)
	runWith(t, h, "aValidPass1\n", "secret", "set", "pz1", "join_password")
	runWith(t, h, "", "secret", "set", "pz1", "rcon_password", "--generate")

	code, out, errb := runWith(t, h, "", "secret", "list", "pz1")
	if code != 0 {
		t.Fatalf("list exited %d: %s", code, errb)
	}
	for _, want := range []string{"join_password", "rcon_password", "FINGERPRINT"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}

	code, jsonOut, _ := runWith(t, h, "", "secret", "list", "pz1", "--json")
	if code != 0 {
		t.Fatalf("list --json exited %d", code)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil {
		t.Fatalf("list --json is not JSON: %v\n%s", err, jsonOut)
	}
	if len(rows) != 2 {
		t.Errorf("list --json returned %d rows, want 2", len(rows))
	}
}

func TestSecretShowRefusesNonTerminal(t *testing.T) {
	h, _ := secretHostFor(t)
	runWith(t, h, "aValidPass1\n", "secret", "set", "pz1", "join_password")

	// In a test stdout is a buffer, not a terminal, so show must refuse rather
	// than write a plaintext credential into a pipe or a file.
	code, out, _ := runWith(t, h, "", "secret", "show", "pz1", "join_password")
	if code != 5 {
		t.Fatalf("show to a non-terminal exited %d, want 5", code)
	}
	if strings.Contains(out, "aValidPass1") {
		t.Errorf("show printed the value to a non-terminal:\n%s", out)
	}
}

func TestSecretShowPathPrintsTheFile(t *testing.T) {
	h, _ := secretHostFor(t)
	code, out, errb := runWith(t, h, "", "secret", "show-path", "pz1", "join_password")
	if code != 0 {
		t.Fatalf("show-path exited %d: %s", code, errb)
	}
	if !strings.Contains(out, "join_password") || !strings.Contains(out, "pz1") {
		t.Errorf("show-path did not name the file:\n%s", out)
	}
}

func TestSecretRotateReplacesValue(t *testing.T) {
	h, store := secretHostFor(t)
	runWith(t, h, "", "secret", "set", "pz1", "rcon_password", "--generate")
	ref := secrets.InstanceRef("pz1", "rcon_password")
	before := secrets.Fingerprint(mustReadSecret(t, store, ref))

	code, out, errb := runWith(t, h, "", "secret", "rotate", "pz1", "rcon_password")
	if code != 0 {
		t.Fatalf("rotate exited %d: %s", code, errb)
	}
	if !strings.Contains(out, "rotated") {
		t.Errorf("rotate output should say rotated:\n%s", out)
	}
	after := secrets.Fingerprint(mustReadSecret(t, store, ref))
	if before == after {
		t.Error("rotate did not change the value")
	}
	assertSecretAudit(t, h, audit.SecretRotate, "rcon_password", "")
}

func TestSecretRotateRefusesAdopted(t *testing.T) {
	h, store := secretHostFor(t)
	ref := secrets.InstanceRef("pz1", "rcon_password")
	if err := store.Write(ref, secrets.New("liveServerPass9")); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSource(ref, secrets.SourceAdopted); err != nil {
		t.Fatal(err)
	}

	code, _, errb := runWith(t, h, "", "secret", "rotate", "pz1", "rcon_password")
	if code != 5 {
		t.Fatalf("rotating an adopted secret exited %d, want 5", code)
	}
	if !strings.Contains(errb, "adopted") {
		t.Errorf("the refusal should cite the adopted origin:\n%s", errb)
	}
	if got := mustReadSecret(t, store, ref); !secrets.Equal(got, secrets.New("liveServerPass9")) {
		t.Error("the adopted value was changed despite the refusal")
	}
}

func TestSecretRotateRefusesHost(t *testing.T) {
	h, _ := secretHostFor(t)
	code, _, errb := runWith(t, h, "", "secret", "rotate", "--host", "curseforge")
	if code != 2 {
		t.Fatalf("rotate --host exited %d, want 2 (usage): %s", code, errb)
	}
}

// assertSecretAudit finds a terminal record for action/name and proves no
// record anywhere carries the plaintext.
func assertSecretAudit(t *testing.T, h *testHost, action audit.Action, name, plaintext string) {
	t.Helper()
	found := false
	for _, r := range h.Audit.Records() {
		raw, _ := json.Marshal(r)
		if plaintext != "" && strings.Contains(string(raw), plaintext) {
			t.Errorf("an audit record carries the plaintext value: %s", raw)
		}
		if r.Action == action && r.Outcome != audit.Started && r.Params["name"] == name {
			found = true
			if _, leaked := r.Params["value"]; leaked {
				t.Errorf("the %s record carries a value param", action)
			}
		}
	}
	if !found {
		t.Errorf("no %s audit record for %q", action, name)
	}
}
