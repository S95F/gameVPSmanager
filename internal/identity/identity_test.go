package identity

import (
	"strings"
	"testing"
	"time"
)

func TestUUIDv7Format(t *testing.T) {
	s, err := UUIDv7String()
	if err != nil {
		t.Fatalf("UUIDv7String: %v", err)
	}
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		t.Errorf("expected 5 parts, got %d: %q", len(parts), s)
	}
	// Version nibble must be 7.
	if len(parts[2]) < 1 || parts[2][0] != '7' {
		t.Errorf("version nibble should be 7, got %q", parts[2])
	}
}

func TestUUIDv7IsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		s, err := UUIDv7String()
		if err != nil {
			t.Fatalf("UUIDv7String: %v", err)
		}
		if seen[s] {
			t.Fatalf("duplicate UUIDv7: %q", s)
		}
		seen[s] = true
	}
}

func TestUUIDv7TimestampIsMonotonic(t *testing.T) {
	// Generate UUIDs across two different milliseconds and verify the later
	// one sorts lexicographically after the earlier one. We can't guarantee
	// order within the same millisecond (random bits), but different-ms UUIDs
	// must be ordered.
	a, _ := UUIDv7String()
	// Sleep long enough to cross a millisecond boundary.
	time.Sleep(2 * time.Millisecond)
	b, _ := UUIDv7String()
	if a >= b {
		t.Errorf("later UUID should sort after earlier one across ms boundary: %q >= %q", b, a)
	}
}

func TestNewAPIToken(t *testing.T) {
	p, err := NewAPIToken()
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	if !strings.HasPrefix(p.Plaintext, "gvmt_") {
		t.Errorf("prefix: %q", p.Plaintext)
	}
	if len(p.SHA256) != 32 {
		t.Errorf("SHA256 len = %d, want 32", len(p.SHA256))
	}
	if p.Prefix != p.Plaintext[:8] {
		t.Errorf("Prefix mismatch")
	}

	// HashToken must match what NewAPIToken stored.
	got := HashToken(p.Plaintext)
	for i := range got {
		if got[i] != p.SHA256[i] {
			t.Error("HashToken result does not match stored SHA256")
			break
		}
	}
}

func TestNewSessionID(t *testing.T) {
	a, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("NewSessionID returned duplicate IDs")
	}
	if len(a) < 20 {
		t.Errorf("session ID too short: %q", a)
	}
}

func TestRecoveryCodes(t *testing.T) {
	plains, hashes, err := GenerateRecoveryCodes(10)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(plains) != 10 || len(hashes) != 10 {
		t.Errorf("expected 10, got %d/%d", len(plains), len(hashes))
	}
	seen := map[string]bool{}
	for _, p := range plains {
		if seen[p] {
			t.Error("duplicate recovery code")
		}
		seen[p] = true
	}
}

func TestTOTPCodeAndVerify(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}

	code, err := TOTPCode(secret)
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	if len(code) != 6 {
		t.Errorf("code length = %d, want 6", len(code))
	}

	step, err := VerifyTOTP(secret, code, 0)
	if err != nil {
		t.Errorf("VerifyTOTP valid code: %v", err)
	}
	if step == 0 {
		t.Error("VerifyTOTP should return non-zero step")
	}
}

func TestTOTPRejectsBadCode(t *testing.T) {
	secret, _ := NewTOTPSecret()
	_, err := VerifyTOTP(secret, "000000", 0)
	if err == nil {
		t.Error("expected rejection of bad code")
	}
}

func TestTOTPRejectsReplayedCode(t *testing.T) {
	secret, _ := NewTOTPSecret()
	code, _ := TOTPCode(secret)
	step, err := VerifyTOTP(secret, code, 0)
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}
	// Replay with lastStep = step must be rejected.
	_, err = VerifyTOTP(secret, code, step)
	if err == nil {
		t.Error("expected replay rejection")
	}
}

func TestBoxSealOpen(t *testing.T) {
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	box, err := NewBox(master)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}

	plain := []byte("my totp secret")
	aad := []byte("user-id-123")

	ct, err := box.Seal("totp", plain, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	got, err := box.Open("totp", ct, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(got) != string(plain) {
		t.Errorf("got %q, want %q", got, plain)
	}
}

func TestBoxWrongAAD(t *testing.T) {
	master := make([]byte, 32)
	box, _ := NewBox(master)
	ct, _ := box.Seal("totp", []byte("secret"), []byte("user-a"))
	_, err := box.Open("totp", ct, []byte("user-b"))
	if err == nil {
		t.Error("expected error with wrong AAD")
	}
}

func TestSealTOTP(t *testing.T) {
	master := make([]byte, 32)
	box, _ := NewBox(master)
	secret, _ := NewTOTPSecret()
	ct, err := box.SealTOTP("user-123", secret)
	if err != nil {
		t.Fatalf("SealTOTP: %v", err)
	}
	got, err := box.OpenTOTP("user-123", ct)
	if err != nil {
		t.Fatalf("OpenTOTP: %v", err)
	}
	if got != secret {
		t.Errorf("got %q, want %q", got, secret)
	}
}
