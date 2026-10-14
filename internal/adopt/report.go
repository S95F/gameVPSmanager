package adopt

import (
	"fmt"
	"strings"
)

// FormatDiscoverResult formats a DiscoverResult as a human-readable report.
// The output is deterministic and used for golden-file comparison.
func FormatDiscoverResult(r *DiscoverResult) string {
	var b strings.Builder

	title := fmt.Sprintf("gvm adopt discover — Project Zomboid · %s", r.ServerName)
	b.WriteString(title + "\n")
	b.WriteString(strings.Repeat("─", len([]rune(title))) + "\n")
	b.WriteString("\n")

	b.WriteString(fmt.Sprintf("  service   %s\n", r.UnitName))
	b.WriteString(fmt.Sprintf("  install   %s\n", r.InstallDir))
	b.WriteString(fmt.Sprintf("  data      %s\n", r.DataDir))
	b.WriteString(fmt.Sprintf("  user      %s\n", r.User))
	if r.PID > 0 {
		b.WriteString(fmt.Sprintf("  pid       running (%s, pid %d)\n", r.PIDType, r.PID))
	} else {
		b.WriteString("  pid       not running\n")
	}
	b.WriteString("\n")

	b.WriteString("  ports\n")
	for _, kv := range []string{"game", "udp", "rcon"} {
		if p, ok := r.Ports[kv]; ok {
			b.WriteString(fmt.Sprintf("    %-6s %d\n", kv, p))
		}
	}
	b.WriteString("\n")

	b.WriteString("  content\n")
	b.WriteString(fmt.Sprintf("    WorkshopItems   %d\n", r.WorkshopItemCount))
	b.WriteString(fmt.Sprintf("    Mods            %d\n", r.ModCount))
	b.WriteString("\n")

	if len(r.Findings) > 0 {
		b.WriteString("  findings\n")
		for _, f := range r.Findings {
			glyph := findingGlyph(f.Severity)
			b.WriteString(fmt.Sprintf("    %s  %-10s  %s\n", glyph, f.Code, f.Message))
		}
		b.WriteString("\n")
	}

	// Summary line.
	errors, warnings, notes := countFindingSeverities(r.Findings)
	if len(r.Findings) == 0 {
		b.WriteString("  no findings\n")
	} else {
		parts := []string{}
		if errors > 0 {
			parts = append(parts, fmt.Sprintf("%d error", errors))
			if errors > 1 {
				parts[len(parts)-1] += "s"
			}
		}
		if warnings > 0 {
			parts = append(parts, fmt.Sprintf("%d warning", warnings))
			if warnings > 1 {
				parts[len(parts)-1] += "s"
			}
		}
		if notes > 0 {
			parts = append(parts, fmt.Sprintf("%d note", notes))
			if notes > 1 {
				parts[len(parts)-1] += "s"
			}
		}
		b.WriteString("  " + strings.Join(parts, " · ") + "\n")
	}

	return b.String()
}

func findingGlyph(severity string) string {
	switch severity {
	case severityError:
		return "✗"
	case severityWarning:
		return "!"
	case severityNote:
		return "·"
	default:
		return "?"
	}
}

func countFindingSeverities(findings []DiscoverFinding) (errors, warnings, notes int) {
	for _, f := range findings {
		switch f.Severity {
		case severityError:
			errors++
		case severityWarning:
			warnings++
		case severityNote:
			notes++
		}
	}
	return
}
