package adopt

import (
	"fmt"
	"strconv"
	"strings"
)

// DiscoverMC discovers a legacy Minecraft server (vanilla, Paper, Forge, or
// NeoForge) from a systemd unit file. unitName is the bare service name without
// the .service suffix (e.g. "minecraft").
//
// Discovery is strictly read-only: it calls only DiscoverEnv methods.
func DiscoverMC(env DiscoverEnv, unitName string) (*DiscoverResult, error) {
	r := &DiscoverResult{
		Game:     "minecraft",
		UnitName: unitName + ".service",
		Ports:    make(map[string]int),
	}

	// Read the systemd unit.
	unitPath := "/etc/systemd/system/" + unitName + ".service"
	unitData, err := env.ReadFile(unitPath)
	if err != nil {
		return nil, fmt.Errorf("read unit %s: %w", unitPath, err)
	}
	fields := parseUnitFields(unitData)

	r.InstallDir = fields["Service.WorkingDirectory"]
	r.User = fields["Service.User"]
	r.Group = fields["Service.Group"]
	if r.Group == "" {
		r.Group = r.User
	}
	r.ExecStart = fields["Service.ExecStart"]

	// Detect JVM heap size from ExecStart.
	r.JVMXmx = parseXmx(r.ExecStart)
	r.JVMXms = parseXms(r.ExecStart)

	// Detect loader kind from the ExecStart command.
	r.Findings = append(r.Findings, mcFindings(r)...)

	return r, nil
}

// mcFindings produces discovery findings for a Minecraft server.
func mcFindings(r *DiscoverResult) []DiscoverFinding {
	var out []DiscoverFinding
	if r.InstallDir == "" {
		out = append(out, DiscoverFinding{
			Code:     "MC-001",
			Severity: severityError,
			Message:  "WorkingDirectory not found in unit; cannot determine install directory",
		})
	}
	if r.User == "" {
		out = append(out, DiscoverFinding{
			Code:     "MC-002",
			Severity: severityWarning,
			Message:  "User not found in unit; gvm will use the current user",
		})
	}
	if r.JVMXmx == "" {
		out = append(out, DiscoverFinding{
			Code:     "MC-003",
			Severity: severityNote,
			Message:  "-Xmx not found in ExecStart; heap_size will use the manifest default",
		})
	}
	return out
}

// parseXmx extracts -Xmx<size> from a java command line.
func parseXmx(execStart string) string {
	return parseJVMFlag(execStart, "-Xmx")
}

// parseXms extracts -Xms<size> from a java command line.
func parseXms(execStart string) string {
	return parseJVMFlag(execStart, "-Xms")
}

// parseJVMFlag extracts a JVM flag value from a command line string.
func parseJVMFlag(cmdline, prefix string) string {
	for _, tok := range strings.Fields(cmdline) {
		if strings.HasPrefix(tok, prefix) {
			return tok[len(prefix):]
		}
	}
	return ""
}

// mcDetectLoader returns the loader kind (vanilla, paper, forge, neoforge)
// by inspecting the executable name in the ExecStart line.
func mcDetectLoader(execStart string) string {
	lower := strings.ToLower(execStart)
	switch {
	case strings.Contains(lower, "paperclip"):
		return "paper"
	case strings.Contains(lower, "neoforge"):
		return "neoforge"
	case strings.Contains(lower, "forge"):
		return "forge"
	case strings.Contains(lower, "fabric"):
		return "fabric"
	default:
		return "vanilla"
	}
}

// mcParseServerPort attempts to read server-port from server.properties.
func mcParseServerPort(env DiscoverEnv, dataDir string) int {
	if dataDir == "" {
		return 0
	}
	propPath := dataDir + "/server.properties"
	data, err := env.ReadFile(propPath)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			if strings.TrimSpace(k) == "server-port" {
				if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					return p
				}
			}
		}
	}
	return 0
}
