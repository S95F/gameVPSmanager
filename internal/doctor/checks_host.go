package doctor

import (
	"context"
	"io/fs"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// HostChecks returns the HOST-* checks that don't require a database.
// HOST-001..005, 011, 012 are added in Phase 14 (they need Postgres).
// HOST-006..008 are added in Phase 15 (they need the identity layer).
//
// unitFS is the embedded filesystem holding the installed unit files (packaging.FS).
func HostChecks(unitFS fs.FS) []lifecycle.DoctorCheck {
	return []lifecycle.DoctorCheck{
		{
			ID:      "HOST-009",
			Summary: "/run/gvm and /run/gvm/web have the correct ownership and mode",
			Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
				return checkRunGvmDirsImpl()
			},
		},
		{
			ID:      "HOST-010",
			Summary: "no game unit references postgresql.service",
			Run:     checkNoUnitDependsOnPostgres(unitFS),
		},
	}
}

// checkNoUnitDependsOnPostgres reads the embedded unit files and fails if any
// game or job unit references postgresql.service. Database failures must never
// stop a game server (D-14).
func checkNoUnitDependsOnPostgres(unitFS fs.FS) func(context.Context, lifecycle.CheckEnv) lifecycle.CheckFinding {
	return func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
		units := []string{
			"systemd/gvm@.service",
			"systemd/gvm-job@.service",
			"systemd/gvm-job@_guard.service",
			"systemd/gvm-job@_verify.service",
		}
		var found []string
		for _, name := range units {
			b, err := fs.ReadFile(unitFS, name)
			if err != nil {
				continue
			}
			// Only check non-comment lines — a comment explaining the ABSENCE of a
			// postgresql dependency must not be flagged.
			for _, line := range strings.Split(string(b), "\n") {
				stripped := strings.TrimSpace(line)
				if strings.HasPrefix(stripped, "#") {
					continue
				}
				if strings.Contains(stripped, "postgresql") {
					found = append(found, name)
					break
				}
			}
		}
		if len(found) == 0 {
			return lifecycle.CheckFinding{
				ID:       "HOST-010",
				Severity: lifecycle.CheckOK,
				Message:  "no game or job unit references postgresql.service",
			}
		}
		return lifecycle.CheckFinding{
			ID:       "HOST-010",
			Severity: lifecycle.CheckProblem,
			Message:  "units reference postgresql.service: " + strings.Join(found, ", "),
			Fix:      "remove postgresql dependency from listed units (D-14)",
		}
	}
}

// InstanceChecks returns INST-* checks for one instance.
func InstanceChecks(inst spec.Instance, manifest *spec.Manifest) []lifecycle.DoctorCheck {
	if manifest == nil {
		return []lifecycle.DoctorCheck{{
			ID:      "INST-001",
			Summary: "manifest pin matches shipped manifest",
			Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
				return lifecycle.CheckFinding{ID: "INST-001", Severity: lifecycle.CheckSkip, Message: "manifest not loaded"}
			},
		}}
	}
	return []lifecycle.DoctorCheck{
		instCheckManifestPin(inst, manifest),
		instCheckMemory(inst, manifest),
		instCheckTimeouts(inst, manifest),
		instCheckSpecApplied(inst),
		instCheckWorkshopTrees(inst, manifest),
		instCheckOrderViolations(inst),
		instCheckSingleGroup(inst),
	}
}
