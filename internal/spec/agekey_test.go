package spec

import (
	"strings"
	"testing"
)

const sampleAgeIdentity = "AGE-SECRET-KEY-1QQPQZRFR7ZZ2WCVQYE2WEXHZW6MM3FK3RT8T8FR0WX8U9YQ0X3ZS8ABCD"

func TestContainsAgeSecretKey(t *testing.T) {
	yes := []byte("backup:\n  remote:\n    recipient: " + sampleAgeIdentity + "\n")
	if !ContainsAgeSecretKey(yes) {
		t.Error("an age identity was not detected")
	}
	// The public recipient (age1…) is the right thing and must NOT trip it.
	no := []byte("backup:\n  remote:\n    recipient: age1qqpqzrfr7zz2wcvqye2wexhzw6\n")
	if ContainsAgeSecretKey(no) {
		t.Error("a public recipient (age1…) was wrongly flagged as an identity")
	}
}

func TestRedactAgeSecretKeys(t *testing.T) {
	raw := []byte("a: " + sampleAgeIdentity + "\nb: " + sampleAgeIdentity + "\n")
	out, n := RedactAgeSecretKeys(raw)
	if n != 2 {
		t.Errorf("redacted %d, want 2", n)
	}
	if strings.Contains(string(out), sampleAgeIdentity) {
		t.Errorf("the identity survived redaction:\n%s", out)
	}
	if !strings.Contains(string(out), "AGE-SECRET-KEY-1(redacted)") {
		t.Errorf("the redaction marker is missing:\n%s", out)
	}
	if ContainsAgeSecretKey(out) {
		t.Error("ContainsAgeSecretKey still matches after redaction")
	}
}
