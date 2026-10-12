package backup

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// ageVector is age's own published example recipient.
//
// A fixed external vector rather than a round trip through this package's own
// encoder, because a round trip would pass even if the charset table were
// wrong in both directions. The 32 bytes below are what the reference
// implementation decodes it to.
const (
	ageVector    = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
	ageVectorHex = "07e22f5e44a542e8dc8e753a42251e1010cc79d192b3f71c5b1c95645209997a"
)

// TestParseRecipientDecodesAgesOwnPublishedVector.
func TestParseRecipientDecodesAgesOwnPublishedVector(t *testing.T) {
	rec, err := ParseRecipient(ageVector)
	if err != nil {
		t.Fatalf("ParseRecipient: %v", err)
	}
	want, err := hex.DecodeString(ageVectorHex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec.Key[:], want) {
		t.Fatalf("key = %x, want %s", rec.Key[:], ageVectorHex)
	}
	if rec.Short() != "age1ql3z7hjy54pw..." {
		t.Errorf("Short() = %q", rec.Short())
	}
}

// TestBech32DecodesTheBIP173Vector.
//
// The checksum arithmetic is the half of age that can be implemented correctly
// without a cryptographic primitive, so it gets an external vector of its own
// rather than only being exercised through a recipient.
func TestBech32DecodesTheBIP173Vector(t *testing.T) {
	hrp, data, err := decodeBech32("abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw")
	if err != nil {
		t.Fatalf("decodeBech32: %v", err)
	}
	if hrp != "abcdef" {
		t.Errorf("hrp = %q", hrp)
	}
	if got, want := hex.EncodeToString(data), "00443214c74254b635cf84653a56d7c675be77df"; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}

// TestParseRecipientRefusesWhatIsNotARecipient.
//
// A key with a transposed character is a DIFFERENT key, and an archive
// encrypted to it could not be decrypted by anybody -- which would be
// discovered at restore time, which is the worst possible time. The checksum is
// what turns that into a refusal before anything is uploaded.
func TestParseRecipientRefusesWhatIsNotARecipient(t *testing.T) {
	// The vector with its last character changed: a single-character error,
	// which is exactly what bech32's checksum exists to catch.
	broken := ageVector[:len(ageVector)-1] + "q"

	cases := map[string]struct{ in, want string }{
		"empty":                {in: "   ", want: "it is empty"},
		"a one-character typo": {in: broken, want: "the checksum does not match"},
		"mixed case": {
			in:   "Age1QL3Z7HJY54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
			want: "mixes upper and lower case",
		},
		"no separator":                           {in: "agexyz", want: "no separator"},
		"a bech32 character that does not exist": {in: "age1bbbbbbbb", want: "is not a bech32 character"},
		"the wrong prefix": {
			in:   "abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw",
			want: "its prefix is",
		},
		"nothing but a checksum": {in: "age1mrlq0l", want: "the checksum alone is 6"},
		"absurdly long":          {in: "age1" + strings.Repeat("q", 200), want: "the limit is"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRecipient(tc.in)
			if !errors.Is(err, ErrRecipient) {
				t.Fatalf("ParseRecipient(%q) error = %v, want ErrRecipient", tc.in, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestPastingThePrivateHalfIsNamedInTheRefusal.
//
// The mistake somebody will make is pasting AGE-SECRET-KEY-1... into
// encrypt_to, and it is the one mistake that would put a private key in a
// file that section 5 describes as safe to git and safe to paste into a
// support thread. So the refusal says what the string is rather than only that
// it is wrong.
func TestPastingThePrivateHalfIsNamedInTheRefusal(t *testing.T) {
	// A correctly-checksummed bech32 string with age's IDENTITY prefix,
	// lowercased as bech32 requires. The point is the message, not this
	// particular key.
	id := encodeBech32ForTest("age-secret-key-", mustHex(t, ageVectorHex))
	_, err := ParseRecipient(id)
	if err == nil {
		t.Fatal("an AGE-SECRET-KEY was accepted as a recipient")
	}
	if !strings.Contains(err.Error(), "PRIVATE half") {
		t.Errorf("the refusal does not warn about the private key: %v", err)
	}
}

// TestARecipientOfTheWrongLengthIsNotARecipient.
//
// A valid bech32 string with the right prefix and twenty bytes in it is not an
// X25519 key, and accepting it would produce an archive encrypted to nothing.
func TestARecipientOfTheWrongLengthIsNotARecipient(t *testing.T) {
	// Twenty bytes under the `age` prefix. Built by re-checksumming rather
	// than by hand, because a hand-written one would mostly be testing my
	// arithmetic.
	short := encodeBech32ForTest("age", mustHex(t, "00443214c74254b635cf84653a56d7c675be77df"))
	_, err := ParseRecipient(short)
	if !errors.Is(err, ErrRecipient) {
		t.Fatalf("error = %v, want ErrRecipient", err)
	}
	if !strings.Contains(err.Error(), "decodes to 20 bytes") {
		t.Errorf("message = %v", err)
	}
}

// TestAConfiguredRecipientWithNoEncrypterRefusesRatherThanUploadingPlaintext.
//
// This is the security property, stated as a test. `encrypt_to` is a PUBLIC
// key so the VPS can produce backups it cannot itself decrypt -- and that is
// worth exactly as much as the guarantee that it cannot be skipped. Uploading
// the world in the clear to the path somebody configured for encrypted backups
// would be gvm deciding their threat model did not matter.
func TestAConfiguredRecipientWithNoEncrypterRefusesRatherThanUploadingPlaintext(t *testing.T) {
	enc := &AgeEncrypter{Recipients: []Recipient{mustRecipient(t, ageVector)}}
	_, err := enc.Wrap(io.Discard)
	if !errors.Is(err, ErrNoEncrypter) {
		t.Fatalf("Wrap error = %v, want ErrNoEncrypter", err)
	}
	if !strings.Contains(err.Error(), "age1ql3z7hjy54pw...") {
		t.Errorf("the refusal does not name the recipient an operator configured: %v", err)
	}

	// And with no recipients at all it is still a refusal, not a pass-through
	// writer: a nil AgeEncrypter reaching Wrap means a caller lost track of
	// whether encryption was configured.
	if _, err := (&AgeEncrypter{}).Wrap(io.Discard); !errors.Is(err, ErrNoEncrypter) {
		t.Errorf("an empty encrypter's Wrap error = %v, want ErrNoEncrypter", err)
	}
}

// TestNewAgeEncrypterValidatesBeforeItIsEverUsed.
func TestNewAgeEncrypterValidatesBeforeItIsEverUsed(t *testing.T) {
	if _, err := NewAgeEncrypter("not-a-recipient", nil); !errors.Is(err, ErrRecipient) {
		t.Fatalf("error = %v, want ErrRecipient", err)
	}
	enc, err := NewAgeEncrypter(ageVector, func(w io.Writer, to []Recipient) (io.WriteCloser, error) {
		return nopCloser{w}, nil
	})
	if err != nil {
		t.Fatalf("NewAgeEncrypter: %v", err)
	}
	if enc.Describe() != "age1ql3z7hjy54pw..." {
		t.Errorf("Describe() = %q", enc.Describe())
	}
	if _, err := enc.Wrap(io.Discard); err != nil {
		t.Errorf("Wrap with a writer wired: %v", err)
	}
}

// TestTheAgeExtensionIsAgesOwn: `age -d -i key < world.tar.gz.age` is the whole
// of a manual restore, and that only works if the name says what the file is.
func TestTheAgeExtensionIsAgesOwn(t *testing.T) {
	if AgeExt != ".age" {
		t.Fatalf("AgeExt = %q", AgeExt)
	}
}

// --- test helpers ------------------------------------------------------------

// nopCloser makes a writer satisfy io.WriteCloser for the stub encrypter.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func mustRecipient(t *testing.T, s string) Recipient {
	t.Helper()
	r, err := ParseRecipient(s)
	if err != nil {
		t.Fatalf("ParseRecipient(%q): %v", s, err)
	}
	return r
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// encodeBech32ForTest is the inverse of decodeBech32, for building the
// wrong-length and wrong-prefix cases above.
//
// It lives in the test and not in the package because gvm never WRITES a
// recipient: it reads one an operator pasted. A production encoder would be
// code with no caller.
func encodeBech32ForTest(hrp string, data []byte) string {
	five, err := convertBits(data, 8, 5, true)
	if err != nil {
		panic(err)
	}
	// The checksum is polymod over the expanded hrp, the data, and six zeroes,
	// xor'd with 1.
	chk := bech32Polymod(bech32HRPExpand(hrp), five, make([]byte, 6)) ^ 1
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, v := range five {
		sb.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		sb.WriteByte(bech32Charset[(chk>>uint(5*(5-i)))&31])
	}
	return sb.String()
}
