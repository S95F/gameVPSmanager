package adopt

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/gamecfg"
)

// DiscoverPZ discovers a legacy Project Zomboid server. unitName is the
// bare service name without .service suffix (e.g. "zomboid").
//
// Discovery is strictly read-only: it calls only DiscoverEnv methods, which
// are all read operations.
func DiscoverPZ(env DiscoverEnv, unitName string) (*DiscoverResult, error) {
	r := &DiscoverResult{
		Game:     "projectzomboid",
		UnitName: unitName + ".service",
		Ports:    make(map[string]int),
	}

	// 1. Read and parse the systemd unit file.
	unitPath := "/etc/systemd/system/" + unitName + ".service"
	unitData, err := env.ReadFile(unitPath)
	if err != nil {
		return nil, fmt.Errorf("read unit %s: %w", unitPath, err)
	}
	unitFields := parseUnitFields(unitData)

	r.InstallDir = unitFields["Service.WorkingDirectory"]
	r.User = unitFields["Service.User"]
	r.Group = unitFields["Service.Group"]
	if r.Group == "" {
		r.Group = r.User
	}
	execStart := unitFields["Service.ExecStart"]
	r.ExecStart = execStart
	r.UsesScreen = strings.Contains(execStart, "screen")

	// 2. Read and parse config.env.
	configPath := filepath.Join(r.InstallDir, "config.env")
	configData, err := env.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config.env at %s: %w", configPath, err)
	}
	cfg := ParseConfigEnv(configData)

	if v := cfg["DATA_DIR"]; v != "" {
		r.DataDir = v
	}
	if v := cfg["SERVER_NAME"]; v != "" {
		r.ServerName = v
	}
	r.JVMXmx = cfg["JVM_XMX"]
	r.JVMXms = cfg["JVM_XMS"]

	// 3. Read the server .ini.
	if r.DataDir != "" && r.ServerName != "" {
		iniPath := filepath.Join(r.DataDir, "Server", r.ServerName+".ini")
		iniData, err := env.ReadFile(iniPath)
		if err == nil {
			ini := gamecfg.ParseINI(iniData)
			r.Ports = pzParsePorts(ini)
			r.WorkshopItemCount = countSemicolonList(ini, "WorkshopItems")
			r.ModCount = countSemicolonList(ini, "Mods")
		}
	}

	// 4. Discover workshop mod folders.
	workshopRoot := filepath.Join(r.InstallDir, "steamapps/workshop/content/108600")
	r.AmbiguousMods = pzFindAmbiguousMods(env, workshopRoot)

	// 5. Detect running process.
	if r.User != "" {
		procs, _ := env.UserProcesses(r.User)
		r.PID, r.PIDType = detectPIDType(procs, r.ServerName)
	}

	// 6. Generate findings.
	r.Findings = pzFindings(r, cfg)

	return r, nil
}

// pzParsePorts extracts the known PZ ports from a parsed ini.
func pzParsePorts(ini *gamecfg.INI) map[string]int {
	ports := make(map[string]int)
	for _, kv := range []struct{ key, label string }{
		{"DefaultPort", "game"},
		{"UDPPort", "udp"},
		{"RCONPort", "rcon"},
	} {
		if v, ok := ini.Get(kv.key); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				ports[kv.label] = n
			}
		}
	}
	return ports
}

// countSemicolonList returns the number of non-empty entries in a
// semicolon-separated ini value.
func countSemicolonList(ini *gamecfg.INI, key string) int {
	v, ok := ini.Get(key)
	if !ok || strings.TrimSpace(v) == "" {
		return 0
	}
	count := 0
	for _, part := range strings.Split(v, ";") {
		if strings.TrimSpace(part) != "" {
			count++
		}
	}
	return count
}

// pzFindAmbiguousMods lists workshop items that have more than one mod
// subfolder under workshopRoot/<id>/mods/.
func pzFindAmbiguousMods(env DiscoverEnv, workshopRoot string) []AmbiguousMod {
	items, err := env.ReadDir(workshopRoot)
	if err != nil {
		return nil
	}
	var out []AmbiguousMod
	for _, item := range items {
		if !item.IsDir() {
			continue
		}
		modsDir := filepath.Join(workshopRoot, item.Name(), "mods")
		mods, err := env.ReadDir(modsDir)
		if err != nil {
			continue
		}
		var names []string
		for _, m := range mods {
			if m.IsDir() {
				names = append(names, m.Name())
			}
		}
		if len(names) > 1 {
			out = append(out, AmbiguousMod{WorkshopID: item.Name(), Candidates: names})
		}
	}
	return out
}

// detectPIDType finds the first PZ-related PID in procs and classifies it.
func detectPIDType(procs []ProcInfo, serverName string) (int, string) {
	for _, p := range procs {
		cmd := p.Cmdline
		if strings.Contains(cmd, "screen") && strings.Contains(cmd, serverName) {
			return p.PID, "screen"
		}
		if strings.Contains(cmd, "java") && strings.Contains(cmd, serverName) {
			return p.PID, "jvm"
		}
	}
	return 0, ""
}

// pzFindings generates discovery findings for the result.
func pzFindings(r *DiscoverResult, cfg map[string]string) []DiscoverFinding {
	var f []DiscoverFinding

	// ADOPT-001: Mods= count differs from WorkshopItems= count.
	if r.WorkshopItemCount > 0 && r.ModCount != r.WorkshopItemCount {
		f = append(f, DiscoverFinding{
			Code:     "ADOPT-001",
			Severity: severityError,
			Message:  fmt.Sprintf("Mods= has %d entries but WorkshopItems= has %d", r.ModCount, r.WorkshopItemCount),
		})
	}

	// ADOPT-002: Ambiguous workshop items.
	for _, am := range r.AmbiguousMods {
		f = append(f, DiscoverFinding{
			Code:     "ADOPT-002",
			Severity: severityWarning,
			Message: fmt.Sprintf("workshop item %s has %d mod folder candidates: %s",
				am.WorkshopID, len(am.Candidates), strings.Join(am.Candidates, ", ")),
		})
	}

	// ADOPT-003: Cleartext password.
	if pw := cfg["SERVER_PASSWORD"]; pw != "" {
		f = append(f, DiscoverFinding{
			Code:     "ADOPT-003",
			Severity: severityError,
			Message:  "SERVER_PASSWORD is set in cleartext in config.env",
		})
	}

	// ADOPT-004: screen wrapper detected.
	if r.UsesScreen {
		f = append(f, DiscoverFinding{
			Code:     "ADOPT-004",
			Severity: severityNote,
			Message:  "ExecStart uses screen(1); gvm will run the JVM directly — verify shutdown ladder still fires",
		})
	}

	return f
}

// parseUnitFields extracts key=value pairs from a systemd unit file.
// Keys are prefixed with their section name: "Service.WorkingDirectory".
// Multi-line values joined with backslash continuation are folded.
func parseUnitFields(data []byte) map[string]string {
	out := make(map[string]string)
	section := ""
	continuation := ""
	continuationKey := ""

	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)

		// Active continuation
		if continuationKey != "" {
			// Strip leading whitespace of continuation line
			cont := strings.TrimLeft(rawLine, " \t")
			if strings.HasSuffix(cont, "\\") {
				continuation += " " + strings.TrimSpace(cont[:len(cont)-1])
			} else {
				continuation += " " + strings.TrimSpace(cont)
				out[continuationKey] = strings.TrimSpace(continuation)
				continuationKey = ""
				continuation = ""
			}
			continue
		}

		// Strip comment
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || line == "" {
			continue
		}

		// Section header
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq <= 0 || section == "" {
			continue
		}
		key := section + "." + strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])

		if strings.HasSuffix(val, "\\") {
			continuationKey = key
			continuation = val[:len(val)-1]
			continue
		}
		// For keys that can repeat (ExecStart, ExecStop), keep first.
		if _, exists := out[key]; !exists {
			out[key] = val
		}
	}
	return out
}
