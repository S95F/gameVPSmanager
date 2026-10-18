package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The modes section 5 gives this tree. The directory is untraversable to the
// unprivileged web tier, which is what makes /etc/gvm itself safe at 0750
// root:gvm: the web tier can read a spec in order to render it and cannot
// reach a credential (D-02).
const (
	FileMode fs.FileMode = 0o600
	DirMode  fs.FileMode = 0o700
	// MasterKeyMode is stricter than a game secret's: the key is read once at
	// gvmd start and never written by anything but `gvm secret rotate-key`,
	// so it has no reason to be writable at all. `gvm doctor` HOST-006
	// reports any deviation.
	MasterKeyMode fs.FileMode = 0o400
)

// MasterKeyPath is where the TOTP column-encryption key lives — beside
// /etc/gvm/secrets rather than inside it, because it is not a game secret and
// never reaches an instance: gvm-web carries no LoadCredential= for it and no
// SELECT on the ciphertext it protects (D-27).
const MasterKeyPath = instance.EtcRoot + "/master.key"

// MasterKeySize is the key length in bytes. AES-256 for the TOTP column, HKDF
// fanning it into per-purpose subkeys (section 14.7), so 32 is the size every
// consumer wants and there is no reason to make it negotiable.
const MasterKeySize = 32

// The store's failure modes, as sentinels, because each one has a different
// correct response and a string match on the message is not a contract.
var (
	// ErrEmpty is a secret file that exists and holds nothing. It is separate
	// from "not found" because the two mean opposite things to an operator:
	// absent is a secret that was never set, empty is one that a failed
	// rotation or a truncating redirect destroyed.
	ErrEmpty = errors.New("secrets: file is present but empty")
	// ErrMultiline is a secret file holding more than one line. A game gets
	// this value on a command line and in an ini `key=value`, neither of
	// which can carry a newline, so reading one out is the last honest moment
	// to say so.
	ErrMultiline = errors.New("secrets: file holds more than one line")
	// ErrNotRegular is a secret path that is a directory, a symlink, a device
	// or a socket.
	ErrNotRegular = errors.New("secrets: not a regular file")
	// ErrInsecureMode is a master key any account but root can read.
	ErrInsecureMode = errors.New("secrets: file is group- or world-accessible")
	// ErrBannedChar is an instance secret holding a character gvm cannot carry
	// into a game's argv or config files (rule 10). It is a write-time refusal,
	// not a read-time one: the value never reaches the game altered, so the
	// operator rotates it to one gvm can carry.
	ErrBannedChar = errors.New("secrets: value contains a character gvm cannot carry into a game")
)

// Store is the /etc/gvm/secrets tree (D-02).
//
// Root is a field rather than a constant only so tests can point it at a
// temporary directory; production builds one with DefaultStore and every path
// it derives is section 5's, verbatim.
type Store struct {
	root string
}

// NewStore roots a store at dir. An empty dir means the real one.
func NewStore(dir string) *Store {
	if dir == "" {
		dir = filepath.FromSlash(instance.SecretsRoot)
	}
	return &Store{root: dir}
}

// DefaultStore is the store of a real box.
func DefaultStore() *Store { return NewStore("") }

// Root is the directory this store writes under.
func (s *Store) Root() string { return s.root }

// Path is the file a ref names in this store.
func (s *Store) Path(ref Ref) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	return s.join(ref.rel()), nil
}

func (s *Store) join(rel string) string {
	return filepath.Join(s.root, filepath.FromSlash(rel))
}

// ValidateValue reports why a value cannot be stored.
//
// The rules are about the file format, not about strength: one file holds
// exactly one value, so a newline or a NUL in it makes the file ambiguous, and
// an empty value is indistinguishable from an unset one — which matters
// because `when: {secret_set: join_password}` would still see the file and
// render an empty `-password` argument, the failure mode that guard exists to
// prevent.
func ValidateValue(v Value) error {
	p := v.plain()
	if p == "" {
		return fmt.Errorf("%w: refusing to write an empty secret", ErrEmpty)
	}
	if i := strings.IndexAny(p, "\n\r\x00"); i >= 0 {
		// The offset, never the byte and never the value: naming the
		// character would print a fragment of the secret in a log.
		return fmt.Errorf("%w: byte %d is a newline, a carriage return or a NUL", ErrMultiline, i)
	}
	return nil
}

// instanceBanReason maps a character gvm cannot carry into a game's argv or
// config files to WHY, so the refusal tells the operator what to avoid without
// ever printing the value. These are the characters that would be cut, escaped
// or re-interpreted between the store and the game.
var instanceBanReason = map[byte]string{
	'\'': "a single quote, which terminates a quoted value in a game config",
	'"':  "a double quote, which terminates a quoted value in a game config",
	'\\': "a backslash, a config and shell escape character",
	'$':  "a dollar sign, a shell metacharacter",
	'`':  "a backtick, a shell metacharacter",
	';':  "a semicolon, a list separator (Project Zomboid cuts its mod list there)",
	'=':  "an equals sign, a key/value separator",
	' ':  "a space, which splits a value in argv and many config formats",
	'\t': "a tab, which splits a value in argv and many config formats",
	'\v': "a vertical tab, whitespace that splits a value in some config formats",
	'\f': "a form feed, whitespace that splits a value in some config formats",
}

// ValidateInstanceValue is ValidateValue plus the rule-10 ban on characters a
// secret cannot carry into a game's argv or config files. A secret that reaches
// a game (every one a manifest's secrets: block declares) must pass this;
// host-scope secrets — an API key sent only in an HTTP header — keep only
// ValidateValue's file-format rules, which is what rule 10 already allows.
//
// It reports the offending byte's OFFSET and its reason, never the byte itself
// and never the value, so a refusal is legible in a log without leaking a
// fragment of the password.
func ValidateInstanceValue(v Value) error {
	if err := ValidateValue(v); err != nil {
		return err
	}
	p := v.plain()
	for i := 0; i < len(p); i++ {
		if reason, banned := instanceBanReason[p[i]]; banned {
			return fmt.Errorf("%w: byte %d is %s — rotate it to a password without that character",
				ErrBannedChar, i, reason)
		}
	}
	return nil
}

// Write replaces a secret, atomically and at mode 0600.
//
// No .bak: spec.WriteFileAtomicNoBackup exists for this caller, because a
// rotation that leaves the previous credential in <path>.bak has not rotated
// anything.
func (s *Store) Write(ref Ref, v Value) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := ValidateValue(v); err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	if err := s.mkdirAll(dirRel(ref.Scope, ref.Key)); err != nil {
		return err
	}
	p := s.join(ref.rel())
	if err := spec.WriteFileAtomicNoBackup(p, []byte(v.plain()), FileMode); err != nil {
		return fmt.Errorf("secrets: write %s: %w", ref, err)
	}
	return chownRoot(p)
}

// Read returns the stored value.
//
// One trailing end-of-line is trimmed, because the operator who sets a secret
// by hand types `echo hunter2 > join` or edits the file in vi, and both leave
// a newline that the game would otherwise receive as part of the password —
// producing a server nobody can join and a credential that looks correct in
// every tool that prints it.
func (s *Store) Read(ref Ref) (Value, error) {
	p, err := s.Path(ref)
	if err != nil {
		return Value{}, err
	}
	if _, err := statRegular(p); err != nil {
		return Value{}, fmt.Errorf("secrets: read %s: %w", ref, err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Value{}, fmt.Errorf("secrets: read %s: %w", ref, err)
	}
	text := trimOneEOL(string(b))
	if text == "" {
		return Value{}, fmt.Errorf("secrets: read %s: %w", ref, ErrEmpty)
	}
	if strings.ContainsAny(text, "\n\r\x00") {
		return Value{}, fmt.Errorf("secrets: read %s: %w", ref, ErrMultiline)
	}
	return New(text), nil
}

// trimOneEOL removes at most one trailing line ending, CRLF included.
//
// At most one, and only a real line ending. Two TrimSuffix calls -- "\n" then
// "\r" -- would look equivalent and would also silently eat a LONE trailing
// carriage return, which is not a line ending on any platform gvm runs on and
// is a signal that the file was mangled in transit. Trimming it would hand the
// game a value the operator never typed; leaving it makes Read report the file
// as multiline, which is what the operator needs to hear.
func trimOneEOL(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	return strings.TrimSuffix(s, "\n")
}

// Has reports whether a secret file exists, without reading it.
func (s *Store) Has(ref Ref) (bool, error) {
	p, err := s.Path(ref)
	if err != nil {
		return false, err
	}
	switch _, err := statRegular(p); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("secrets: stat %s: %w", ref, err)
	}
}

// Delete removes a secret, and is idempotent: destroying an instance and
// rotating a credential both run it against a set that may already be partly
// gone, and an error there would stop a cleanup halfway.
//
// The bytes are not overwritten first. On a journalling filesystem, and on
// every copy-on-write one, writing zeroes over a file does not reach the
// blocks that held it, so shredding here would be a comforting lie in a
// package whose whole job is to not tell those.
func (s *Store) Delete(ref Ref) error {
	p, err := s.Path(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("secrets: delete %s: %w", ref, err)
	}
	return nil
}

// Info is what `gvm doctor` needs to judge a secret file without opening it.
type Info struct {
	Ref  Ref
	Path string
	// Mode is the permission bits, valid only when ModeChecked is true.
	Mode fs.FileMode
	// ModeChecked is false on a platform that cannot report unix permission
	// bits. Reporting that the check did not happen is the point: a doctor
	// that prints "ok" for a check it could not run is worse than one that
	// prints nothing.
	ModeChecked bool
	Size        int64
}

// Stat describes a stored secret.
func (s *Store) Stat(ref Ref) (Info, error) {
	p, err := s.Path(ref)
	if err != nil {
		return Info{}, err
	}
	fi, err := statRegular(p)
	if err != nil {
		return Info{}, fmt.Errorf("secrets: stat %s: %w", ref, err)
	}
	perm, ok := permOf(fi)
	return Info{Ref: ref, Path: p, Mode: perm, ModeChecked: ok, Size: fi.Size()}, nil
}

// List returns every secret in one scope, sorted by name. A missing directory
// is an empty list, not an error: an instance with no secrets yet and an
// instance whose secrets were all deleted are the same state.
//
// Entries that are not secrets are skipped rather than reported, and the
// leftovers of an interrupted write are the reason. spec's atomic writer
// stages into `.<name>.tmp*` in the same directory and deliberately does not
// clean up after a crash; a lister that took every directory entry at face
// value would hand a caller a ref named `.rcon.tmp2841582`, and the caller
// would faithfully write it into secret_refs.
func (s *Store) List(scope Scope, key string) ([]Ref, error) {
	if err := ValidateScopeKey(scope, key); err != nil {
		return nil, err
	}
	dir := s.join(dirRel(scope, key))
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: list %s/%s: %w", scope, key, err)
	}
	var out []Ref
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		if ValidateName(e.Name()) != nil {
			continue
		}
		out = append(out, Ref{Scope: scope, Key: key, Name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// mkdirAll creates the store's directories at 0700 root:root.
//
// It creates the ROOT but never the root's parent, and that restriction is
// load-bearing. /etc/gvm is 0750 root:gvm so the web tier can read specs
// (section 5.1); an os.MkdirAll that happened to create it on the way to
// /etc/gvm/secrets would create it at 0700 root:root and the web tier would
// lose every instance page on the box, with nothing to point at.
func (s *Store) mkdirAll(relDir string) error {
	if err := ensureDir(s.root); err != nil {
		return err
	}
	cur := s.root
	for _, seg := range strings.Split(relDir, "/") {
		cur = filepath.Join(cur, seg)
		if err := ensureDir(cur); err != nil {
			return err
		}
	}
	return nil
}

func ensureDir(dir string) error {
	err := os.Mkdir(dir, DirMode)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		// Already there, possibly with the wrong mode; fall through and fix
		// it. A 0755 /etc/gvm/secrets inherited from a hand-made directory is
		// exactly the state this package exists to not tolerate.
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("secrets: %s: parent directory does not exist; "+
			"gvm does not create it, because creating /etc/gvm on the way past would "+
			"give it this directory's 0700 root:root instead of its own 0750 root:gvm "+
			"and the web tier would lose every spec it renders", dir)
	default:
		return fmt.Errorf("secrets: mkdir %s: %w", dir, err)
	}
	// The mkdir is not enough on its own: the ErrExist path above lands here
	// precisely to repair a directory somebody else created, and os.Mkdir's
	// mode is masked by the process umask, so one that masks owner bits (0177,
	// say) would leave even a fresh directory at 0600 and untraversable.
	if err := os.Chmod(dir, DirMode); err != nil {
		return fmt.Errorf("secrets: chmod %s: %w", dir, err)
	}
	return chownRoot(dir)
}

// statRegular refuses anything that is not a plain file, and it lstats rather
// than stats so that a symlink is refused as a symlink.
//
// The directory is 0700 root:root, so planting a link here already needs root
// — but a secret path that silently follows one is how a store ends up
// reporting the contents of /etc/shadow as an instance's rcon password, and
// the check costs one syscall.
func statRegular(p string) (fs.FileInfo, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotRegular, p, fi.Mode().Type())
	}
	return fi, nil
}

// InsecureMode reports whether a permission bit set lets anyone but the owner
// read or write the file. It is the predicate behind the master-key refusal
// and behind `gvm doctor` HOST-006.
func InsecureMode(perm fs.FileMode) bool { return perm.Perm()&0o077 != 0 }

// MasterKey is the AES-256 key behind the TOTP column (class B, section 14.7).
// It is not a game secret: it never reaches an instance, gvm-web never loads
// it, and losing it costs every user's TOTP enrolment rather than a world.
type MasterKey struct {
	// Value carries the raw key bytes. Holding them in a Value rather than a
	// []byte is what keeps the key out of a panic dump: a []byte field prints
	// as hexadecimal from any %v on the enclosing struct.
	Value Value
	// ModeChecked is false when the platform cannot report unix permission
	// bits, which means the group- and world-readable refusal below did NOT
	// happen. A caller that treats a MasterKey as safe without looking at
	// this has silently skipped the check on that platform.
	ModeChecked bool
}

// LoadMasterKey reads and vets the master key.
//
// It refuses a group- or world-accessible file outright. A 0644 master.key is
// a silent downgrade of everything below it: every TOTP seed on the box
// becomes readable by any account that can traverse /etc/gvm, and nothing
// anywhere reports a problem because the decryption keeps working perfectly.
func LoadMasterKey(p string) (MasterKey, error) {
	fi, err := statRegular(p)
	if err != nil {
		return MasterKey{}, fmt.Errorf("secrets: master key %s: %w", p, err)
	}
	perm, checked := permOf(fi)
	if checked && InsecureMode(perm) {
		return MasterKey{}, fmt.Errorf("secrets: master key %s is mode %04o, want %04o: %w",
			p, perm, MasterKeyMode, ErrInsecureMode)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return MasterKey{}, fmt.Errorf("secrets: master key %s: %w", p, err)
	}
	if len(b) != MasterKeySize {
		// No trailing-newline trim, unlike a game secret: the key is raw
		// binary and one byte in 256 of it legitimately IS a newline, so
		// trimming would silently shorten one key in 256 and produce a box
		// that cannot decrypt what the previous boot encrypted.
		hint := ""
		if len(b) == MasterKeySize+1 && b[len(b)-1] == '\n' {
			hint = " (it ends in a newline; the key file is raw bytes, so write it with `printf` or `gvm secret rotate-key`, not `echo`)"
		}
		return MasterKey{}, fmt.Errorf("secrets: master key %s is %d bytes, want %d%s",
			p, len(b), MasterKeySize, hint)
	}
	return MasterKey{Value: New(string(b)), ModeChecked: checked}, nil
}

// WriteMasterKey writes the key at 0400 root:root.
//
// It exists so that the format LoadMasterKey enforces has exactly one writer.
// A key file produced by hand is the one that ends up with a trailing newline.
func WriteMasterKey(p string, k MasterKey) error {
	raw := k.Value.plain()
	if len(raw) != MasterKeySize {
		return fmt.Errorf("secrets: master key is %d bytes, want %d", len(raw), MasterKeySize)
	}
	if err := spec.WriteFileAtomicNoBackup(p, []byte(raw), MasterKeyMode); err != nil {
		return fmt.Errorf("secrets: write master key %s: %w", p, err)
	}
	return chownRoot(p)
}
