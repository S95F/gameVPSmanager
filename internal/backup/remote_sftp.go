package backup

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The one off-box target v1 ships, and the parts of it that are decided here
// rather than on the wire.
//
// SFTP rather than an S3 SDK because section 13.2 is explicit about the
// operator this tool is for: `x/crypto/ssh` plus `pkg/sftp` are pure Go and
// they point at the NAS a single-VPS operator actually owns. RemoteTarget is
// an interface so S3 and B2 land later without touching the engine.
//
// What is in this file is everything that can be got wrong WITHOUT a network:
// the host-key pin, the refusal to trust on first use, the remote path, the
// name, and the fact that the private key never passes through this package.
// Those are the parts that decide whether the upload is safe, and they are all
// pure functions with tests.
//
// What is NOT in this file is the SSH handshake, and that is a dependency
// decision rather than an omission. The project's dependency set is yaml.v3,
// x/sys, pgzip and go-systemd, and adding x/crypto plus pkg/sftp is a decision
// that belongs to the phase that ships the wire -- not one to smuggle in
// underneath a backup engine. So Dialer is the seam, newSFTPTarget without one
// REFUSES, and the refusal names what is missing.
//
// It refuses rather than skipping, and that distinction is the whole reason it
// is written this way: a push that quietly did nothing would leave an operator
// believing they have an off-box copy of a world they do not have a copy of.

// ErrNoSFTPTransport is a configured sftp target with no dialer.
var ErrNoSFTPTransport = errors.New("backup: this build has no SFTP transport wired")

// ErrHostKey is a host_key line that cannot be used as a pin.
var ErrHostKey = errors.New("backup: the pinned host key cannot be parsed")

// ErrHostKeyMismatch is a server whose key is not the pinned one. It is its own
// sentinel because it is the one failure here that is not a configuration
// mistake: either the server was rebuilt, or somebody is between gvm and it,
// and the operator has to decide which.
var ErrHostKeyMismatch = errors.New("backup: the remote host presented a key that is not the pinned one")

// hostKeyAlgos is the set gvm will pin.
//
// Narrow on purpose. ssh-rsa with SHA-1 signatures is disabled by default in
// OpenSSH 8.8 and later, and a pin an operator cannot use is worse than a
// refusal they can read; ssh-dss has been dead for a decade. The two here are
// what `ssh-keyscan` offers on anything built this decade.
var hostKeyAlgos = map[string]bool{
	"ssh-ed25519":                true,
	"ecdsa-sha2-nistp256":        true,
	"ecdsa-sha2-nistp384":        true,
	"ecdsa-sha2-nistp521":        true,
	"rsa-sha2-256":               true,
	"rsa-sha2-512":               true,
	"sk-ssh-ed25519@openssh.com": true,
}

// HostKey is a pinned public host key.
type HostKey struct {
	// Algo is the key type, "ssh-ed25519".
	Algo string
	// Blob is the wire-format public key, base64-decoded. Comparing the blob
	// rather than the base64 text means a pin that was pasted with different
	// whitespace or a trailing comment still matches.
	Blob []byte
	// Raw is the line as the operator wrote it, for the diagnostic.
	Raw string
}

// ParseHostKey reads an authorized_keys-style host key line.
//
// It accepts "<algo> <base64>" with an optional trailing comment, which is
// what `ssh-keyscan -t ed25519 host` prints minus the leading hostname. A
// leading hostname field is accepted too, because that is what an operator
// will paste, and refusing their own keyscan output over a field they did not
// know to strip is how a host_key ends up empty.
func ParseHostKey(s string) (HostKey, error) {
	line := strings.TrimSpace(s)
	if line == "" {
		return HostKey{}, fmt.Errorf("%w: the line is empty", ErrHostKey)
	}
	if i := strings.IndexAny(line, "#"); i == 0 {
		return HostKey{}, fmt.Errorf("%w: the line is a comment", ErrHostKey)
	}
	fields := strings.Fields(line)
	// Find the algorithm field. keyscan output is "<host> <algo> <base64>";
	// a pasted host_key is usually "<algo> <base64>".
	idx := -1
	for i, f := range fields {
		if hostKeyAlgos[f] {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(fields) {
		return HostKey{}, fmt.Errorf("%w: %q names no key type gvm will pin (want one of %s)",
			ErrHostKey, truncate(line, 48), algoList())
	}
	algo, b64 := fields[idx], fields[idx+1]
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return HostKey{}, fmt.Errorf("%w: the key after %s is not base64: %v", ErrHostKey, algo, err)
	}
	if len(blob) == 0 {
		return HostKey{}, fmt.Errorf("%w: the key after %s decodes to nothing", ErrHostKey, algo)
	}
	// The wire format's first field is the algorithm name again, and a blob
	// whose embedded name disagrees with the text one is either a paste error
	// or a deliberately confusing pin. Checking it costs four lines and makes
	// the two spellings of the same claim agree.
	if inner, ok := sshStringAt(blob, 0); !ok || inner != algo {
		return HostKey{}, fmt.Errorf("%w: the %s key's own encoded type is %q", ErrHostKey, algo, inner)
	}
	return HostKey{Algo: algo, Blob: blob, Raw: line}, nil
}

// Matches reports whether a presented key is the pinned one.
//
// Constant time, which is close to superstition for a public key and costs
// nothing: the comparison is on the server's identity, the function will one
// day be reached from a loop over several offered algorithms, and
// subtle.ConstantTimeCompare is one call.
func (h HostKey) Matches(algo string, blob []byte) bool {
	if h.Algo == "" || len(h.Blob) == 0 {
		// An unset pin matches NOTHING. This is the trust-on-first-use
		// refusal expressed as a method: a zero HostKey must never read as
		// "anything goes".
		return false
	}
	if algo != h.Algo {
		return false
	}
	return subtle.ConstantTimeCompare(h.Blob, blob) == 1
}

// Fingerprint is the pin as it goes in a log line: OpenSSH's SHA256 form,
// "SHA256:" followed by the unpadded base64 of sha256(blob). It is byte for
// byte what `ssh-keygen -lf` prints for the same key, so an operator can
// compare the pin in a push log against their own `ssh-keyscan`/`ssh-keygen`
// output directly, and a whole key never ends up in a journal.
//
// The earlier form printed the algorithm and a truncated slice of the key's own
// base64, which matched nothing an operator could produce with a standard tool;
// a fingerprint they cannot cross-check is a fingerprint that quietly stops
// being checked.
func (h HostKey) Fingerprint() string {
	if h.Algo == "" {
		return "(unpinned)"
	}
	sum := sha256.Sum256(h.Blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// SSHConfig is everything a dialer needs, and nothing it does not.
//
// KeyFile is a PATH and not key material. The private key is read by the
// dialer, as root, at the moment it is used -- so it never passes through this
// package's arguments, error strings, job records or audit params. That is the
// same reasoning that keeps secrets out of hostops' Request struct.
type SSHConfig struct {
	Host string
	Port int
	User string
	// KeyFile is /etc/gvm/secrets/host/sftp_ed25519, 0600 root:root (D-02).
	KeyFile string
	// HostKey is the pin. A dialer MUST compare against it and MUST NOT fall
	// back to known_hosts, and the zero value matching nothing is what makes
	// a dialer that forgot fail closed.
	HostKey HostKey
	// Dir is the remote directory every Put, List and Remove is relative to.
	Dir string
}

// DefaultSSHPort is 22. Named because a zero Port arriving from a half-filled
// struct should connect to the obvious place rather than to port 0.
const DefaultSSHPort = 22

// Addr is host:port.
func (c SSHConfig) Addr() string {
	p := c.Port
	if p <= 0 {
		p = DefaultSSHPort
	}
	return fmt.Sprintf("%s:%d", c.Host, p)
}

// Describe is the job-log line: who, where, and which key is pinned. No path
// to the private key and no key material.
func (c SSHConfig) Describe() string {
	return fmt.Sprintf("sftp %s@%s:%s [%s]", c.User, c.Addr(), c.Dir, c.HostKey.Fingerprint())
}

// Dialer opens an authenticated SFTP session.
//
// The seam x/crypto/ssh plus pkg/sftp land behind. A dialer is handed the
// config above and nothing else, and the one thing it must not do is consult
// known_hosts: the pin is the whole authentication story on this path.
type Dialer interface {
	Dial(ctx context.Context, cfg SSHConfig) (Session, error)
}

// SFTPTarget is the sftp transport.
type SFTPTarget struct {
	Cfg SSHConfig
	// Dialer is nil in this build. See the file comment: the handshake is a
	// dependency decision and the refusal is deliberate.
	Dialer Dialer
}

// Describe implements Transport.
func (t *SFTPTarget) Describe() string { return t.Cfg.Describe() }

// Open implements Transport.
func (t *SFTPTarget) Open(ctx context.Context) (Session, error) {
	if t.Dialer == nil {
		return nil, fmt.Errorf("%w: %s is configured but no dialer is wired, so nothing was "+
			"uploaded. The handshake needs an SSH client and this build's dependency set does "+
			"not have one; the local archive is unaffected and is the protection that matters",
			ErrNoSFTPTransport, t.Cfg.Describe())
	}
	if t.Cfg.HostKey.Algo == "" {
		// Belt and braces over Remote.Validate: a target assembled directly
		// rather than through the validator must still not dial unpinned.
		return nil, fmt.Errorf("%w: %s has no pinned host key", ErrRemote, t.Cfg.Addr())
	}
	return t.Dialer.Dial(ctx, t.Cfg)
}

// newSFTPTarget builds the transport from the instance's remote block.
func newSFTPTarget(t spec.RemoteTarget) (Transport, error) {
	hk, err := ParseHostKey(t.HostKey)
	if err != nil {
		return nil, err
	}
	host, port := splitHostPort(strings.TrimSpace(t.Host))
	return &SFTPTarget{Cfg: SSHConfig{
		Host:    host,
		Port:    port,
		User:    strings.TrimSpace(t.User),
		KeyFile: strings.TrimSpace(t.KeyFile),
		HostKey: hk,
		Dir:     path.Clean(strings.TrimSpace(t.Path)),
	}}, nil
}

// splitHostPort accepts "backup.example.net" and "backup.example.net:2222".
//
// It does NOT accept a bracketed IPv6 literal with a port, and says so by
// returning the whole string as the host: guessing wrong about where a colon
// belongs inside an address is how a backup goes to the wrong machine, and a
// dialer handed an address it cannot resolve fails loudly.
func splitHostPort(s string) (string, int) {
	if strings.HasPrefix(s, "[") || strings.Count(s, ":") != 1 {
		return s, 0
	}
	host, portStr, found := strings.Cut(s, ":")
	if !found {
		return s, 0
	}
	port := 0
	for _, r := range portStr {
		if r < '0' || r > '9' {
			return s, 0
		}
		port = port*10 + int(r-'0')
		if port > 65535 {
			return s, 0
		}
	}
	if port == 0 {
		return s, 0
	}
	return host, port
}

// sshStringAt reads a length-prefixed string from an SSH wire-format blob.
//
// Four bytes of big-endian length then that many bytes. Written out rather
// than reached for from a library because this package has no SSH library, and
// the one field it needs is the first one.
func sshStringAt(b []byte, off int) (string, bool) {
	if off+4 > len(b) {
		return "", false
	}
	n := int(b[off])<<24 | int(b[off+1])<<16 | int(b[off+2])<<8 | int(b[off+3])
	if n < 0 || off+4+n > len(b) {
		return "", false
	}
	return string(b[off+4 : off+4+n]), true
}

func algoList() string {
	out := make([]string, 0, len(hostKeyAlgos))
	for a := range hostKeyAlgos {
		out = append(out, a)
	}
	// Sorted so the message is the same every time somebody pastes a bad key.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return strings.Join(out, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
