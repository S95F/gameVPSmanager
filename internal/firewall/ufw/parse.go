// Package ufw parses the output of `ufw status numbered` and applies rules.
package ufw

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/firewall"
)

// ParseResult is the output of Parse.
type ParseResult struct {
	Active  bool
	Rules   firewall.RuleSet
	Trusted bool // false when any line could not be parsed (D-42)
}

// Parse reads `ufw status numbered` output and returns the live rule set.
// It never returns an error for parse failures; instead, it sets Trusted=false
// so the caller can suppress deletions (D-42).
func Parse(r io.Reader) ParseResult {
	res := ParseResult{Trusted: true}
	sc := bufio.NewScanner(r)
	headerPassed := false
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "Status:") {
			res.Active = strings.TrimSpace(strings.TrimPrefix(trimmed, "Status:")) == "active"
			continue
		}
		if !res.Active {
			continue
		}
		if strings.HasPrefix(trimmed, "To") && strings.Contains(trimmed, "Action") {
			headerPassed = true
			continue
		}
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		if !headerPassed || trimmed == "" {
			continue
		}

		rule, ok := parseLine(trimmed)
		if !ok {
			res.Trusted = false
			continue
		}
		// Skip v6 duplicates — they are the same logical rule.
		if rule.Family == "v6" {
			continue
		}
		// parseLine returns ok=true with a zero Rule for soft-skips (e.g. OUT rules).
		if rule.Port == 0 && rule.Verb == "" {
			continue
		}
		res.Rules = append(res.Rules, rule)
	}
	return res
}

// parseLine parses a single numbered rule line from `ufw status numbered`.
// Format (approximate):
//
//	[ N] <to-spec>  <ACTION> IN  <from-spec>  # comment
func parseLine(line string) (firewall.Rule, bool) {
	// Must start with [ N]
	if !strings.HasPrefix(line, "[") {
		return firewall.Rule{}, false
	}
	close := strings.Index(line, "]")
	if close < 0 {
		return firewall.Rule{}, false
	}
	numStr := strings.TrimSpace(line[1:close])
	if _, err := strconv.Atoi(numStr); err != nil {
		return firewall.Rule{}, false
	}
	rest := strings.TrimSpace(line[close+1:])

	// Extract trailing comment: last `# text` not inside port/proto.
	comment := ""
	if idx := lastHashIndex(rest); idx >= 0 {
		comment = strings.TrimSpace(rest[idx+1:])
		rest = strings.TrimSpace(rest[:idx])
	}

	// Strip (v6) suffix from the To and From fields.
	family := ""
	if strings.Contains(rest, "(v6)") {
		family = "v6"
		rest = strings.ReplaceAll(rest, "(v6)", "")
		rest = strings.TrimSpace(rest)
	}

	// Split on whitespace runs; expected: [to, action, "IN"|"OUT", from...]
	fields := strings.Fields(rest)
	if len(fields) < 3 {
		return firewall.Rule{}, false
	}

	toSpec := fields[0]
	actionStr := strings.ToLower(fields[1])
	// fields[2] should be "IN" or "OUT" — we only care about IN.
	if len(fields) >= 3 && strings.ToLower(fields[2]) != "in" {
		// OUT or unexpected — skip, but don't corrupt trusted.
		return firewall.Rule{}, true // soft-skip: return ok=true with zero rule
	}

	from := ""
	if len(fields) >= 4 {
		// Anything after "IN" is the From spec.
		fromParts := fields[3:]
		joined := strings.Join(fromParts, " ")
		if joined != "Anywhere" && joined != "anywhere" {
			from = joined
		}
	}

	verb := parseVerb(actionStr)
	port, portEnd, proto, err := parseToSpec(toSpec)
	if err != nil {
		return firewall.Rule{}, false
	}

	return firewall.Rule{
		Verb:    verb,
		Proto:   proto,
		Port:    port,
		PortEnd: portEnd,
		From:    from,
		Comment: comment,
		Family:  family,
	}, true
}

// lastHashIndex returns the index of the first '#' in s that is preceded by
// whitespace — that marks the start of the ufw comment field. The comment
// text itself may contain further '#' characters; scanning left-to-right
// gives us the separator, not a '#' inside the comment value.
func lastHashIndex(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && s[i-1] == ' ' {
			return i
		}
	}
	return -1
}

func parseVerb(s string) firewall.Verb {
	switch strings.ToLower(s) {
	case "limit":
		return firewall.VerbLimit
	case "deny":
		return firewall.VerbDeny
	case "reject":
		return firewall.VerbReject
	default:
		return firewall.VerbAllow
	}
}

// parseToSpec parses strings like "22/tcp", "443", "27000:27030/udp", "Anywhere".
func parseToSpec(s string) (port, portEnd int, proto firewall.Proto, err error) {
	if s == "Anywhere" || s == "anywhere" {
		return 0, 0, firewall.ProtoBoth, nil
	}
	protoStr := ""
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		protoStr = strings.ToLower(s[idx+1:])
		s = s[:idx]
	}
	switch protoStr {
	case "tcp":
		proto = firewall.ProtoTCP
	case "udp":
		proto = firewall.ProtoUDP
	case "":
		proto = firewall.ProtoBoth
	default:
		proto = firewall.ProtoBoth
	}

	if idx := strings.Index(s, ":"); idx >= 0 {
		portStr := s[:idx]
		endStr := s[idx+1:]
		p, e1 := strconv.Atoi(portStr)
		pe, e2 := strconv.Atoi(endStr)
		if e1 != nil || e2 != nil {
			return 0, 0, proto, fmt.Errorf("bad port range %q", s+"/"+protoStr)
		}
		return p, pe, proto, nil
	}

	p, e := strconv.Atoi(s)
	if e != nil {
		return 0, 0, proto, fmt.Errorf("bad port %q", s)
	}
	return p, 0, proto, nil
}
