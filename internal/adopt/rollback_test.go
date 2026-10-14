package adopt

import (
	"errors"
	"testing"
	"time"
)

func TestRollbackScriptDependsOnlyOnSystemctlAndTar(t *testing.T) {
	script := GenerateRollbackScript("servertest", "zomboid.service",
		"/var/backups/pzserver/adopt-servertest-20240101T000000Z.tar.gz",
		"/home/pzserver/Zomboid",
		"20240101T000000Z",
	)
	if err := ValidateRollbackScript(script); err != nil {
		t.Errorf("rollback script uses disallowed commands: %v", err)
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

func TestFinalizeRefusesWithin7Days(t *testing.T) {
	now := parseRFC3339("2024-06-10T12:00:00Z")
	adoptedAt := parseRFC3339("2024-06-07T12:00:00Z") // 3 days ago
	adoptedFrom := "zomboid.service@" + adoptedAt.Format("2006-01-02T15:04:05Z07:00")

	err := FinalizePZ("servertest", "/etc/systemd/system/zomboid.service", adoptedFrom,
		FinalizeOptions{Now: now})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isErrFinalizeNotReady(err) {
		t.Errorf("expected ErrFinalizeNotReady, got: %v", err)
	}
}

func TestFinalizeAllowedAfter7Days(t *testing.T) {
	// Without an actual unit file present, it should fail on os.ReadFile — but
	// NOT on the 7-day guard.
	now := parseRFC3339("2024-06-20T12:00:00Z")
	adoptedAt := parseRFC3339("2024-06-07T12:00:00Z") // 13 days ago
	adoptedFrom := "zomboid.service@" + adoptedAt.Format("2006-01-02T15:04:05Z07:00")

	err := FinalizePZ("servertest", "/nonexistent/path/zomboid.service", adoptedFrom,
		FinalizeOptions{Now: now})
	// Should get a file-not-found error, not ErrFinalizeNotReady.
	if isErrFinalizeNotReady(err) {
		t.Errorf("should not get ErrFinalizeNotReady after 7 days, got: %v", err)
	}
}

func TestFinalizeForceBypassesWindow(t *testing.T) {
	now := parseRFC3339("2024-06-08T12:00:00Z")
	adoptedAt := parseRFC3339("2024-06-07T12:00:00Z") // 1 day ago
	adoptedFrom := "zomboid.service@" + adoptedAt.Format("2006-01-02T15:04:05Z07:00")

	err := FinalizePZ("servertest", "/nonexistent/path/zomboid.service", adoptedFrom,
		FinalizeOptions{Now: now, Force: true})
	// With Force, the 7-day guard must not fire (may get file-not-found though).
	if isErrFinalizeNotReady(err) {
		t.Errorf("--force should bypass 7-day guard, got: %v", err)
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
