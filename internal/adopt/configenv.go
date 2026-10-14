package adopt

import (
	"bufio"
	"bytes"
	"strings"
)

// ParseConfigEnv parses a bash-style KEY=VALUE environment file WITHOUT
// executing any shell substitution. Command substitution ($(...) and
// backticks), variable interpolation (${VAR} and $VAR), and all other
// metacharacters are returned as literal strings.
//
// This is intentional: sourcing the file would let the legacy operator script
// execute arbitrary code under the daemon's credentials (D-47).
func ParseConfigEnv(data []byte) map[string]string {
	out := make(map[string]string)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			continue
		}
		val := configEnvStripQuotes(line[eq+1:])
		out[key] = val
	}
	return out
}

// configEnvStripQuotes removes surrounding double or single quotes when both
// are present, and strips trailing inline comments from bare values.
// It never interprets any shell metacharacter.
func configEnvStripQuotes(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	// Bare value: strip trailing whitespace and inline comments ("  #…").
	if i := strings.Index(s, "  #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, " \t")
}
