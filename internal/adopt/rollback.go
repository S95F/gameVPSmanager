package adopt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GenerateRollbackScript produces a shell script that undoes the cutover: it
// stops the gvm-managed unit and brings the legacy server back, using ONLY
// systemctl. It does NOT extract the world.
//
// Restoring world data overwrites files, and a script that ran `tar -xzf` over
// a live world is one `sh rollback.sh` away from destroying exactly what the
// pre-cutover archive was taken to protect. So the restore is PRINTED for the
// operator to run by hand, deliberately, and ValidateRollbackScript refuses any
// script whose executable lines are not systemctl/echo/set — a bare `tar` line
// is rejected (migration-adopt-5, rule 2).
func GenerateRollbackScript(serverName, legacyUnit, backupPath, dataDir, timestamp string) string {
	return fmt.Sprintf(`#!/bin/sh
# gvm adopt rollback — %s
# Generated: %s
# Depends only on: systemctl
#
# Run this as root to undo the cutover: it stops the gvm-managed unit and brings
# the legacy server back. It NEVER touches the world itself — restoring world
# data overwrites files, so that step is PRINTED below for you to run by hand.
set -e

echo "=== gvm adopt rollback: %s ==="

# Stop and disable the gvm-managed unit.
systemctl disable --now gvm@%s.service 2>/dev/null || true

# Bring the legacy unit back.
systemctl enable %s
systemctl start %s

echo ""
echo "The legacy server is running again on its current world."
echo ""
echo "If the world changed after cutover and you need the pre-cutover copy,"
echo "restore it BY HAND from the verified archive, in this order:"
echo "  1. systemctl stop %s"
echo "  2. move the live world aside (Saves, <srv>_player, db/) to *.after-cutover-%s"
echo "  3. tar -xzf %s -C %s   # strip the archive's own prefix so files land under the data dir"
echo "  4. chown -R the data dir back to the legacy server's user"
echo "  5. systemctl start %s"
echo ""
echo "gvm will not do that for you: it overwrites a world, and that is your call."
echo "=== Rollback complete: legacy server started. ==="
`,
		serverName,
		timestamp,
		serverName,
		serverName,             // disable --now gvm@<n>
		legacyUnit, legacyUnit, // enable, start
		legacyUnit,          // restore step 1: stop
		timestamp,           // restore step 2: suffix
		backupPath, dataDir, // restore step 3: tar -xzf <archive> -C <dataDir>
		legacyUnit, // restore step 5: start
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

// ValidateRollbackScript checks that the script's executable lines are only
// systemctl, echo and set — never tar. gvm never runs this script, but an
// operator will, and a `tar -xzf` line that overwrote a world the moment they
// ran it is the whole reason the restore is printed rather than scripted. A
// bare tar line is rejected; a tar command shown inside an echo (as guidance)
// is fine, because its executable word is echo.
func ValidateRollbackScript(script string) error {
	// Allowed program names at the start of a line (after optional spaces/tabs).
	allowed := map[string]bool{"systemctl": true, "echo": true, "set": true}
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
