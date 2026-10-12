package backup

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// The encryption recipient, and the one property that makes an off-box backup
// of a game server defensible.
//
// `encrypt_to` is a PUBLIC key. The VPS holds no private half, so it can
// produce backups it cannot itself decrypt -- which means a box that is fully
// compromised leaks this week's world and not the archive of every world the
// server has ever had. backup.sh has no equivalent: its tarballs are readable
// by anything that can read the backup directory, locally and on the NAS.
//
// That property is worth exactly as much as the guarantee that it cannot be
// skipped. So this file's job is two things:
//
//  1. parse and validate the recipient, strictly, before anything is uploaded.
//     An `age1...` string with one transposed character is a different key, and
//     bech32's checksum is what turns that into a refusal instead of an
//     unopenable archive discovered at restore time.
//  2. make a configured recipient with no encrypter a REFUSAL. Remote.encrypter
//     is where that happens, and ErrNoEncrypter is declared here beside the
//     reason: uploading the world in the clear to the path somebody configured
//     for encrypted backups would be gvm deciding their threat model did not
//     matter.
//
// The AEAD itself is not here, and that is a dependency decision rather than an
// omission -- the same one remote_sftp.go documents. age's format is X25519
// plus ChaCha20-Poly1305, and ChaCha20-Poly1305 is in golang.org/x/crypto,
// which this project does not depend on. Hand-rolling an AEAD to avoid an
// import would be the worst of the available choices. So Encrypter is the seam,
// NewAgeEncrypter takes the constructor, and an unwired recipient refuses.

// AgeExt is appended to an archive's name when the stream is encrypted. It is
// age's own extension, so `age -d -i key < file.tar.gz.age > file.tar.gz` is
// the whole of a manual restore.
const AgeExt = ".age"

// ErrRecipient is an encrypt_to value that is not a usable age recipient.
var ErrRecipient = errors.New("backup: encrypt_to is not a valid age recipient")

// ErrNoEncrypter is a configured recipient with no encrypter available.
//
// It is a push failure and never a silent plaintext upload. The remote's
// on_failure policy still decides whether it fails the JOB -- the local
// archive is the protection that matters -- but it never decides to upload the
// world unencrypted.
var ErrNoEncrypter = errors.New("backup: an encryption recipient is configured and no encrypter is available")

// ageHRP is the human-readable part of an age X25519 recipient.
const ageHRP = "age"

// x25519KeyLen is how many bytes an age X25519 recipient carries. A recipient
// of any other length is not one, whatever its checksum says.
const x25519KeyLen = 32

// Recipient is a parsed age X25519 recipient.
type Recipient struct {
	// Raw is the string as written, lowercased. age recipients are
	// lowercase; see the mixed-case refusal in decodeBech32.
	Raw string
	// Key is the 32-byte X25519 public key.
	Key [x25519KeyLen]byte
}

// Short is the recipient as it goes in a log line or a job record.
//
// Truncated, even though the key is public. Not for secrecy: a 62-character
// opaque string in the middle of a sentence is unreadable, and the first dozen
// characters are enough for an operator to tell which of their two keys an
// archive was encrypted to.
func (r Recipient) Short() string {
	if len(r.Raw) <= 16 {
		return r.Raw
	}
	return r.Raw[:16] + "..."
}

// ParseRecipient decodes and validates an `age1...` recipient.
func ParseRecipient(s string) (Recipient, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return Recipient{}, fmt.Errorf("%w: it is empty", ErrRecipient)
	}
	hrp, data, err := decodeBech32(v)
	if err != nil {
		return Recipient{}, fmt.Errorf("%w: %w", ErrRecipient, err)
	}
	if hrp != ageHRP {
		// `age1...` is an X25519 recipient; `AGE-SECRET-KEY-1...` is the
		// PRIVATE half and is not bech32 with this hrp. Naming both in the
		// message is worth it: pasting the identity instead of the recipient
		// is the mistake somebody will make, and it is the one mistake that
		// would put the private key in a YAML file.
		return Recipient{}, fmt.Errorf("%w: its prefix is %q and an age recipient's is %q. If "+
			"this is an AGE-SECRET-KEY, it is the PRIVATE half and must never be in an instance "+
			"file -- encrypt_to takes the public recipient", ErrRecipient, hrp, ageHRP)
	}
	if len(data) != x25519KeyLen {
		return Recipient{}, fmt.Errorf("%w: it decodes to %d bytes and an X25519 recipient is %d",
			ErrRecipient, len(data), x25519KeyLen)
	}
	var out Recipient
	out.Raw = strings.ToLower(v)
	copy(out.Key[:], data)
	return out, nil
}

// AgeEncrypter wraps an upload stream for one or more recipients.
//
// New is the constructor the phase that takes the x/crypto dependency fills
// in: age.Encrypt(w, recipients...) is a one-line adapter to it. Until then a
// nil New is a refusal, so the only way to get an unencrypted upload out of a
// configured recipient is to remove the recipient.
type AgeEncrypter struct {
	Recipients []Recipient
	New        func(w io.Writer, to []Recipient) (io.WriteCloser, error)
}

// NewAgeEncrypter builds an encrypter for one recipient string.
func NewAgeEncrypter(to string, newWriter func(w io.Writer, to []Recipient) (io.WriteCloser, error)) (*AgeEncrypter, error) {
	rec, err := ParseRecipient(to)
	if err != nil {
		return nil, err
	}
	return &AgeEncrypter{Recipients: []Recipient{rec}, New: newWriter}, nil
}

// Describe names the recipients, for the push record.
func (a *AgeEncrypter) Describe() string {
	if a == nil || len(a.Recipients) == 0 {
		return ""
	}
	parts := make([]string, 0, len(a.Recipients))
	for _, r := range a.Recipients {
		parts = append(parts, r.Short())
	}
	return strings.Join(parts, ",")
}

// Wrap implements Encrypter.
func (a *AgeEncrypter) Wrap(w io.Writer) (io.WriteCloser, error) {
	if a == nil || len(a.Recipients) == 0 {
		return nil, fmt.Errorf("%w: no recipients", ErrNoEncrypter)
	}
	if a.New == nil {
		return nil, fmt.Errorf("%w: %s is configured and no age writer is wired into this build",
			ErrNoEncrypter, a.Describe())
	}
	return a.New(w, a.Recipients)
}

// --- bech32 ------------------------------------------------------------------
//
// Implemented here rather than imported, and the reason is proportionality: it
// is sixty lines of table-driven arithmetic with a published test vector set,
// while the alternative is a module dependency to validate one string in one
// config block. It is also the HALF of age that can be implemented correctly
// without a cryptographic primitive -- the checksum is error detection, not
// authentication -- so writing it here and seaming out the AEAD divides the
// problem along the line the dependency rule draws.
//
// BIP-173 bech32, not bech32m: age recipients use the original constant. A
// bech32m string therefore fails the checksum here, which is correct -- it is
// not an age recipient.

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// bech32Generator is BIP-173's generator polynomial.
var bech32Generator = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

// bech32MaxLen bounds the input.
//
// BIP-173's own limit is 90 characters. age recipients are 62, so the limit is
// generous; it exists so that a megabyte pasted into encrypt_to is a one-line
// refusal rather than a loop over a megabyte.
const bech32MaxLen = 128

var errBech32 = errors.New("it is not a valid bech32 string")

// decodeBech32 returns the human-readable part and the decoded 8-bit payload.
func decodeBech32(s string) (string, []byte, error) {
	if len(s) > bech32MaxLen {
		return "", nil, fmt.Errorf("%w: it is %d characters and the limit is %d", errBech32, len(s), bech32MaxLen)
	}
	// Mixed case is refused outright rather than normalised. BIP-173 requires
	// it: the checksum is computed over one case, so a mixed-case string is
	// one whose checksum cannot be verified at all, and normalising it would
	// turn a detectable corruption into an accepted key.
	lower, upper := strings.ToLower(s), strings.ToUpper(s)
	switch {
	case s != lower && s != upper:
		return "", nil, fmt.Errorf("%w: it mixes upper and lower case, which bech32 forbids "+
			"because the checksum is case-specific", errBech32)
	}
	s = lower

	cut := strings.LastIndexByte(s, '1')
	if cut < 1 {
		return "", nil, fmt.Errorf("%w: it has no separator, so there is no prefix to check", errBech32)
	}
	hrp, dataPart := s[:cut], s[cut+1:]
	// Six characters of checksum, and at least one of payload.
	if len(dataPart) < 7 {
		return "", nil, fmt.Errorf("%w: there are %d characters after the separator and the "+
			"checksum alone is 6", errBech32, len(dataPart))
	}
	for i := 0; i < len(hrp); i++ {
		if hrp[i] < 33 || hrp[i] > 126 {
			return "", nil, fmt.Errorf("%w: the prefix contains a character bech32 does not allow", errBech32)
		}
	}

	values := make([]byte, 0, len(dataPart))
	for i := 0; i < len(dataPart); i++ {
		idx := strings.IndexByte(bech32Charset, dataPart[i])
		if idx < 0 {
			// 1, b, i and o are deliberately absent from the charset because
			// they are the characters people misread. A string containing one
			// is a transcription error, which is exactly what this check is
			// for.
			return "", nil, fmt.Errorf("%w: %q is not a bech32 character", errBech32, string(dataPart[i]))
		}
		values = append(values, byte(idx))
	}

	if bech32Polymod(bech32HRPExpand(hrp), values) != 1 {
		return "", nil, fmt.Errorf("%w: the checksum does not match, so at least one character "+
			"is wrong; a key with a transposed character is a DIFFERENT key, and an archive "+
			"encrypted to it could not be decrypted by anybody", errBech32)
	}

	out, err := convertBits(values[:len(values)-6], 5, 8, false)
	if err != nil {
		return "", nil, err
	}
	return hrp, out, nil
}

// bech32HRPExpand is BIP-173's hrp expansion: the high bits, a zero, then the
// low bits.
func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

// bech32Polymod is the checksum over the expanded hrp followed by the data.
func bech32Polymod(parts ...[]byte) uint32 {
	chk := uint32(1)
	for _, part := range parts {
		for _, v := range part {
			top := chk >> 25
			chk = (chk&0x1ffffff)<<5 ^ uint32(v)
			for i := 0; i < 5; i++ {
				if top>>uint(i)&1 == 1 {
					chk ^= bech32Generator[i]
				}
			}
		}
	}
	return chk
}

// convertBits regroups a byte slice from one bit width to another.
//
// pad is false for decoding, which makes two things errors rather than
// silently-accepted sloppiness: a leftover group of five or more bits, and a
// non-zero padding remainder. Both mean the string encodes something other
// than a whole number of bytes, and an "almost valid" key is not a key.
func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc uint32
	var bits uint
	maxv := uint32(1)<<to - 1
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)

	for _, v := range data {
		if uint32(v)>>from != 0 {
			return nil, fmt.Errorf("%w: a value does not fit in %d bits", errBech32, from)
		}
		acc = acc<<from | uint32(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	switch {
	case pad:
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxv))
		}
	case bits >= from:
		return nil, fmt.Errorf("%w: %d bits are left over, which is a whole group", errBech32, bits)
	case acc<<(to-bits)&maxv != 0:
		return nil, fmt.Errorf("%w: the padding bits are not zero", errBech32)
	}
	return out, nil
}
