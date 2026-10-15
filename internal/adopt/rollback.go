package adopt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GenerateRollbackScript produces a shell script that re-enables the legacy
// server and restores the pre-cutover backup. The script depends ONLY on
// systemctl and tar (asserted by TestRollbackScriptDependsOnlyOnSystemctlAndTar).
func GenerateRollbackScript(serverName, legacyUnit, backupPath, dataDir, timestamp string) string {
	return fmt.Sprintf(`#!/bin/sh
# gvm adopt rollback — %s
# Generated: %s
# Depends only on: systemctl, tar
#
# Run this script as root to undo the cutover and restore the legacy server.
# It will:
#   1. Stop the gvm-managed unit (if running).
#   2. Re-enable and start the legacy unit.
#   3. Restore world data from the pre-cutover backup.
set -e

echo "=== gvm adopt rollback: %s ==="

# Step 1: Stop gvm unit.
systemctl stop gvm@%s.service 2>/dev/null || true

# Step 2: Restore legacy unit.
systemctl enable %s
systemctl start %s

# Step 3: Restore world backup.
echo "Restoring world data from: %s"
tar -xzf %s -C %s

echo "=== Rollback complete. Legacy server started. ==="
echo "Verify with: systemctl status %s"
`,
		serverName,
		timestamp,
		serverName,
		serverName,
		legacyUnit, legacyUnit,
		backupPath, backupPath, filepath.Dir(dataDir),
		legacyUnit,
	)
}

// listSaveFiles walks dataDir and returns relative paths of all regular files
// under the Saves subdirectory.
func listSaveFiles(dataDir string) ([]string, error) {
	savesDir := filepath.Join(dataDir, "Saves")
	var files []string
	err := filepath.WalkDir(savesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(savesDir, path)
			files = append(files, rel)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return files, err
}

// runSystemctl executes a systemctl command. This is the ONLY place gvm
// calls systemctl during cutover; the rollback script also uses it directly.
func runSystemctl(args ...string) error {
	return runProgram("systemctl", args...)
}

// runTar creates a gzip-compressed tar archive of src at dst.
func runTar(src, dst string) error {
	return runProgram("tar", "-czf", dst, "-C", filepath.Dir(src), filepath.Base(src))
}

// ValidateRollbackScript checks that the script contains no commands other
// than systemctl and tar. Used by TestRollbackScriptDependsOnlyOnSystemctlAndTar.
func ValidateRollbackScript(script string) error {
	// Allowed program names at the start of a line (after optional spaces/tabs).
	allowed := map[string]bool{"systemctl": true, "tar": true, "echo": true, "set": true}
	for _, line := range strings.Split(script, "\n") {
		stripped := strings.TrimLeft(line, " \t")
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		// Get the first word (the command).
		fields := strings.Fields(stripped)
		if len(fields) == 0 {
			continue
		}
		cmd := fields[0]
		// Allow shell builtins and variable assignments.
		if strings.Contains(cmd, "=") || cmd == "true" || cmd == "false" || cmd == "exit" {
			continue
		}
		if !allowed[cmd] {
			return fmt.Errorf("rollback script: disallowed command %q on line: %q", cmd, line)
		}
	}
	return nil
}

// adoptedAt reads the adoption timestamp from the instance metadata.
// Returns zero time if the instance has not been adopted.
func adoptedAtTime(adoptedFrom string) time.Time {
	s := strings.TrimSpace(adoptedFrom)
	if s == "" {
		return time.Time{}
	}
	// A bare RFC3339 timestamp is one documented form.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	// "<unit>@<RFC3339 timestamp>" is the other.
	if i := strings.LastIndex(s, "@"); i >= 0 {
		if t, err := time.Parse(time.RFC3339, s[i+1:]); err == nil {
			return t
		}
	}
	// Anything else — the "bash-suite:/root/pz-server" form, or a corrupt
	// value — carries no timestamp, and finalize treats that as not-ready.
	return time.Time{}
}
