package gamecfg

import (
	"bytes"
	"fmt"
	"strings"
)

// UESettings edits Palworld's PalWorldSettings.ini, an Unreal Engine settings
// file whose whole configurable surface is one line:
//
//	[/Script/Pal.PalGameWorldSettings]
//	OptionSettings=(Difficulty=None,DayTimeSpeedRate=1.000000,ServerName="Pal",AdminPassword="",PublicPort=8211,RCONEnabled=False)
//
// Around 90 settings on a single line, half of them quoted and half not, and
// the game rejects the file outright if the parenthesised list is malformed —
// it does not fall back to defaults for the one option it could not read, it
// falls back to defaults for all of them, and the operator sees a server that
// booted fine with the wrong world settings.
//
// So edits are byte-range splices inside that one line, and each item keeps
// the quoting style it already had. Quoting is per-option and not derivable
// from the value: PublicPort=8211 is a bare integer, ServerPassword="8211"
// would be a string, and swapping either one changes the setting's meaning.
// Copying the file's own choice is the only rule that is right without a
// per-option schema.
type UESettings struct {
	lines [][]byte // each with its own terminator, so untouched lines are exact
	// optIdx is the index in lines of the OptionSettings assignment, or -1.
	optIdx int
}

// ueItem is one K=V pair inside the parenthesised list, located within its line.
type ueItem struct {
	key string
	// keyStart is where the key text begins in the line, and valStart/valEnd
	// bracket the value as written, quotes included.
	keyStart         int
	valStart, valEnd int
	quoted           bool
}

// ParseUESettings reads the document. It never fails; a file with no
// OptionSettings line simply has no addressable keys, and Set then says so.
func ParseUESettings(data []byte) *UESettings {
	u := &UESettings{lines: splitKeepEOL(data), optIdx: -1}
	for i, ln := range u.lines {
		text := string(trimEOL(ln))
		trimmed := strings.TrimLeft(text, " \t")
		if strings.HasPrefix(trimmed, "OptionSettings") && ueAssignsOptionSettings(trimmed) {
			u.optIdx = i
			break
		}
	}
	return u
}

func ueAssignsOptionSettings(s string) bool {
	rest := strings.TrimLeft(s[len("OptionSettings"):], " \t")
	return strings.HasPrefix(rest, "=")
}

// items locates every pair inside the OptionSettings parentheses.
//
// The split is quote-aware: ServerDescription="a, b" contains a comma that is
// not a separator, and a naive strings.Split on "," turns one setting into two
// malformed ones and the whole file into defaults.
func (u *UESettings) items() []ueItem {
	if u.optIdx < 0 {
		return nil
	}
	line := trimEOL(u.lines[u.optIdx])
	open := bytes.IndexByte(line, '(')
	if open < 0 {
		return nil
	}
	shut := ueMatchingParen(line, open)
	if shut < 0 {
		return nil
	}

	var out []ueItem
	i := open + 1
	for i < shut {
		for i < shut && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		keyStart := i
		for i < shut && line[i] != '=' && line[i] != ',' {
			i++
		}
		if i >= shut || line[i] != '=' {
			// Not a K=V pair; skip to the next separator rather than guessing.
			for i < shut && line[i] != ',' {
				i++
			}
			i++
			continue
		}
		key := strings.TrimSpace(string(line[keyStart:i]))
		i++ // past '='
		valStart := i
		quoted := i < shut && line[i] == '"'
		if quoted {
			i++
			for i < shut {
				if line[i] == '\\' {
					i += 2
					continue
				}
				if line[i] == '"' {
					i++
					break
				}
				i++
			}
		}
		for i < shut && line[i] != ',' {
			i++
		}
		if key != "" {
			out = append(out, ueItem{key: key, keyStart: keyStart, valStart: valStart, valEnd: i, quoted: quoted})
		}
		i++ // past ','
	}
	return out
}

// ueMatchingParen returns the index of the ')' closing the '(' at open,
// counting nesting and ignoring anything inside quotes.
func ueMatchingParen(line []byte, open int) int {
	depth := 0
	inStr := false
	for i := open; i < len(line); i++ {
		c := line[i]
		if inStr {
			switch c {
			case '\\':
				i++
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (u *UESettings) find(key string) (ueItem, bool) {
	for _, it := range u.items() {
		if it.key == key {
			return it, true
		}
	}
	return ueItem{}, false
}

// Get returns an option's value with its quotes and escapes removed.
func (u *UESettings) Get(key string) (string, bool) {
	it, ok := u.find(key)
	if !ok {
		return "", false
	}
	raw := string(trimEOL(u.lines[u.optIdx])[it.valStart:it.valEnd])
	if it.quoted {
		return ueUnquote(raw), true
	}
	return strings.TrimSpace(raw), true
}

// Set rewrites one option in place, keeping its quoting style.
//
// A key the file does not already contain is ErrKeyNotFound, not an append.
// PalWorldSettings.ini is seeded from the game's own DefaultPalWorldSettings.ini
// by pal.settings_seed, so it carries every option the installed version
// supports; a key that is missing is one this version does not have, and
// appending it produces a line the game reads, ignores, and gives no sign
// about.
func (u *UESettings) Set(key, value string) error {
	if err := (ueSettingsCodec{}).KeySyntax(key); err != nil {
		return err
	}
	if u.optIdx < 0 {
		return fmt.Errorf("%w: this file has no OptionSettings=( ... ) line", ErrKeyNotFound)
	}
	it, ok := u.find(key)
	if !ok {
		return fmt.Errorf("%w: %s is not in OptionSettings; pal.settings_seed copies the version's own default list, so this option does not exist in the installed build", ErrKeyNotFound, key)
	}

	written := value
	if it.quoted {
		written = ueQuote(value)
	} else if strings.ContainsAny(value, `",()`) || strings.TrimSpace(value) != value {
		// An unquoted slot holds a number, an enum or a bool. Splicing a comma
		// or a quote into one silently redefines where the next option starts.
		return fmt.Errorf("%w: %s is an unquoted option and %q would break the list", ErrUnsupported, key, value)
	}

	u.lines[u.optIdx] = spliceLine(u.lines[u.optIdx], it.valStart, it.valEnd, written)
	return nil
}

// Delete removes options from the list. It reports how many it removed.
func (u *UESettings) Delete(key string) int {
	n := 0
	for {
		it, ok := u.find(key)
		if !ok {
			return n
		}
		line := trimEOL(u.lines[u.optIdx])

		start := it.keyStart
		for start > 0 && (line[start-1] == ' ' || line[start-1] == '\t') {
			start--
		}
		end := it.valEnd
		if end < len(line) && line[end] == ',' {
			end++
		} else if start > 0 && line[start-1] == ',' {
			// Last item in the list: take the comma in front of it instead, or
			// the list ends with a dangling separator.
			start--
		}

		u.lines[u.optIdx] = spliceLine(u.lines[u.optIdx], start, end, "")
		n++
	}
}

// Keys lists the option names inside OptionSettings, in file order.
//
// The surrounding INI keys are deliberately not addressable: the only one is
// OptionSettings itself, and exposing it would let a manifest replace the whole
// list with a single templated string.
func (u *UESettings) Keys() []string {
	var out []string
	for _, it := range u.items() {
		out = append(out, it.key)
	}
	return dedupe(out)
}

// Bytes renders the document.
func (u *UESettings) Bytes() []byte {
	var b bytes.Buffer
	for _, ln := range u.lines {
		b.Write(ln)
	}
	return b.Bytes()
}

func (u *UESettings) String() string { return string(u.Bytes()) }

// ueQuote writes a quoted Unreal string value.
func ueQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\r', '\n':
			// The whole list is one line; a newline in a server name would
			// truncate every option after it.
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func ueUnquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' {
		return s
	}
	s = s[1:]
	if strings.HasSuffix(s, `"`) {
		s = s[:len(s)-1]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

type ueSettingsCodec struct{}

func (ueSettingsCodec) Name() string { return "ue_option_settings" }

func (ueSettingsCodec) Parse(data []byte) (Doc, error) { return ParseUESettings(data), nil }

// New refuses: Palworld ships DefaultPalWorldSettings.ini with the full option
// list for the installed build, and pal.settings_seed copies it. A file gvm
// synthesised would contain only the options the manifest names, and the game
// would run every other one at its compiled-in default rather than the
// documented one.
func (ueSettingsCodec) New() (Doc, error) {
	return nil, fmt.Errorf("%w: pal.settings_seed copies DefaultPalWorldSettings.ini", ErrCannotCreate)
}

func (ueSettingsCodec) KeySyntax(key string) error {
	if err := plainKeySyntax(key); err != nil {
		return err
	}
	if !isLuaIdent(key) {
		return fmt.Errorf("config key %q is not an OptionSettings identifier", key)
	}
	return nil
}
