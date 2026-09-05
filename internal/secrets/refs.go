package secrets

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
)

// ErrRef is the sentinel every addressing failure wraps. It is separate from
// the I/O errors the store returns because the two want different handling: a
// bad ref is a caller bug or a corrupt database row, and no retry fixes it.
var ErrRef = errors.New("secrets: invalid reference")

// Scope is which of the two namespaces a secret lives in.
type Scope string

const (
	// ScopeInstance is one server's own credentials: rcon, admin, join.
	ScopeInstance Scope = "instance"
	// ScopeHost is the box-wide ones: curseforge, modrinth, sftp_ed25519,
	// age_recipient.
	ScopeHost Scope = "host"
)

// HostKey is the Key every host-scoped ref carries.
//
// It is safe as a sentinel for a reason worth writing down: "host" is a
// reserved instance name (D-45), so no instance can ever own this key and the
// two namespaces cannot collide even though they share one column in
// secret_refs and one directory tree on disk.
const HostKey = "host"

// nameRE constrains a secret's name, which is simultaneously three things:
// a path segment under /etc/gvm/secrets, a systemd credential id in a
// `LoadCredential=<name>:<path>` line (section 7.4), and the `name` column of
// secret_refs. The intersection of what those three accept is narrow.
//
//	[a-z0-9]      no leading '_' or digit-free oddity; a leading dot would be
//	              a hidden file, and a leading '-' is a flag to half the tools
//	              that will ever read this directory.
//	[a-z0-9_]     lowercase and '_' only. '_' is present because the host
//	              secrets are spelled sftp_ed25519 and age_recipient; '/' and
//	              '.' are absent because this string is joined into a path and
//	              "../../shadow" must not be expressible at all.
//	{0,30}        2..32 characters, comfortably inside systemd's 255-byte
//	              credential id limit and readable in a unit file.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,30}[a-z0-9]$`)

// MaxNameLen is the ceiling nameRE encodes.
const MaxNameLen = 32

// Ref addresses one secret. It is exactly the primary key of secret_refs
// (scope, scope_key, name), so a row and a file are the same address written
// two ways, and neither can drift into naming something the other cannot.
//
// A Ref holds no value and is safe to log, put in an audit params blob, or
// print in a diagnostic. That is the point of having one: code that needs to
// say WHICH secret failed never has to reach for the secret to say it.
type Ref struct {
	Scope Scope
	Key   string // instance name, or HostKey
	Name  string // rcon | admin | join | curseforge | modrinth | ...
}

// InstanceRef addresses one instance's secret.
func InstanceRef(inst, name string) Ref {
	return Ref{Scope: ScopeInstance, Key: inst, Name: name}
}

// HostRef addresses a box-wide secret.
func HostRef(name string) Ref { return Ref{Scope: ScopeHost, Key: HostKey, Name: name} }

// Validate reports why a ref cannot address a file, naming the clause it
// broke. Every store operation calls it first, so a ref that came out of a
// database row or off a command line is checked before it reaches a path.
func (r Ref) Validate() error {
	if err := ValidateScopeKey(r.Scope, r.Key); err != nil {
		return err
	}
	return ValidateName(r.Name)
}

// ValidateScopeKey checks the directory half of an address. It is separate
// from Validate because listing a scope is a real operation that has no name
// to check, and because the two callers must not disagree about what a legal
// key is.
func ValidateScopeKey(scope Scope, key string) error {
	switch scope {
	case ScopeInstance:
		if err := instance.ValidateName(key); err != nil {
			// The instance rules are D-45's and are not restated here: a
			// second copy would drift, and the first thing it would allow is
			// a name that useradd rejects.
			return fmt.Errorf("%w: instance %w", ErrRef, err)
		}
	case ScopeHost:
		if key != HostKey {
			return fmt.Errorf("%w: host-scoped secrets carry key %q, got %q", ErrRef, HostKey, key)
		}
	case "":
		return fmt.Errorf("%w: scope is required (%q or %q)", ErrRef, ScopeInstance, ScopeHost)
	default:
		return fmt.Errorf("%w: unknown scope %q, want %q or %q", ErrRef, scope, ScopeInstance, ScopeHost)
	}
	return nil
}

// ValidateName applies nameRE, with the reason for each refusal.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: a secret name is required", ErrRef)
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("%w: secret name %q is %d characters; the ceiling is %d",
			ErrRef, name, len(name), MaxNameLen)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: secret name %q must be 2-%d characters of [a-z0-9_], "+
			"starting and ending with [a-z0-9]; it is a path segment, a systemd "+
			"credential id and a database key at once", ErrRef, name, MaxNameLen)
	}
	return nil
}

// dirRel is the directory one scope's secrets live in, relative to the secrets
// root, in slash form.
//
// It panics on an invalid scope or key rather than returning an error,
// matching instance.MustValidName's discipline: every exported entry point
// validates first, and a path derived from an unchecked key is a root-owned
// 0600 file written wherever the key pointed.
func dirRel(scope Scope, key string) string {
	if err := ValidateScopeKey(scope, key); err != nil {
		panic(err)
	}
	if scope == ScopeHost {
		return "host"
	}
	return path.Join("instances", key)
}

// rel is the ref's path relative to the secrets root, in slash form.
func (r Ref) rel() string {
	if err := r.Validate(); err != nil {
		panic(err)
	}
	return path.Join(dirRel(r.Scope, r.Key), r.Name)
}

// Path is the file this ref names on a real box: the exact string a
// `LoadCredential=` line carries (section 7.4).
//
// For an instance ref this is the same path instance.PathsFor(key).Secret(name)
// builds, and refs_test.go asserts the two agree. Section 5 is transcribed
// once, in internal/instance, and this is a second view of it rather than a
// second copy.
func (r Ref) Path() string { return path.Join(instance.SecretsRoot, r.rel()) }

// CredentialID is the id half of `LoadCredential=<id>:<path>`. It is the name
// verbatim: the runner reads $CREDENTIALS_DIRECTORY/<id>, so the id and the
// filename being the same string is what lets the runner find a credential
// without being told the layout.
func (r Ref) CredentialID() string { return r.Name }

// String is a stable, value-free address for logs, audit params and
// diagnostics: "instance/pz-main/rcon", "host/curseforge".
func (r Ref) String() string {
	return string(r.Scope) + "/" + r.Key + "/" + r.Name
}

// ParseRef is String's inverse, for a command line and for reconstructing a
// ref from an audit record. It validates, so a parsed ref is usable.
func ParseRef(s string) (Ref, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return Ref{}, fmt.Errorf("%w: %q is not <scope>/<key>/<name>", ErrRef, s)
	}
	r := Ref{Scope: Scope(parts[0]), Key: parts[1], Name: parts[2]}
	if err := r.Validate(); err != nil {
		return Ref{}, err
	}
	return r, nil
}

// templateSuffix is the difference between the two spellings of one secret.
const templateSuffix = "_password"

// FromTemplate maps a manifest's `.Secret.<name>` spelling to the store's.
//
// A manifest says `{{ .Secret.rcon_password }}`; the file is
// /etc/gvm/secrets/instances/pz-main/rcon and the systemd credential id is
// `rcon` (section 5, section 7.4, and the secret_refs.name comment in 14.4 all
// agree on the short name). The suffix is the whole difference, and something
// has to know that: the drop-in generator has to emit a LoadCredential line
// per secret a manifest references, and the runner has to find
// $CREDENTIALS_DIRECTORY/rcon when the template said rcon_password.
//
// There is deliberately no inverse. `curseforge` and `rcon` are both valid
// store names and only one of them gains the suffix in a manifest, so a
// function mapping a store name back to a template name would have to guess.
func FromTemplate(tmpl string) (string, error) {
	name := strings.TrimSuffix(tmpl, templateSuffix)
	if name == "" {
		return "", fmt.Errorf("%w: %q is the suffix and nothing else", ErrRef, tmpl)
	}
	if err := ValidateName(name); err != nil {
		return "", fmt.Errorf("%w (from template name %q)", err, tmpl)
	}
	return name, nil
}
