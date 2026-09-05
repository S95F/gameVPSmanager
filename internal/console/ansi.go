package console

import "strings"

const esc = 0x1b

// StripANSI removes ANSI CSI and OSC escape sequences (section 11.3 rule 3).
//
// The grammar is the spec's, exactly:
//
//	\x1b\[[0-9;?]*[ -/]*[@-~]  |  \x1b\][^\x07\x1b]*(\x07|\x1b\\)
//
// It is a scanner rather than a regexp for one reason that matters and one
// that does not. The one that matters: a line containing no ESC at all --
// which is almost every line a game server prints -- returns the input string
// unchanged, with no allocation and no regexp machine, on the hot path that
// every byte of every server's output travels. The one that does not: it is
// faster. ansi_test.go asserts byte-for-byte agreement with the spec regexp
// over a corpus including random bytes, so the fast path cannot drift.
//
// Anything the grammar does not recognise, including a sequence truncated at
// the end of a line, is left alone. Stripping a lone ESC would mean guessing,
// and a console log is evidence.
func StripANSI(s string) string {
	i := strings.IndexByte(s, esc)
	if i < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:i])
	for i < len(s) {
		if s[i] != esc {
			j := strings.IndexByte(s[i:], esc)
			if j < 0 {
				b.WriteString(s[i:])
				break
			}
			b.WriteString(s[i : i+j])
			i += j
			continue
		}
		if n := escLen(s[i:]); n > 0 {
			i += n
			continue
		}
		b.WriteByte(esc)
		i++
	}
	return b.String()
}

// escLen returns the length of the escape sequence at the start of s, or 0 if
// s does not start with one this grammar recognises.
func escLen(s string) int {
	if len(s) < 2 || s[0] != esc {
		return 0
	}
	switch s[1] {
	case '[': // CSI: parameter bytes, then intermediate bytes, then a final
		i := 2
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == ';' || s[i] == '?') {
			i++
		}
		for i < len(s) && s[i] >= ' ' && s[i] <= '/' {
			i++
		}
		if i < len(s) && s[i] >= '@' && s[i] <= '~' {
			return i + 1
		}
		return 0
	case ']': // OSC: a payload terminated by BEL or by ST (ESC \)
		for i := 2; i < len(s); i++ {
			switch s[i] {
			case 0x07:
				return i + 1
			case esc:
				// The payload class excludes ESC, so this ESC can only be the
				// first half of ST. If it is not, there is no match starting
				// here -- exactly what the regexp concludes.
				if i+1 < len(s) && s[i+1] == '\\' {
					return i + 2
				}
				return 0
			}
		}
		return 0
	}
	return 0
}
