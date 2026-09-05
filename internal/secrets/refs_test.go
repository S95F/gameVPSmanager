package secrets

import (
	"errors"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/instance"
)

func TestPathIsSection5Verbatim(t *testing.T) {
	cases := []struct {
		ref  Ref
		want string
	}{
		{InstanceRef("pz-main", "rcon"), "/etc/gvm/secrets/instances/pz-main/rcon"},
		{InstanceRef("pz-main", "admin"), "/etc/gvm/secrets/instances/pz-main/admin"},
		{InstanceRef("ck-1", "join"), "/etc/gvm/secrets/instances/ck-1/join"},
		{HostRef("curseforge"), "/etc/gvm/secrets/host/curseforge"},
		{HostRef("sftp_ed25519"), "/etc/gvm/secrets/host/sftp_ed25519"},
	}
	for _, tc := range cases {
		if got := tc.ref.Path(); got != tc.want {
			t.Errorf("%s.Path() = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestInstancePathAgreesWithTheInstancePackage(t *testing.T) {
	// Section 5 is transcribed once, in internal/instance. This package
	// derives the same strings a second way -- from a scope and a key rather
	// than from a name -- and the two must not be able to drift, because the
	// drop-in generator uses one and the store uses the other, and the
	// failure mode is a LoadCredential= pointing at a file nothing writes.
	for _, name := range []string{"pz-main", "ck-1", "a1"} {
		p := instance.PathsFor(name)
		for _, key := range []string{"rcon", "admin", "join"} {
			want := p.Secret(key)
			if got := InstanceRef(name, key).Path(); got != want {
				t.Errorf("InstanceRef(%q,%q).Path() = %q, instance.PathsFor(%q).Secret(%q) = %q",
					name, key, got, name, key, want)
			}
		}
	}
	if HostRef("modrinth").Path() != instance.HostSecretsDir+"/modrinth" {
		t.Errorf("host path %q does not sit under instance.HostSecretsDir %q",
			HostRef("modrinth").Path(), instance.HostSecretsDir)
	}
}

func TestHostKeyCannotCollideWithAnInstance(t *testing.T) {
	// scope_key is one column holding either an instance name or "host"
	// (section 14.4), and one directory level holding either instances/ or
	// host/. That is only safe because D-45 reserves the word.
	if err := instance.ValidateName(HostKey); err == nil {
		t.Fatalf("instance.ValidateName(%q) now succeeds; an instance by that name would "+
			"share a secret_refs key with every host secret on the box", HostKey)
	}
}

func TestNameRuleAcceptsEverySecretTheDocumentNames(t *testing.T) {
	// Section 5's layout and the secret_refs.name comment in 14.4, together.
	for _, n := range []string{"rcon", "admin", "join", "curseforge", "modrinth", "sftp_ed25519", "age_recipient"} {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v; the document says this file exists", n, err)
		}
	}
}

func TestValidateRefusesEveryWayOutOfTheDirectory(t *testing.T) {
	// A Name and a Key are both path segments under a root-owned 0700
	// directory that gvm writes 0600 files into. Anything that escapes it
	// writes a root-owned file somewhere else on the box.
	escapes := []string{
		"..", ".", "../rcon", "rcon/../../etc/shadow", "a/b", `a\b`,
		"/etc/shadow", "rcon/", "./rcon", "..\\..\\hosts",
		"rcon\x00.txt", "rcon\n", " rcon", "rcon ", "-rcon", ".rcon",
		"RCON", "rcon.bak", "rcon.tmp", "", "r", "_rcon", "rcon_",
		strings.Repeat("r", MaxNameLen+1),
	}
	for _, bad := range escapes {
		t.Run("name "+bad, func(t *testing.T) {
			if err := ValidateName(bad); err == nil {
				t.Fatalf("ValidateName(%q) accepted it", bad)
			} else if !errors.Is(err, ErrRef) {
				t.Fatalf("ValidateName(%q) = %v, which does not wrap ErrRef", bad, err)
			}
			if err := InstanceRef("pz-main", bad).Validate(); err == nil {
				t.Errorf("Ref{Name:%q}.Validate() accepted it", bad)
			}
			if err := (Ref{Scope: ScopeInstance, Key: bad, Name: "rcon"}).Validate(); err == nil {
				t.Errorf("Ref{Key:%q}.Validate() accepted it", bad)
			}
		})
	}
}

func TestPathPanicsOnARefNobodyValidated(t *testing.T) {
	// Deriving a path from an unchecked name is how a traversal becomes a
	// file, so the derivation refuses rather than returning something wrong.
	// Every exported entry point in the store validates first, which is why
	// no caller can reach this.
	defer func() {
		if recover() == nil {
			t.Fatal("Ref{Name: \"../../shadow\"}.Path() returned instead of panicking")
		}
	}()
	_ = InstanceRef("pz-main", "../../shadow").Path()
}

func TestScopeKeyRulesAreShapedLikeTheDatabase(t *testing.T) {
	cases := []struct {
		name  string
		scope Scope
		key   string
		ok    bool
	}{
		{"instance", ScopeInstance, "pz-main", true},
		{"host", ScopeHost, HostKey, true},
		{"host with an instance key", ScopeHost, "pz-main", false},
		{"host with no key", ScopeHost, "", false},
		{"instance with the host key", ScopeInstance, HostKey, false},
		{"no scope", "", "pz-main", false},
		{"invented scope", "global", "pz-main", false},
		{"uppercase scope", "Instance", "pz-main", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScopeKey(tc.scope, tc.key)
			if tc.ok != (err == nil) {
				t.Fatalf("ValidateScopeKey(%q,%q) = %v, want ok=%v", tc.scope, tc.key, err, tc.ok)
			}
		})
	}
}

func TestStringAndParseRefRoundTrip(t *testing.T) {
	for _, ref := range []Ref{InstanceRef("pz-main", "rcon"), HostRef("curseforge")} {
		got, err := ParseRef(ref.String())
		if err != nil {
			t.Fatalf("ParseRef(%q) = %v", ref.String(), err)
		}
		if got != ref {
			t.Errorf("ParseRef(%q) = %+v, want %+v", ref.String(), got, ref)
		}
	}
	for _, bad := range []string{"", "rcon", "instance/pz-main", "instance/pz-main/rcon/extra", "instance//rcon", "../../x/y"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted it", bad)
		}
	}
}

func TestCredentialIDIsTheFilename(t *testing.T) {
	// The runner reads $CREDENTIALS_DIRECTORY/<id> and never learns the
	// layout, so the id and the filename being one string is the whole
	// mechanism (section 14.7).
	ref := InstanceRef("pz-main", "rcon")
	if ref.CredentialID() != "rcon" || !strings.HasSuffix(ref.Path(), "/"+ref.CredentialID()) {
		t.Errorf("CredentialID %q does not match the last segment of %q", ref.CredentialID(), ref.Path())
	}
}

// secretRefRE finds a `.Secret.<name>` reference in a manifest's raw bytes.
// Reading the YAML rather than the parsed manifest is deliberate: the
// reference can appear in args, env, control.password, verbs and config_files
// keys (section 8.6's secretSurfaces), and a walker that visited only the
// fields this test's author remembered would silently stop covering the rest.
var secretRefRE = regexp.MustCompile(`\.Secret\.([A-Za-z0-9_]+)`)

func TestEverySecretTheShippedManifestsNameHasAStoreFile(t *testing.T) {
	found := map[string][]string{} // store name -> template spellings
	err := fs.WalkDir(games.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := games.FS.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range secretRefRE.FindAllStringSubmatch(string(b), -1) {
			tmpl := m[1]
			name, err := FromTemplate(tmpl)
			if err != nil {
				t.Errorf("%s references {{ .Secret.%s }}, which has no legal store name: %v", p, tmpl, err)
				continue
			}
			if !contains(found[name], tmpl) {
				found[name] = append(found[name], tmpl)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded manifests: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no .Secret.* reference found in any shipped manifest; the regex or the manifests changed")
	}

	// The names section 5 and 14.4 promise are on disk. Anything else a
	// manifest names is legal but new, and worth seeing in the failure
	// output of whoever adds it.
	documented := map[string]bool{"rcon": true, "admin": true, "join": true}
	var undocumented []string
	for name, tmpls := range found {
		if len(tmpls) > 1 {
			sort.Strings(tmpls)
			t.Errorf("%v all map to the single store file %q; two secrets sharing a file means "+
				"writing one silently overwrites the other", tmpls, name)
		}
		if !documented[name] {
			undocumented = append(undocumented, name)
		}
	}
	sort.Strings(undocumented)
	t.Logf("shipped manifests name %d secrets; beyond rcon/admin/join they use %v", len(found), undocumented)
}

func TestFromTemplateStripsOnlyThePasswordSuffix(t *testing.T) {
	cases := map[string]string{
		"rcon_password":   "rcon",
		"admin_password":  "admin",
		"join_password":   "join",
		"telnet_password": "telnet",
		"curseforge":      "curseforge",   // a host secret keeps its spelling
		"sftp_ed25519":    "sftp_ed25519", // and an underscore is not a suffix
	}
	for in, want := range cases {
		got, err := FromTemplate(in)
		if err != nil || got != want {
			t.Errorf("FromTemplate(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"_password", "", "../x", "Rcon_password", "a"} {
		if got, err := FromTemplate(bad); err == nil {
			t.Errorf("FromTemplate(%q) = %q, want an error", bad, got)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
