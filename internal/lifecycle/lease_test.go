package lifecycle

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLeaseRoundTrip(t *testing.T) {
	want := Lease{
		Instance: "pz-main", Kind: JobUpdate, JobID: 4471,
		Unit: "gvm-job@pz-main_nightly.service", PID: 41799,
		BootID:    "5f1c0b3e-7a24-4b8e-9c31-2d8e6a4f0b17",
		StartedAt: at("2026-09-04T18:55:00Z"),
		Deadline:  at("2026-09-04T22:55:00Z"),
		Reason:    "steam build 19487321",
	}
	b, err := want.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLease(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	want.Proto = LeaseProto
	if *got != want {
		t.Fatalf("round trip changed the lease:\n got %+v\nwant %+v", *got, want)
	}
}

// TestALeaseThatDoesNotParseIsNotNoLease.
//
// Every refusal in ParseLease exists because the alternative is to read the
// file as absent, and a lease read as absent lets a second updater start.
func TestALeaseThatDoesNotParseIsNotNoLease(t *testing.T) {
	bad := map[string]string{
		"not json":        `{ this is not json`,
		"empty":           ``,
		"a future schema": `{"proto":2,"kind":"update","started_at":"2026-09-04T18:55:00Z"}`,
		"no proto":        `{"kind":"update","started_at":"2026-09-04T18:55:00Z"}`,
		"an unknown kind": `{"proto":1,"kind":"defrag","started_at":"2026-09-04T18:55:00Z"}`,
		"no started_at":   `{"proto":1,"kind":"update"}`,
	}
	for name, body := range bad {
		l, err := ParseLease(strings.NewReader(body))
		if err == nil {
			t.Errorf("%s: parsed as %+v; an unreadable lease must be refused, not read as absent", name, l)
			continue
		}
		if !errors.Is(err, ErrLease) {
			t.Errorf("%s: %v does not wrap ErrLease, so a caller cannot tell it apart", name, err)
		}
	}
}

// TestClassifyRequiresEvidenceOfDeath is the asymmetry, as a table.
//
// Read the want column: every row that says stale had PROOF the holder is
// gone. Every row that could not establish it says live, and that is the safe
// direction -- an operator running `gvm job clear` on a lease that was
// actually dead loses nothing, while a second updater against a live install
// tree loses a world.
func TestClassifyRequiresEvidenceOfDeath(t *testing.T) {
	now := at("2026-09-04T19:00:00Z")
	const boot = "5f1c0b3e-7a24-4b8e-9c31-2d8e6a4f0b17"
	started := at("2026-09-04T18:55:00Z")

	tests := []struct {
		name  string
		lease *Lease
		boot  string
		alive Tri
		want  LeaseState
	}{
		{"no lease", nil, boot, Unknown, LeaseNone},
		{
			"the holder is running", &Lease{Kind: JobUpdate, PID: 900, BootID: boot, StartedAt: started},
			boot, Yes, LeaseLive,
		},
		{
			"the holder is gone", &Lease{Kind: JobUpdate, PID: 900, BootID: boot, StartedAt: started},
			boot, No, LeaseStale,
		},
		{
			// The box rebooted mid-update. /run went with it and
			// /var/lib/gvm did not, which is precisely why the lease lives
			// under /var/lib.
			"a lease from a previous boot", &Lease{Kind: JobUpdate, PID: 900, BootID: "older-boot", StartedAt: started},
			boot, Yes, LeaseStale,
		},
		{
			// Nobody could look at the process table. Live is the answer that
			// does not corrupt anything.
			"liveness could not be established", &Lease{Kind: JobUpdate, PID: 900, StartedAt: started},
			"", Unknown, LeaseLive,
		},
		{
			"past its deadline with no liveness signal",
			&Lease{Kind: JobUpdate, PID: 900, StartedAt: started, Deadline: at("2026-09-04T18:59:00Z")},
			"", Unknown, LeaseStale,
		},
		{
			// The deadline is a backstop for the unknown case only. A job
			// that is genuinely still running past its budget is still
			// running, and killing it is worse than waiting.
			"past its deadline and demonstrably alive",
			&Lease{Kind: JobUpdate, PID: 900, StartedAt: started, Deadline: at("2026-09-04T18:59:00Z")},
			boot, Yes, LeaseLive,
		},
		{
			"an old lease whose holder is alive",
			&Lease{Kind: JobUpdate, PID: 900, BootID: boot, StartedAt: at("2026-09-01T00:00:00Z")},
			boot, Yes, LeaseLive,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lease.Classify(now, tc.boot, tc.alive); got != tc.want {
				t.Fatalf("Classify = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestJobKindPhaseCoversTheWholeEnum(t *testing.T) {
	// Every kind must have a decided answer, and the mutating set must be
	// exactly the jobs that rewrite the install tree or the world. A kind
	// added to the enum and forgotten here silently becomes "harmless".
	mutating := map[JobKind]Phase{
		JobInstall:     PhaseInstalling,
		JobUpdate:      PhaseUpdating,
		JobRebuild:     PhaseUpdating,
		JobRestore:     PhaseUpdating,
		JobAdopt:       PhaseUpdating,
		JobPromote:     PhaseUpdating,
		JobContentSync: PhaseUpdating,
	}
	for _, k := range JobKinds() {
		got, masks := k.Phase()
		want, shouldMask := mutating[k]
		if masks != shouldMask {
			t.Errorf("%s masks=%v, want %v", k, masks, shouldMask)
		}
		if masks && got != want {
			t.Errorf("%s -> %s, want %s", k, got, want)
		}
	}
	if JobKind("defrag").Valid() {
		t.Error("an undeclared kind validated")
	}
}

func TestHolderLineRoundTrips(t *testing.T) {
	// The format is D-30's, and started_at is last because RFC3339 has colons
	// in it -- so the parser splits from the left exactly three times.
	want := Holder{
		PID: 41799, Unit: "gvm-job@pz-main_nightly.service", Kind: "update",
		StartedAt: at("2026-09-04T18:55:00Z"),
	}
	line := want.Line()
	if want, got := "41799:gvm-job@pz-main_nightly.service:update:2026-09-04T18:55:00Z", line; got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
	got, err := ParseHolder(strings.NewReader(line + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ParseHolder = %+v, want %+v", got, want)
	}
}

func TestParseHolderRefusesWhatItCannotName(t *testing.T) {
	for _, bad := range []string{
		"", "held", "0:u:k:2026-09-04T18:55:00Z", "abc:u:k:2026-09-04T18:55:00Z",
		"41799:u:k:yesterday", "41799:u:k",
	} {
		if h, err := ParseHolder(strings.NewReader(bad + "\n")); err == nil {
			t.Errorf("%q parsed as %+v; a lock whose holder cannot be named must say so", bad, h)
		} else if !errors.Is(err, ErrHolder) {
			t.Errorf("%q: %v does not wrap ErrHolder", bad, err)
		}
	}
}

func TestLeaseDescribeNamesTheHolderAndTheAge(t *testing.T) {
	l := &Lease{
		Kind: JobUpdate, Unit: "gvm-job@pz-main_nightly.service",
		StartedAt: at("2026-09-04T18:55:00Z"), Reason: "steam build 19487321",
	}
	got := l.Describe(at("2026-09-04T19:00:00Z"))
	for _, want := range []string{"update", "gvm-job@pz-main_nightly.service", "5m0s", "steam build"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe = %q, missing %q", got, want)
		}
	}
	if (*Lease)(nil).Describe(time.Now()) != "" {
		t.Error("a nil lease described itself")
	}
}
