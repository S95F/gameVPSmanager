package instance

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestValidateNameAcceptsWhatD45Describes(t *testing.T) {
	ok := []string{
		"pz",                            // the two-character floor
		"pz-main",                       // the name the whole document uses
		"a1",                            // digits are fine anywhere
		"7dtd",                          // and may lead
		"0",                             // ...but one character is not a name
		strings.Repeat("a", MaxNameLen), // the 28-character ceiling
	}
	for _, s := range ok {
		err := ValidateName(s)
		if s == "0" {
			if err == nil {
				t.Errorf("ValidateName(%q) accepted a one-character name", s)
			}
			continue
		}
		if err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", s, err)
		}
	}
}

func TestValidateNameRejectsEachClauseSeparately(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // a fragment of the diagnostic, so the message stays useful
	}{
		{"empty", "", "required"},
		{"one character", "a", "names are"},
		{"29 characters", strings.Repeat("a", MaxNameLen+1), "useradd"},
		{"uppercase", "PZ-main", "lowercase"},
		{"underscore", "pz_main", "lowercase"},
		{"leading dash", "-pz", "lowercase"},
		{"trailing dash", "pz-", "lowercase"},
		{"dot", "pz.main", "lowercase"},
		{"slash", "pz/main", "lowercase"},
		{"space", "pz main", "lowercase"},
		{"at sign", "pz@main", "lowercase"},
		{"percent", "pz%i", "lowercase"},
		{"non-ascii", "pz-mäin", "lowercase"},
		{"double dash", "pz--main", `"--"`},
		{"double dash at the end", "a--b", `"--"`},
		{"reserved word", "all", "reserved"},
		{"reserved account", "console", "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.in)
			if err == nil {
				t.Fatalf("ValidateName(%q) = nil, want an error", tc.in)
			}
			if !errors.Is(err, ErrName) {
				t.Errorf("error does not wrap ErrName: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ValidateName(%q) = %q, which does not explain %q", tc.in, err, tc.want)
			}
		})
	}
}

// The length message is worth its own assertion: 28 is not an arbitrary
// number, and an operator told only "too long" will try 30.
func TestTheLengthCeilingIsWhatUseraddCanHold(t *testing.T) {
	longest := strings.Repeat("a", MaxNameLen)
	if err := ValidateName(longest); err != nil {
		t.Fatalf("the documented ceiling is not accepted: %v", err)
	}
	if got := len(UserName(longest)); got != 32 {
		t.Fatalf("UserName(%d chars) is %d characters; useradd caps at 32, which is the whole "+
			"reason MaxNameLen is %d", MaxNameLen, got, MaxNameLen)
	}
}

// D-45's own list, transcribed. A future edit that drops one of these has to
// notice it is doing so.
func TestEveryReservedWordD45NamesIsStillReserved(t *testing.T) {
	d45 := []string{
		"all", "none", "web", "gvmd", "run",
		"job", "jobs", "host", "default", "system",
		"guard", "verify",
	}
	for _, s := range d45 {
		if err := ValidateName(s); err == nil {
			t.Errorf("%q is reserved by D-45 and was accepted", s)
		}
	}
}

// Section 6.4 makes an instance's account gvm-<name>. These four names would
// therefore ask for an account or group gvm already owns -- gvm-console gates
// every console socket on the box (D-10) -- and D-45's list does not cover
// them, because it enumerated units and verbs rather than accounts.
func TestNamesThatWouldCollideWithGvmsOwnAccountsAreReserved(t *testing.T) {
	for _, s := range []string{"web", "fetch", "console", "content"} {
		err := ValidateName(s)
		if err == nil {
			t.Fatalf("instance %q would take the account %s, which gvm already owns", s, UserName(s))
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("ValidateName(%q) = %q, want a reserved-word diagnostic", s, err)
		}
	}
}

func TestEveryReservedNameCarriesItsReason(t *testing.T) {
	for name, why := range ReservedNames() {
		if strings.TrimSpace(why) == "" {
			t.Errorf("%q is reserved with no reason, so the operator gets no next move", name)
		}
		err := ValidateName(name)
		if err == nil {
			t.Errorf("%q is listed as reserved and was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), why) {
			t.Errorf("ValidateName(%q) does not carry its reason %q: %v", name, why, err)
		}
	}
}

// ReservedNames must hand out a copy: a caller that could mutate the map could
// un-reserve a name for the whole process.
func TestReservedNamesIsACopy(t *testing.T) {
	m := ReservedNames()
	delete(m, "all")
	if err := ValidateName("all"); err == nil {
		t.Fatal("deleting from the returned map un-reserved a name")
	}
}

// systemd escapes any character outside [a-zA-Z0-9:_.-] in a unit-instance
// name, which makes %i and %I differ and drags escaping into every unit file,
// drop-in path and journalctl invocation. D-45's charset is chosen so that
// never happens; this is the assertion that it still holds.
func TestAnAcceptedNameNeedsNoSystemdEscaping(t *testing.T) {
	plain := regexp.MustCompile(`^[a-z0-9-]+$`)
	for _, s := range []string{"pz", "pz-main", "a1b2", "7dtd-pvp", strings.Repeat("z", MaxNameLen)} {
		if err := ValidateName(s); err != nil {
			t.Fatalf("fixture %q is not a valid name: %v", s, err)
		}
		if !plain.MatchString(s) {
			t.Errorf("%q would be escaped by systemd, so %%i and %%I stop being byte-identical", s)
		}
	}
}

func TestDerivedNames(t *testing.T) {
	const n = "pz-main"
	cases := []struct{ got, want string }{
		{UnitName(n), "gvm@pz-main.service"},
		{UserName(n), "gvm-pz-main"},
		{GroupName(n), "gvm-pz-main"},
		{DropInPath(n), "/etc/systemd/system/gvm@pz-main.service.d/10-gvm.conf"},
		{JobSlug(n, "nightly"), "pz-main_nightly"},
		{JobUnit(n, "nightly"), "gvm-job@pz-main_nightly.service"},
		{JobTimer(n, "nightly"), "gvm-job@pz-main_nightly.timer"},
		{JobTimerPath(n, "nightly"), "/etc/systemd/system/gvm-job@pz-main_nightly.timer"},
		{JobSlug("", "guard"), "_guard"},
		{JobUnit("", "verify"), "gvm-job@_verify.service"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// Every derived name is a real systemd unit name or a real unix account name.
// A caller that skipped ValidateName is addressing something gvm did not
// intend, and possibly something that exists, so the failure is loud.
func TestDerivedNamesPanicOnAnUnvalidatedName(t *testing.T) {
	cases := map[string]func(){
		"UnitName":   func() { UnitName("../etc") },
		"UserName":   func() { UserName("pz_main") },
		"DropInPath": func() { DropInPath("pz main") },
		"JobSlug":    func() { JobSlug("pz-main", "night_ly") },
		"JobUnit":    func() { JobUnit("PZ", "nightly") },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s accepted an invalid name and produced one anyway", name)
				}
			}()
			fn()
		})
	}
}

func TestSplitJobSlugRoundTrips(t *testing.T) {
	cases := []struct{ inst, job string }{
		{"pz-main", "nightly"},
		{"pz-main", "backup"},
		{"a1", "restart"},
		{"", "guard"},  // host-level (D-13)
		{"", "verify"}, // host-level (D-13)
	}
	for _, tc := range cases {
		slug := JobSlug(tc.inst, tc.job)
		inst, job, err := SplitJobSlug(slug)
		if err != nil {
			t.Errorf("SplitJobSlug(%q) = %v", slug, err)
			continue
		}
		if inst != tc.inst || job != tc.job {
			t.Errorf("SplitJobSlug(%q) = (%q, %q), want (%q, %q)", slug, inst, job, tc.inst, tc.job)
		}
	}
}

func TestSplitJobSlugRejects(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no separator", "pzmain", "no '_'"},
		{"two separators", "pz_main_nightly", "more than one"},
		{"empty job", "pz-main_", "required"},
		{"both empty", "_", "required"},
		{"invalid instance", "PZ_nightly", "lowercase"},
		{"invalid job", "pz-main_Nightly", "lowercase"},
		{"reserved job is fine as an instance word", "all_nightly", "reserved"},
		{"empty", "", "no '_'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := SplitJobSlug(tc.in)
			if err == nil {
				t.Fatalf("SplitJobSlug(%q) = nil error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("SplitJobSlug(%q) = %q, which does not explain %q", tc.in, err, tc.want)
			}
		})
	}
}

// The reserved list belongs to instance names, not to job names: "guard" and
// "verify" are reserved as instances precisely BECAUSE D-13 spends them on
// host-level job slugs. Sharing one list between the two would make _guard and
// _verify unconstructible, which is the exact failure this test caught when
// ValidateJobName delegated to ValidateName.
func TestAJobMayBeCalledGuardEvenThoughAnInstanceMayNot(t *testing.T) {
	for _, job := range []string{"guard", "verify", "all", "none", "host", "run", "web"} {
		if err := ValidateJobName(job); err != nil {
			t.Errorf("ValidateJobName(%q) = %v; a job name becomes half of a slug, not a unit "+
				"or an account, so the reserved list does not apply to it", job, err)
		}
		if err := ValidateName(job); err == nil {
			t.Errorf("%q is accepted as an instance name; the two lists have drifted", job)
		}
	}
}

// The separator only works because neither half can contain one. If the name
// charset ever gains '_', this is the test that says what breaks.
func TestNoValidNameContainsTheJobSeparator(t *testing.T) {
	for _, s := range []string{"pz_main", "_", "a_", "_b"} {
		if err := ValidateName(s); err == nil {
			t.Fatalf("%q is a valid name, so '_' is no longer an unambiguous job separator (D-13)", s)
		}
	}
}
