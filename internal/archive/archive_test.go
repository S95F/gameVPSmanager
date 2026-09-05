package archive

import (
	"testing"
	"time"
)

func TestRefIsNotAReceiptUntilItIsVerified(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		ref  *Ref
		want bool
	}{
		{"nil", nil, false},
		{"zero", &Ref{}, false},
		{"written but never verified", &Ref{Path: "/var/backups/gvm/pz-main/x.tar.gz", CreatedAt: now}, false},
		{"verified but pathless", &Ref{VerifiedAt: now}, false},
		{"verified", &Ref{Path: "/var/backups/gvm/pz-main/x.tar.gz", CreatedAt: now, VerifiedAt: now}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.Verified(); got != tc.want {
				t.Errorf("Verified() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsPreserveIsFalseForANilRef(t *testing.T) {
	var r *Ref
	if r.IsPreserve() {
		t.Fatal("a nil Ref reported itself as preserve; retention would skip a file that does not exist")
	}
	if (&Ref{Class: ClassRoutine}).IsPreserve() {
		t.Fatal("a routine archive reported itself as preserve")
	}
	if !(&Ref{Class: ClassPreserve}).IsPreserve() {
		t.Fatal("a preserve archive did not report itself as preserve; prune would eat a pre-rebuild world")
	}
}
