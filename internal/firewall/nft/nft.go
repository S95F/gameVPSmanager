// Package nft implements the nftables firewall backend (inet gvm table).
//
// This backend owns EXACTLY ONE table: inet gvm. It never touches any other
// table, chain, or rule on the host. The full rule set lives in one atomic
// `nft -f -` transaction, so a half-applied plan is impossible.
package nft

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/firewall"
)

// tableOwner is the table this backend owns.
const tableOwner = "inet gvm"

// Backend is the nftables firewall backend.
//
// It owns exactly one table (inet gvm) and touches nothing else on the host.
type Backend struct{}

func (Backend) Name() string { return "nft" }

// Available reports whether the nft binary is in PATH.
func (Backend) Available() (bool, string) {
	_, err := exec.LookPath("nft")
	if err != nil {
		return false, "nft binary not found in PATH; install nftables"
	}
	return true, ""
}

// Status returns the current rules in the inet gvm table and whether the
// table exists. A missing table is not an error; it means nftables is not
// enabled for this instance.
func (Backend) Status(ctx context.Context) (firewall.RuleSet, bool, error) {
	out, err := runNFT(ctx, "list", "table", "inet", "gvm")
	if err != nil {
		ne := &nftErr{}
		if asNFTErr(err, ne) &&
			(bytes.Contains(ne.output, []byte("No such file or directory")) ||
				bytes.Contains(ne.output, []byte("table not found"))) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Parse gvm-tagged rules from comments.
	rules := parseRuleComments(out)
	return rules, true, nil
}

// Apply replaces the inet gvm table with the rules from plan in one atomic
// nft transaction.
func (Backend) Apply(ctx context.Context, plan *firewall.Plan) error {
	return applyScript(ctx, buildScript(plan))
}

// Enable creates an accept-all inet gvm table if one does not already exist.
func (Backend) Enable(ctx context.Context) error {
	script := "flush table " + tableOwner + "\n" +
		"table " + tableOwner + " {\n" +
		"\tchain input { type filter hook input priority 0; policy accept; }\n" +
		"}\n"
	return applyScript(ctx, script)
}

// Disable flushes and deletes the inet gvm table. Idempotent.
func (Backend) Disable(ctx context.Context) error {
	out, err := runNFT(ctx, "delete", "table", "inet", "gvm")
	if err != nil {
		ne := &nftErr{}
		if asNFTErr(err, ne) &&
			(bytes.Contains(ne.output, []byte("No such file or directory")) ||
				bytes.Contains(ne.output, []byte("table not found"))) {
			return nil
		}
	}
	_ = out
	return err
}

// buildScript produces the nft input script for a Plan.
//
// The script uses flush-and-replace: it always flushes the inet gvm table
// before writing new rules, making every Apply idempotent.
func buildScript(plan *firewall.Plan) string {
	var b strings.Builder
	b.WriteString("flush table " + tableOwner + "\n")
	b.WriteString("table " + tableOwner + " {\n")
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority 0;\n")
	b.WriteString("\t\tpolicy drop;\n")
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t\tiif lo accept\n")

	for _, step := range plan.Steps {
		switch step.Kind {
		case firewall.StepAdd, firewall.StepAdopt:
			rule, comment := ruleToNFT(step.Rule)
			if rule != "" {
				b.WriteString("\t\t")
				b.WriteString(rule)
				if comment != "" {
					b.WriteString(" # gvm: ")
					b.WriteString(comment)
				}
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String()
}

// ruleToNFT converts a firewall.Rule into an nft rule fragment and a
// human-readable comment.
func ruleToNFT(r firewall.Rule) (rule, comment string) {
	var parts []string

	// Source restriction.
	if r.From != "" {
		parts = append(parts, "ip saddr "+r.From)
	}

	// Protocol + port.
	if r.Port > 0 {
		proto := string(r.Proto)
		if proto == string(firewall.ProtoBoth) || proto == "" {
			// Emit two rules (tcp+udp) by returning a combined fragment.
			portStr := portRange(r.Port, r.PortEnd)
			parts = append(parts, "th dport "+portStr)
		} else {
			portStr := portRange(r.Port, r.PortEnd)
			parts = append(parts, proto+" dport "+portStr)
		}
	}

	// Verdict.
	switch r.Verb {
	case firewall.VerbDeny, firewall.VerbReject:
		parts = append(parts, "drop")
	default:
		parts = append(parts, "accept")
	}

	rule = strings.Join(parts, " ")
	comment = r.UFWSpec()
	return rule, comment
}

// portRange renders a port or port range for nft.
func portRange(port, portEnd int) string {
	if portEnd > port {
		return strconv.Itoa(port) + "-" + strconv.Itoa(portEnd)
	}
	return strconv.Itoa(port)
}

// applyScript pipes an nft script to `nft -f -`.
func applyScript(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &nftErr{args: "nft -f -", output: bytes.TrimSpace(out), cause: err}
	}
	return nil
}

// runNFT runs nft with args and returns stdout.
func runNFT(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nft", args...)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = bytes.TrimSpace(exitErr.Stderr)
		}
		return nil, &nftErr{args: fmt.Sprintf("nft %s", strings.Join(args, " ")), output: stderr, cause: err}
	}
	return out, nil
}

// parseRuleComments extracts gvm-tagged rules from nft list output.
//
// Each rule this package writes carries a "# gvm: <ufw-spec>" comment.
// Status parses these back to produce a RuleSet for the planner.
func parseRuleComments(out []byte) firewall.RuleSet {
	var rs firewall.RuleSet
	for _, line := range bytes.Split(out, []byte("\n")) {
		s := string(line)
		i := strings.Index(s, "# gvm: ")
		if i < 0 {
			continue
		}
		spec := strings.TrimSpace(s[i+7:])
		r := parseUFWSpec(spec)
		rs = append(rs, r)
	}
	return rs
}

// parseUFWSpec parses a ufw spec string back into a Rule.
// It is a best-effort reverse of Rule.UFWSpec.
func parseUFWSpec(spec string) firewall.Rule {
	// Very lightweight: extract verb, port and proto from the annotation.
	r := firewall.Rule{}
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return r
	}
	switch fields[0] {
	case "deny", "reject":
		r.Verb = firewall.VerbDeny
	case "limit":
		r.Verb = firewall.VerbLimit
	default:
		r.Verb = firewall.VerbAllow
	}
	for i, f := range fields {
		switch f {
		case "from":
			if i+1 < len(fields) {
				r.From = fields[i+1]
			}
		case "port":
			if i+1 < len(fields) {
				p := fields[i+1]
				if j := strings.Index(p, ":"); j >= 0 {
					r.Port, _ = strconv.Atoi(p[:j])
					r.PortEnd, _ = strconv.Atoi(p[j+1:])
				} else {
					r.Port, _ = strconv.Atoi(p)
				}
			}
		case "proto":
			if i+1 < len(fields) {
				r.Proto = firewall.Proto(fields[i+1])
			}
		}
	}
	return r
}

// nftErr is a structured error from the nft binary.
type nftErr struct {
	args   string
	output []byte
	cause  error
}

func (e *nftErr) Error() string {
	if len(e.output) > 0 {
		return fmt.Sprintf("%s: %s", e.args, e.output)
	}
	return fmt.Sprintf("%s: %v", e.args, e.cause)
}

func (e *nftErr) Unwrap() error { return e.cause }

// asNFTErr checks whether err is an *nftErr and assigns it.
func asNFTErr(err error, out *nftErr) bool {
	if ne, ok := err.(*nftErr); ok {
		*out = *ne
		return true
	}
	return false
}

// compile-time assertion: Backend implements firewall.Backend.
var _ firewall.Backend = Backend{}
