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

// configEnvStripQuotes returns a value the way bash would read it, without
// executing anything.
//
//   - A value that starts with a quote runs to the matching closing quote;
//     after it, only whitespace and a `#…` comment may follow, and both are
//     dropped. So `"" # none` is the empty string, and `"a # b"` keeps its
//     inner `#`.
//   - A bare value ends at a `#` that follows whitespace (a `#` with no space
//     before it is part of the value, like a URL fragment), and trailing
//     whitespace is trimmed.
//
// It never interprets a shell metacharacter: `$(…)`, backticks, `${VAR}` and
// `$VAR` are returned verbatim (D-47).
func configEnvStripQuotes(s string) string {
	s = strings.TrimLeft(s, " \t")
	if len(s) > 0 && (s[0] == '"' || s[0] == '\'') {
		q := s[0]
		if end := strings.IndexByte(s[1:], q); end >= 0 {
			return s[1 : 1+end]
		}
		// An unterminated quote: take the rest, rather than guess where it ends.
		return s[1:]
	}
	if i := bareCommentIndex(s); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, " \t")
}

// bareCommentIndex returns the index of the comment `#` in a bare value — the
// first `#` at the start or preceded by whitespace — or -1 when there is none.
func bareCommentIndex(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '#' {
			continue
		}
		if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
			return i
		}
	}
	return -1
}

// UnsafePassword reports whether an imported password contains a character gvm
// cannot carry through its config and credential files unaltered — a single or
// double quote, a backslash, a dollar sign or a backtick. Such a password is a
// cutover blocker: gvm never imports it changed, and never logs its value (the
// caller rotates it). Keeping the rule here, next to the parser, is what lets
// discover check the value it just read without re-deriving the rule.
func UnsafePassword(v string) bool {
	return strings.ContainsAny(v, "'\"\\$`")
}
