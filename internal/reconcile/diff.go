package reconcile

import (
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Mask is what stands between a rendered secret and every place a human or a
// log might see one: `gvm apply --diff`, the dashboard's file preview, an
// audit param, a bug report an operator pastes into a chat window.
//
// It masks by TAINT, not by looking at values and guessing. The renderer
// already knows which keys expanded a `.Secret.*` reference and carries them
// on RenderedFile.TaintedKeys, so the differ never has to recognise a password
// by its shape -- which is fortunate, because a password whose shape is
// recognisable is not much of a password and a server name that looks like one
// would be masked forever.
//
// The value scan is a second, weaker net for the case the taint cannot cover:
// a secret that reached a file through a path this package did not render.
type Mask struct {
	// keys are config keys whose value expanded a secret, lowercased for
	// comparison because ini dialects differ on case and a diff that masked
	// `Password` and printed `password` would be worse than one that masked
	// neither.
	keys map[string]bool

	// values are literal secret strings, longest first so that a secret which
	// contains another is replaced whole.
	values []string
}

// MaskedFloor is the shortest secret the value scan will look for.
//
// A one- or two-character value appears inside ordinary words, and masking
// every occurrence of "a" would produce a diff nobody can read -- which is
// worse than useless, because an unreadable diff is one an operator approves
// without looking. Shorter secrets are still masked by KEY, which is the
// mechanism that is actually supposed to work; the value scan only exists to
// catch what the taint did not name.
const MaskedFloor = 4

// NewMask returns an empty mask, which redacts nothing. That is the correct
// default for a file with no secrets in it, and it means a caller that forgot
// to add anything produces a readable diff rather than a solid block of dots.
func NewMask() *Mask { return &Mask{keys: map[string]bool{}} }

// AddFile records every tainted key of a rendered config file.
func (m *Mask) AddFile(f spec.RenderedFile) *Mask {
	for _, k := range f.TaintedKeys {
		m.AddKey(k)
		// The VALUE too: the same secret appears in the game's own config
		// file under a key gvm does not own, and the operator's diff of that
		// file has to be safe as well.
		m.AddValue(f.Keys[k])
	}
	return m
}

// AddKey marks one key's value as unprintable.
func (m *Mask) AddKey(k string) *Mask {
	if m.keys == nil {
		m.keys = map[string]bool{}
	}
	if k = strings.TrimSpace(k); k != "" {
		m.keys[strings.ToLower(k)] = true
	}
	return m
}

// AddValue records a literal secret to replace wherever it appears.
func (m *Mask) AddValue(v string) *Mask {
	if len(v) < MaskedFloor {
		return m
	}
	for _, have := range m.values {
		if have == v {
			return m
		}
	}
	m.values = append(m.values, v)
	sort.Slice(m.values, func(i, j int) bool { return len(m.values[i]) > len(m.values[j]) })
	return m
}

// Line returns the line as a human may see it.
//
// Every line that reaches a diff, a preview or a log goes through here, so
// "did this call site remember to mask?" is not a question anyone has to ask
// per call site.
func (m *Mask) Line(s string) string {
	if m == nil {
		return s
	}
	out := s
	if k, sep, ok := splitAssignment(out); ok && m.keys[strings.ToLower(strings.TrimSpace(k))] {
		out = k + sep + spec.Redacted
	}
	for _, v := range m.values {
		out = strings.ReplaceAll(out, v, spec.Redacted)
	}
	return out
}

// splitAssignment finds the first `=` or `:` and returns the key, the
// separator with its surrounding spaces, and whether it found one.
//
// It handles the three shapes gvm writes and reads: systemd's `Key=value`,
// ini's `Key = value` and yaml's `key: value`. A line with no assignment on it
// -- a section header, a comment, a bare list item -- is not a key/value line
// and is left alone.
func splitAssignment(s string) (key, sep string, ok bool) {
	i := strings.IndexAny(s, "=:")
	if i < 0 {
		return "", "", false
	}
	key = s[:i]
	rest := s[i+1:]
	// The separator carries whatever spacing the file used, so masking does
	// not reformat a line and turn a one-line change into two.
	pad := len(rest) - len(strings.TrimLeft(rest, " \t"))
	return key, s[i:i+1] + rest[:pad], true
}

// maxDiffCells bounds the O(n*m) table the line diff builds.
//
// A generated unit is tens of lines and a game config is hundreds, so the
// bound is never reached in practice. It exists because a diff is something a
// dashboard request can trigger, and an unbounded quadratic allocation reached
// from an HTTP handler is a denial of service with extra steps. Past the
// bound the whole block is reported as replaced, which is honest and cheap.
const maxDiffCells = 4 << 20

// Unified renders a unified diff of two file versions, masked.
//
// Masking happens on the way OUT rather than before the comparison, so a
// rotated credential still shows as a changed line -- it just shows as
// `Password=••••••` on both sides. Masking first would have made the two
// versions compare equal and made a rotation invisible in the plan.
func Unified(label string, oldB, newB []byte, m *Mask) string {
	if string(oldB) == string(newB) {
		return ""
	}
	a := splitLines(oldB)
	b := splitLines(newB)

	var out strings.Builder
	out.WriteString("--- " + label + " (on disk)\n")
	out.WriteString("+++ " + label + " (would be written)\n")

	// Common prefix and suffix are trimmed first. It is not only an
	// optimisation: it is what keeps the quadratic table small for the case
	// this package actually produces, which is a forty-line unit file with
	// two changed lines in the middle.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	midA, midB := a[pre:len(a)-suf], b[pre:len(b)-suf]

	var ops []editOp
	if len(midA)*len(midB) > maxDiffCells {
		// Too big to align honestly. Saying so is better than picking an
		// arbitrary alignment and presenting it as the change.
		for _, l := range midA {
			ops = append(ops, editOp{'-', l})
		}
		for _, l := range midB {
			ops = append(ops, editOp{'+', l})
		}
	} else {
		ops = diffLines(midA, midB)
	}

	// Context: three lines either side, which is diff(1)'s default and the
	// amount that lets a reviewer see which section of an ini file a change
	// landed in.
	const ctx = 3
	writeContext(&out, a[:pre], ctx, true, m)
	for _, op := range ops {
		out.WriteString(string(op.kind) + " " + m.Line(op.line) + "\n")
	}
	writeContext(&out, a[len(a)-suf:], ctx, false, m)
	return out.String()
}

// writeContext emits up to n lines of unchanged context, from the tail of the
// prefix or the head of the suffix.
func writeContext(out *strings.Builder, lines []string, n int, tail bool, m *Mask) {
	if len(lines) > n {
		if tail {
			lines = lines[len(lines)-n:]
		} else {
			lines = lines[:n]
		}
	}
	for _, l := range lines {
		out.WriteString("  " + m.Line(l) + "\n")
	}
}

type editOp struct {
	kind byte // '-', '+' or ' '
	line string
}

// diffLines is a longest-common-subsequence diff over whole lines.
//
// Lines rather than characters, because every file this package writes is
// line-oriented and a character diff of an ini file produces changes that
// straddle a `=` and read as nonsense. LCS rather than Myers because the
// inputs are already trimmed to their differing middle and the table is
// bounded above; the extra code Myers would cost buys nothing at this size.
func diffLines(a, b []string) []editOp {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	// lcs[i][j] is the length of the longest common subsequence of a[i:] and
	// b[j:]. Building from the end means the walk below emits in order.
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var out []editOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, editOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, editOp{'-', a[i]})
			i++
		default:
			out = append(out, editOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, editOp{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, editOp{'+', b[j]})
	}
	return out
}

// splitLines splits on \n and drops one trailing empty element, so a file
// ending in a newline does not diff as having a blank last line.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
