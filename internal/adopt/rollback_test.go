package adopt

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRollbackScriptRunsOnlySystemctl(t *testing.T) {
	script := GenerateRollbackScript("servertest", "zomboid.service",
		"/var/backups/pzserver/adopt-servertest-20240101T000000Z.tar.gz",
		"/home/pzserver/Zomboid",
		"20240101T000000Z",
	)
	if err := ValidateRollbackScript(script); err != nil {
		t.Errorf("rollback script uses disallowed commands: %v", err)
	}
	// Nothing destructive is EXECUTED: every non-comment line is systemctl, echo
	// or set. In particular no line extracts an archive over a live world.
	for _, line := range splitLines(script) {
		s := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(s, "tar ") || s == "tar" || strings.HasPrefix(s, "chown ") {
			t.Errorf("rollback script executes a world-overwriting command: %q", line)
		}
	}
}

// TestValidateRollbackScriptRejectsABareTarLine is the migration-adopt-5 gate:
// a script that would extract an archive when run is refused, so the restore is
// never something gvm (or a generated script) does on its own.
func TestValidateRollbackScriptRejectsABareTarLine(t *testing.T) {
	bad := "#!/bin/sh\nset -e\nsystemctl stop gvm@x.service\ntar -xzf /b/a.tar.gz -C /home/pz/Zomboid\n"
	if err := ValidateRollbackScript(bad); err == nil {
		t.Error("a bare tar line was allowed; gvm must never auto-extract a world")
	}
}

func TestRollbackScriptContainsLegacyUnit(t *testing.T) {
	script := GenerateRollbackScript("servertest", "zomboid.service",
		"/var/backups/pzserver/adopt-servertest-ts.tar.gz",
		"/home/pzserver/Zomboid",
		"ts",
	)
	for _, want := range []string{"zomboid.service", "systemctl enable", "systemctl start", "tar -xzf"} {
		found := false
		for _, line := range splitLines(script) {
			if containsSubstr(line, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("rollback script missing %q", want)
		}
	}
}

// A well-formed, adopted instance whose window to check against.
func okFinalizeInputs(now time.Time, adoptedFrom string) FinalizeInputs {
	return FinalizeInputs{
		Name: "servertest", AdoptedFrom: adoptedFrom, ManagedInstall: false,
		LegacyUnit: "zomboid.service", HaveDropin: true,
		UnitPathFlag: "/etc/systemd/system/zomboid.service", Now: now,
	}
}

func TestCheckFinalizeRefusesWithin7Days(t *testing.T) {
	now := parseRFC3339("2024-06-10T12:00:00Z")
	adoptedAt := parseRFC3339("2024-06-07T12:00:00Z") // 3 days ago
	if err := CheckFinalize(okFinalizeInputs(now, adoptedAt.Format(time.RFC3339))); !isErrFinalizeNotReady(err) {
		t.Errorf("3 days in, want ErrFinalizeNotReady, got: %v", err)
	}
}

func TestCheckFinalizeAllowedAfter7Days(t *testing.T) {
	now := parseRFC3339("2024-06-20T12:00:00Z")
	adoptedAt := parseRFC3339("2024-06-07T12:00:00Z") // 13 days ago
	if err := CheckFinalize(okFinalizeInputs(now, adoptedAt.Format(time.RFC3339))); err != nil {
		t.Errorf("13 days in, want nil, got: %v", err)
	}
}

func TestCheckFinalizeForceBypassesTheWindow(t *testing.T) {
	now := parseRFC3339("2024-06-08T12:00:00Z")
	adoptedAt := parseRFC3339("2024-06-07T12:00:00Z") // 1 day ago
	in := okFinalizeInputs(now, adoptedAt.Format(time.RFC3339))
	in.Force = true
	if err := CheckFinalize(in); err != nil {
		t.Errorf("--force should bypass the window, got: %v", err)
	}
}

// TestCheckFinalizeFailsClosedWithoutATimestamp is the migration-adopt-2 hazard:
// a missing, timestamp-less or corrupt adopted_from must NOT skip the guard.
func TestCheckFinalizeFailsClosedWithoutATimestamp(t *testing.T) {
	now := parseRFC3339("2030-01-01T00:00:00Z")
	for _, af := range []string{"bash-suite:/root/pz-server", "garbage", "zomboid.service@nope"} {
		in := okFinalizeInputs(now, af)
		if err := CheckFinalize(in); !isErrFinalizeNotReady(err) {
			t.Errorf("adopted_from=%q without --force: want ErrFinalizeNotReady, got %v", af, err)
		}
		in.Force = true
		if err := CheckFinalize(in); err != nil {
			t.Errorf("adopted_from=%q with --force: want nil, got %v", af, err)
		}
	}
}

func TestCheckFinalizeRefusesAWrongUnitPathEvenWithForce(t *testing.T) {
	in := okFinalizeInputs(parseRFC3339("2030-01-01T00:00:00Z"), "bash-suite:/root/pz")
	in.Force = true
	in.UnitPathFlag = "/etc/systemd/system/other.service"
	if err := CheckFinalize(in); !errors.Is(err, ErrFinalizeRefused) {
		t.Errorf("a --unit-path that is not the guard's unit: want ErrFinalizeRefused, got %v", err)
	}
}

func TestCheckFinalizeRefusesWithoutADropin(t *testing.T) {
	in := okFinalizeInputs(parseRFC3339("2030-01-01T00:00:00Z"), "bash-suite:/root/pz")
	in.Force = true
	in.HaveDropin, in.LegacyUnit = false, ""
	if err := CheckFinalize(in); !errors.Is(err, ErrFinalizeRefused) {
		t.Errorf("no adopt guard: want ErrFinalizeRefused, got %v", err)
	}
}

func TestCheckFinalizeRefusesANonAdoptedInstance(t *testing.T) {
	in := okFinalizeInputs(parseRFC3339("2030-01-01T00:00:00Z"), "bash-suite:/root/pz")
	in.Force = true
	in.ManagedInstall = true
	if err := CheckFinalize(in); !errors.Is(err, ErrFinalizeRefused) {
		t.Errorf("managed_install=true: want ErrFinalizeRefused, got %v", err)
	}
}

func TestParseAdoptGuardUnit(t *testing.T) {
	dropin := []byte("# comment\n[Unit]\nConflicts=zomboid.service\nAfter=zomboid.service\n")
	if u, ok := ParseAdoptGuardUnit(dropin); !ok || u != "zomboid.service" {
		t.Errorf("ParseAdoptGuardUnit = %q, %v; want zomboid.service, true", u, ok)
	}
	if _, ok := ParseAdoptGuardUnit([]byte("[Unit]\nAfter=x.service\n")); ok {
		t.Error("a guard with no Conflicts= should name no unit")
	}
}

// helpers

func splitLines(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

func parseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func isErrFinalizeNotReady(err error) bool {
	return errors.Is(err, ErrFinalizeNotReady)
}
