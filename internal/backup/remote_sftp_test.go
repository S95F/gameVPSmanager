package backup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// ed25519HostKey builds a wire-format ssh-ed25519 public key blob.
//
// Built rather than pasted so the test does not carry somebody's real host
// key, and cross-checked below against the base64 prefix every real
// ssh-ed25519 key line starts with -- which is what makes the hand-rolled
// length framing in sshStringAt verifiable without an SSH library.
func ed25519HostKey(fill byte) (algo string, blob []byte, line string) {
	algo = "ssh-ed25519"
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	blob = make([]byte, 0, 4+len(algo)+4+len(key))
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(algo)))
	blob = append(blob, algo...)
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(key)))
	blob = append(blob, key...)
	return algo, blob, algo + " " + base64.StdEncoding.EncodeToString(blob)
}

// TestTheHostKeyWireFormatMatchesWhatSSHKeyscanPrints.
//
// Every real ssh-ed25519 key line's base64 begins with the framing of
// "ssh-ed25519" and a 32-byte key, which is this prefix. Asserting it is how
// the four lines of hand-rolled length decoding in this package are checked
// against reality rather than against themselves.
func TestTheHostKeyWireFormatMatchesWhatSSHKeyscanPrints(t *testing.T) {
	_, _, line := ed25519HostKey(0xab)
	_, b64, _ := strings.Cut(line, " ")
	b64 = strings.TrimSpace(b64)
	const realWorldPrefix = "AAAAC3NzaC1lZDI1NTE5AAAAI"
	if !strings.HasPrefix(b64, realWorldPrefix) {
		t.Fatalf("the built key's base64 starts %q, and every ssh-ed25519 key starts %q",
			b64[:min(len(b64), len(realWorldPrefix))], realWorldPrefix)
	}
}

// TestParseHostKeyAcceptsWhatAnOperatorWillActuallyPaste.
//
// `ssh-keyscan -t ed25519 host` prints "<host> <algo> <base64>". Refusing
// their own keyscan output over a leading field they did not know to strip is
// how a host_key ends up empty -- and an empty host_key is the trust-on-first-
// use this package exists to refuse.
func TestParseHostKeyAcceptsWhatAnOperatorWillActuallyPaste(t *testing.T) {
	algo, blob, line := ed25519HostKey(0x11)
	for name, in := range map[string]string{
		"algo and key":           line,
		"keyscan output":         "backup.example.net " + line,
		"with a comment":         line + " root@backup",
		"with surrounding space": "   " + line + "  \n",
	} {
		t.Run(name, func(t *testing.T) {
			hk, err := ParseHostKey(in)
			if err != nil {
				t.Fatalf("ParseHostKey(%q): %v", in, err)
			}
			if hk.Algo != algo {
				t.Errorf("algo = %q, want %q", hk.Algo, algo)
			}
			if !hk.Matches(algo, blob) {
				t.Error("the parsed pin does not match the key it was parsed from")
			}
		})
	}
}

// TestParseHostKeyRefusesWhatItCannotPin.
func TestParseHostKeyRefusesWhatItCannotPin(t *testing.T) {
	_, _, line := ed25519HostKey(0x22)
	_, goodB64, _ := strings.Cut(line, " ")

	cases := map[string]struct{ in, want string }{
		"empty":   {in: "   ", want: "the line is empty"},
		"comment": {in: "# the backup host", want: "the line is a comment"},
		// ssh-rsa with SHA-1 signatures is disabled by default in OpenSSH 8.8
		// and later, and ssh-dss has been dead for a decade: a pin an operator
		// cannot use is worse than a refusal they can read.
		"a dead algorithm":  {in: "ssh-dss " + goodB64, want: "names no key type gvm will pin"},
		"no key at all":     {in: "ssh-ed25519", want: "names no key type gvm will pin"},
		"not base64":        {in: "ssh-ed25519 !!!!not-base64!!!!", want: "is not base64"},
		"a mismatched blob": {in: "ecdsa-sha2-nistp256 " + goodB64, want: "own encoded type"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHostKey(tc.in)
			if !errors.Is(err, ErrHostKey) {
				t.Fatalf("ParseHostKey(%q) error = %v, want ErrHostKey", tc.in, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestAnUnsetPinMatchesNothing.
//
// The trust-on-first-use refusal, expressed as a method. A zero HostKey
// reading as "anything goes" is the single most dangerous default this file
// could have, because it would turn a forgotten host_key into a push that
// authenticates against whatever answered.
func TestAnUnsetPinMatchesNothing(t *testing.T) {
	algo, blob, _ := ed25519HostKey(0x33)
	var zero HostKey
	if zero.Matches(algo, blob) {
		t.Fatal("an unpinned HostKey matched a key")
	}
	if zero.Matches("", nil) {
		t.Fatal("an unpinned HostKey matched an empty key")
	}
	if zero.Fingerprint() != "(unpinned)" {
		t.Errorf("Fingerprint() = %q", zero.Fingerprint())
	}
}

// TestThePinRejectsADifferentKeyAndADifferentAlgorithm.
func TestThePinRejectsADifferentKeyAndADifferentAlgorithm(t *testing.T) {
	_, _, line := ed25519HostKey(0x44)
	hk, err := ParseHostKey(line)
	if err != nil {
		t.Fatal(err)
	}
	_, otherBlob, _ := ed25519HostKey(0x45)
	if hk.Matches("ssh-ed25519", otherBlob) {
		t.Error("the pin accepted a different key; either the server was rebuilt or somebody is " +
			"in the middle, and both need the operator to decide")
	}
	if hk.Matches("ecdsa-sha2-nistp256", hk.Blob) {
		t.Error("the pin accepted the right bytes under the wrong algorithm")
	}
}

// TestFingerprintIsTheOpenSSHSHA256Form: the pin a push log prints is exactly
// what `ssh-keygen -lf` prints for the same key — "SHA256:" then the unpadded
// base64 of sha256(blob) — so an operator can compare the two without special
// tooling, and no key material lands in the journal.
func TestFingerprintIsTheOpenSSHSHA256Form(t *testing.T) {
	_, blob, line := ed25519HostKey(0x44)
	hk, err := ParseHostKey(line)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	want := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	got := hk.Fingerprint()
	if got != want {
		t.Errorf("Fingerprint() = %q, want %q (ssh-keygen -lf's form)", got, want)
	}
	// The format itself: the OpenSSH prefix, no base64 padding, and the 43
	// characters a 32-byte digest raw-encodes to. ssh-keygen prints exactly
	// this token, so any drift here is drift away from it.
	if !strings.HasPrefix(got, "SHA256:") {
		t.Errorf("Fingerprint() = %q, want the SHA256: prefix ssh-keygen uses", got)
	}
	if strings.Contains(got, "=") {
		t.Errorf("Fingerprint() = %q carries base64 padding; ssh-keygen prints the unpadded form", got)
	}
	if digest := strings.TrimPrefix(got, "SHA256:"); len(digest) != 43 {
		t.Errorf("the digest is %d chars, want 43 (a 32-byte sha256, raw-base64): %q", len(digest), got)
	}
}

// TestAnSFTPTargetWithNoHostKeyIsRefusedWithTheCommandToFixIt.
//
// Section 13.2: "PINNED; no TOFU, no known_hosts". This is the one validation
// an operator will be tempted to work around, so the message says what to
// paste and where to get it.
func TestAnSFTPTargetWithNoHostKeyIsRefusedWithTheCommandToFixIt(t *testing.T) {
	r := &Remote{Target: spec.RemoteTarget{
		Kind: RemoteSFTP, Host: "backup.example.net", User: "gvmpush",
		Path: "/srv/backups/gvm/pz-main", KeyFile: "/etc/gvm/secrets/host/sftp_ed25519",
	}}
	err := r.Validate()
	if !errors.Is(err, ErrRemote) {
		t.Fatalf("Validate error = %v, want ErrRemote", err)
	}
	if !strings.Contains(err.Error(), "ssh-keyscan -t ed25519 backup.example.net") {
		t.Errorf("the refusal does not say how to get the key: %v", err)
	}
	if !strings.Contains(err.Error(), "trust-on-first-use") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestValidateNamesEveryMissingFieldAtOnce.
//
// Joined rather than first-wins, because an operator filling in a remote block
// for the first time should get the whole list in one run rather than five.
func TestValidateNamesEveryMissingFieldAtOnce(t *testing.T) {
	r := &Remote{Target: spec.RemoteTarget{Kind: RemoteSFTP}}
	err := r.Validate()
	if err == nil {
		t.Fatal("an empty sftp target validated")
	}
	for _, want := range []string{"no host", "no user", "no path", "no key_file", "no host_key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestAnUnknownRemoteKindIsRefusedRatherThanIgnored.
func TestAnUnknownRemoteKindIsRefusedRatherThanIgnored(t *testing.T) {
	r := &Remote{Target: spec.RemoteTarget{Kind: "s3"}}
	if err := r.Validate(); !errors.Is(err, ErrRemote) {
		t.Fatalf("Validate error = %v, want ErrRemote", err)
	}
	// And `none` is not an error at all: it is the default.
	none := &Remote{Target: spec.RemoteTarget{Kind: RemoteNone}}
	if none.Enabled() {
		t.Error("kind: none reported itself enabled")
	}
	if err := none.Validate(); err != nil {
		t.Errorf("kind: none did not validate: %v", err)
	}
}

// TestAConfiguredSFTPTargetWithNoDialerRefusesAndSaysWhatIsUnaffected.
//
// The handshake is a dependency decision this build has not taken, so there is
// no transport. It REFUSES rather than skipping, because a push that quietly
// did nothing would leave an operator believing they have an off-box copy of a
// world they do not have a copy of -- and the message says the local archive is
// fine, which is the part that decides whether they need to act tonight.
func TestAConfiguredSFTPTargetWithNoDialerRefusesAndSaysWhatIsUnaffected(t *testing.T) {
	_, _, line := ed25519HostKey(0x55)
	tr, err := newSFTPTarget(spec.RemoteTarget{
		Kind: RemoteSFTP, Host: "backup.example.net:2222", User: "gvmpush",
		Path: "/srv/backups/gvm/pz-main/", KeyFile: "/etc/gvm/secrets/host/sftp_ed25519",
		HostKey: line,
	})
	if err != nil {
		t.Fatalf("newSFTPTarget: %v", err)
	}
	if _, err := tr.Open(context.Background()); !errors.Is(err, ErrNoSFTPTransport) {
		t.Fatalf("Open error = %v, want ErrNoSFTPTransport", err)
	} else if !strings.Contains(err.Error(), "local archive is unaffected") {
		t.Errorf("the refusal does not say what is still protected: %v", err)
	}

	// The description is what goes in the job log: who, where, which pin. The
	// private key is a PATH handed to the dialer and must not appear.
	d := tr.Describe()
	for _, want := range []string{"gvmpush", "backup.example.net:2222", "/srv/backups/gvm/pz-main", "SHA256:"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe() = %q, missing %q", d, want)
		}
	}
	if strings.Contains(d, "sftp_ed25519") {
		t.Errorf("Describe() names the private key file: %q", d)
	}
}

// TestADialerIsNeverHandedAnUnpinnedConfig.
//
// Belt and braces over Validate: a target assembled directly rather than
// through the validator must still not dial. A dialer that was handed a zero
// HostKey and fell back to known_hosts would undo the whole of rule two.
func TestADialerIsNeverHandedAnUnpinnedConfig(t *testing.T) {
	called := false
	tgt := &SFTPTarget{
		Cfg:    SSHConfig{Host: "backup.example.net", User: "gvmpush", Dir: "/srv"},
		Dialer: dialerFunc(func(context.Context, SSHConfig) (Session, error) { called = true; return nil, nil }),
	}
	if _, err := tgt.Open(context.Background()); !errors.Is(err, ErrRemote) {
		t.Fatalf("Open error = %v, want ErrRemote", err)
	}
	if called {
		t.Fatal("the dialer was called with no pinned host key")
	}
}

// TestADialerGetsThePathAndNotTheKey.
//
// The private key never passes through this package's arguments, error
// strings, job records or audit params: the dialer is handed a PATH and reads
// it, as root, at the moment it is used. That is the same reasoning that keeps
// secrets out of hostops' Request struct.
func TestADialerGetsThePathAndNotTheKey(t *testing.T) {
	_, _, line := ed25519HostKey(0x66)
	hk, err := ParseHostKey(line)
	if err != nil {
		t.Fatal(err)
	}
	var got SSHConfig
	tgt := &SFTPTarget{
		Cfg: SSHConfig{Host: "nas.lan", User: "gvmpush", Dir: "/srv/backups",
			KeyFile: "/etc/gvm/secrets/host/sftp_ed25519", HostKey: hk},
		Dialer: dialerFunc(func(_ context.Context, c SSHConfig) (Session, error) {
			got = c
			return &fakeSession{}, nil
		}),
	}
	if _, err := tgt.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.KeyFile != "/etc/gvm/secrets/host/sftp_ed25519" {
		t.Errorf("KeyFile = %q, want the path", got.KeyFile)
	}
	if got.Addr() != "nas.lan:22" {
		t.Errorf("Addr() = %q, want the default port", got.Addr())
	}
	if !got.HostKey.Matches(hk.Algo, hk.Blob) {
		t.Error("the dialer was not handed the pin")
	}
}

// TestSplitHostPortGuessesNothing.
//
// Guessing wrong about where a colon belongs inside an address is how a backup
// goes to the wrong machine, so anything ambiguous is returned whole and the
// dialer fails loudly on an address it cannot resolve.
func TestSplitHostPortGuessesNothing(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
	}{
		{"backup.example.net", "backup.example.net", 0},
		{"backup.example.net:2222", "backup.example.net", 2222},
		{"192.0.2.10:22", "192.0.2.10", 22},
		// Ambiguous or malformed: returned whole rather than split.
		{"[2001:db8::1]:22", "[2001:db8::1]:22", 0},
		{"2001:db8::1", "2001:db8::1", 0},
		{"host:notaport", "host:notaport", 0},
		{"host:0", "host:0", 0},
		{"host:99999", "host:99999", 0},
	}
	for _, tc := range cases {
		host, port := splitHostPort(tc.in)
		if host != tc.host || port != tc.port {
			t.Errorf("splitHostPort(%q) = (%q, %d), want (%q, %d)", tc.in, host, port, tc.host, tc.port)
		}
	}
}

// dialerFunc adapts a function to Dialer.
type dialerFunc func(context.Context, SSHConfig) (Session, error)

func (f dialerFunc) Dial(ctx context.Context, c SSHConfig) (Session, error) { return f(ctx, c) }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
