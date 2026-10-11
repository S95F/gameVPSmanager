package steamcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Valve KeyValues -- the text format `appmanifest_<appid>.acf` and the output
// of `app_info_print` are both written in.
//
// It is parsed rather than grepped. The format is genuinely nested: quoted keys
// and values with escapes inside them, `//` comments, blocks to arbitrary
// depth, `[$CONDITION]` suffixes, and the same key name appearing at several
// levels. `grep BytesDownloaded` is right until the first install whose
// InstalledDepots block carries one, and the cost of a wrong answer here is the
// exact failure D-32 exists to prevent: a half-finished download reported as a
// verified install, and a server that then dies for reasons that look nothing
// like a download problem.
//
// Nothing in this file touches the filesystem or the clock, so every rule in it
// is tested on the Windows development machine against the fixtures in testdata.

// The parse refusals. Each is a sentinel because the operator's next move
// differs: a malformed document is a bug or a truncated file, a document that
// is not an appmanifest means gvm is reading the wrong path, and a missing
// app_info block means the login failed or the appinfo cache is still stale --
// which is the one failure buildid.go's purge exists to prevent.
var (
	ErrKeyValues      = errors.New("steamcmd: malformed Valve KeyValues")
	ErrNotAppManifest = errors.New("steamcmd: this is not an appmanifest")
	ErrNoAppInfo      = errors.New("steamcmd: app_info_print printed no block for this app")
)

// Document bounds. An appmanifest is a few kilobytes and a one-app
// app_info_print is a few hundred; these are three orders of magnitude above
// both, and they exist because the bytes come from a third party and a parser
// that will allocate whatever it is handed is a denial of service with a
// friendly name.
const (
	maxKVBytes = 16 << 20
	maxKVDepth = 64
	maxKVToken = 1 << 20
)

// KV is one KeyValues node: a leaf with a Value, or a Block with Children.
//
// Block is carried explicitly rather than inferred from len(Children), because
// an empty block and an empty string are different documents and reading one as
// the other is how "this key is absent" becomes "this key is empty".
type KV struct {
	Key      string
	Value    string
	Block    bool
	Children []*KV
	Line     int
}

// Child returns the first child whose key matches, case-insensitively.
//
// Case-insensitively because Valve's own writers disagree with each other:
// an appmanifest spells it `appid` and `buildid` while app_info output spells
// the same facts `AppID` and `buildid` in different blocks. A lookup that
// matched exactly would work on one of them.
func (n *KV) Child(key string) (*KV, bool) {
	if n == nil {
		return nil, false
	}
	for _, c := range n.Children {
		if strings.EqualFold(c.Key, key) {
			return c, true
		}
	}
	return nil, false
}

// Lookup walks a path of keys.
func (n *KV) Lookup(path ...string) (*KV, bool) {
	cur := n
	for _, key := range path {
		c, ok := cur.Child(key)
		if !ok {
			return nil, false
		}
		cur = c
	}
	return cur, cur != nil
}

// Str is the leaf value at path, or "" when it is absent or is a block.
func (n *KV) Str(path ...string) string {
	c, ok := n.Lookup(path...)
	if !ok || c.Block {
		return ""
	}
	return c.Value
}

// Int reads a decimal leaf. The second result is false when the key is absent
// or is not a number, which callers must distinguish from a value of 0: a
// missing BytesToDownload read as zero makes the byte-parity check pass by
// having nothing to compare, which is verification by absence of evidence.
func (n *KV) Int(path ...string) (int64, bool) {
	s := n.Str(path...)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Uint reads an unsigned decimal leaf, for StateFlags and the depot sizes.
func (n *KV) Uint(path ...string) (uint64, bool) {
	s := n.Str(path...)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Keys lists this node's child keys in document order, for diagnostics: a
// message that says which keys WERE there turns "no buildid" from a dead end
// into a readable finding.
func (n *KV) Keys() []string {
	if n == nil {
		return nil
	}
	out := make([]string, 0, len(n.Children))
	for _, c := range n.Children {
		out = append(out, c.Key)
	}
	return out
}

// ParseKeyValues parses a whole document and requires it to end cleanly.
//
// Trailing junk is an error rather than a shrug: the one thing this parser must
// never do is read half a truncated appmanifest and report the keys it managed
// to reach.
func ParseKeyValues(r io.Reader) (*KV, error) {
	lx := newKVLexer(r)
	kids, end, err := parseKVChildren(lx, 0)
	if err != nil {
		return nil, err
	}
	if end.kind != kvEOF {
		return nil, fmt.Errorf("%w: line %d: unexpected %s at the top level",
			ErrKeyValues, end.line, end.kind)
	}
	return &KV{Block: true, Children: kids}, nil
}

// ParseAppInfo pulls one app's block out of `app_info_print` output.
//
// steamcmd prints a console preamble before the block -- the stderr redirect
// line, the version banner, the login progress -- and that preamble is not
// KeyValues at all, so the block is located by its own header line (`"380870"`)
// and parsed from there. An absent header is ErrNoAppInfo and never an empty
// result: "Steam told us nothing about this app" and "this app has no buildid"
// lead to different places, and the first one is usually a failed login or the
// stale appinfo.vdf cache (see PurgeAppInfo).
func ParseAppInfo(r io.Reader, appid int) (*KV, error) {
	br := bufio.NewReader(io.LimitReader(r, maxKVBytes))
	want := `"` + strconv.Itoa(appid) + `"`
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed == want {
			break
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: looked for the %s header in the output of "+
					"app_info_print %d", ErrNoAppInfo, want, appid)
			}
			return nil, fmt.Errorf("steamcmd: read app_info_print output: %w", err)
		}
	}

	lx := newKVLexerFrom(br)
	open := lx.next()
	if open.err != nil {
		return nil, open.err
	}
	if open.kind != kvOpen {
		return nil, fmt.Errorf("%w: line %d: the %s header is followed by %s, not a block",
			ErrKeyValues, open.line, want, open.kind)
	}
	kids, end, err := parseKVChildren(lx, 1)
	if err != nil {
		return nil, err
	}
	if end.kind != kvClose {
		return nil, fmt.Errorf("%w: the %s block is not closed", ErrKeyValues, want)
	}
	// Whatever follows the block is console output again, so it is deliberately
	// not required to be anything.
	return &KV{Key: strconv.Itoa(appid), Block: true, Children: kids}, nil
}

// parseKVChildren reads pairs until a closing brace or EOF, and reports which
// of the two it hit so the caller can require the one it expects.
func parseKVChildren(lx *kvLexer, depth int) ([]*KV, kvToken, error) {
	if depth > maxKVDepth {
		return nil, kvToken{}, fmt.Errorf("%w: nested deeper than %d blocks", ErrKeyValues, maxKVDepth)
	}
	var out []*KV
	for {
		tok := lx.next()
		switch {
		case tok.err != nil:
			return nil, tok, tok.err
		case tok.kind == kvEOF, tok.kind == kvClose:
			return out, tok, nil
		case tok.kind == kvCond:
			// A conditional qualifies the pair it follows. gvm renders nothing
			// from these documents, so the qualifier is read and dropped rather
			// than evaluated -- and dropping it is recorded here so a future
			// reader knows it was a decision.
			continue
		case tok.kind != kvString:
			return nil, tok, fmt.Errorf("%w: line %d: %s where a key was expected",
				ErrKeyValues, tok.line, tok.kind)
		}

		key := tok.text
		val := lx.next()
		for val.kind == kvCond && val.err == nil {
			val = lx.next()
		}
		switch {
		case val.err != nil:
			return nil, val, val.err
		case val.kind == kvString:
			out = append(out, &KV{Key: key, Value: val.text, Line: tok.line})
		case val.kind == kvOpen:
			kids, end, err := parseKVChildren(lx, depth+1)
			if err != nil {
				return nil, end, err
			}
			if end.kind != kvClose {
				return nil, end, fmt.Errorf("%w: line %d: the %q block is never closed",
					ErrKeyValues, tok.line, key)
			}
			out = append(out, &KV{Key: key, Block: true, Children: kids, Line: tok.line})
		default:
			return nil, val, fmt.Errorf("%w: line %d: key %q is followed by %s",
				ErrKeyValues, val.line, key, val.kind)
		}
	}
}

// kvKind is a token class.
type kvKind uint8

const (
	kvEOF kvKind = iota
	kvString
	kvOpen
	kvClose
	kvCond
)

func (k kvKind) String() string {
	switch k {
	case kvEOF:
		return "end of document"
	case kvString:
		return "a value"
	case kvOpen:
		return "{"
	case kvClose:
		return "}"
	case kvCond:
		return "a [$conditional]"
	}
	return "an unknown token"
}

type kvToken struct {
	kind kvKind
	text string
	line int
	err  error
}

// kvLexer tokenises KeyValues from a reader.
//
// A lexer rather than a line splitter because a quoted value may contain
// braces, `//` and newlines, and a line-oriented reader cannot tell a brace
// inside a Windows install path from the end of a block.
type kvLexer struct {
	r    *bufio.Reader
	line int
}

func newKVLexer(r io.Reader) *kvLexer {
	return newKVLexerFrom(bufio.NewReader(io.LimitReader(r, maxKVBytes)))
}

func newKVLexerFrom(br *bufio.Reader) *kvLexer { return &kvLexer{r: br, line: 1} }

func (lx *kvLexer) readByte() (byte, error) {
	b, err := lx.r.ReadByte()
	if err == nil && b == '\n' {
		lx.line++
	}
	return b, err
}

func (lx *kvLexer) unread() { _ = lx.r.UnreadByte() }

func (lx *kvLexer) next() kvToken {
	for {
		b, err := lx.readByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return kvToken{kind: kvEOF, line: lx.line}
			}
			return kvToken{line: lx.line, err: fmt.Errorf("steamcmd: read KeyValues: %w", err)}
		}
		switch {
		case b == ' ' || b == '\t' || b == '\r' || b == '\n':
			continue
		case b == '{':
			return kvToken{kind: kvOpen, line: lx.line}
		case b == '}':
			return kvToken{kind: kvClose, line: lx.line}
		case b == '"':
			return lx.quoted()
		case b == '[':
			return lx.conditional()
		case b == '/':
			// A comment. Valve writes `//`; a lone `/` has no meaning here and
			// is refused rather than guessed at.
			nb, nerr := lx.readByte()
			if nerr != nil || nb != '/' {
				return kvToken{line: lx.line, err: fmt.Errorf("%w: line %d: a lone %q",
					ErrKeyValues, lx.line, "/")}
			}
			lx.skipLine()
			continue
		case b == '#':
			// `#base` and `#include` pull in another file. An appmanifest never
			// has one, and silently ignoring a directive that adds keys is how a
			// parser confidently reads half a document.
			word := lx.bare('#')
			return kvToken{line: lx.line, err: fmt.Errorf("%w: line %d: %s is not supported; "+
				"gvm will not follow a KeyValues include", ErrKeyValues, lx.line, word)}
		default:
			lx.unread()
			return kvToken{kind: kvString, text: lx.bare(0), line: lx.line}
		}
	}
}

// quoted reads a double-quoted token, resolving the escapes Valve emits.
//
// Unknown escapes are kept verbatim, backslash included. Valve's own writer
// escapes only \\, \", \n and \t, so a surviving `\d` in a Windows path
// (`C:\depots`) is far more likely to be an unescaped separator than an escape
// sequence this parser has not heard of, and dropping the backslash would
// silently corrupt the path.
func (lx *kvLexer) quoted() kvToken {
	var sb strings.Builder
	start := lx.line
	for {
		b, err := lx.readByte()
		if err != nil {
			return kvToken{line: start, err: fmt.Errorf("%w: line %d: a quoted value is never "+
				"closed, so the document is truncated", ErrKeyValues, start)}
		}
		switch b {
		case '"':
			return kvToken{kind: kvString, text: sb.String(), line: start}
		case '\\':
			e, eerr := lx.readByte()
			if eerr != nil {
				return kvToken{line: start, err: fmt.Errorf("%w: line %d: the document ends "+
					"inside an escape", ErrKeyValues, start)}
			}
			switch e {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '\\':
				sb.WriteByte('\\')
			case '"':
				sb.WriteByte('"')
			default:
				sb.WriteByte('\\')
				sb.WriteByte(e)
			}
		default:
			sb.WriteByte(b)
		}
		if sb.Len() > maxKVToken {
			return kvToken{line: start, err: fmt.Errorf("%w: line %d: a value longer than %d bytes",
				ErrKeyValues, start, maxKVToken)}
		}
	}
}

// conditional reads a `[$WIN32]` platform qualifier as one opaque token.
func (lx *kvLexer) conditional() kvToken {
	var sb strings.Builder
	sb.WriteByte('[')
	for {
		b, err := lx.readByte()
		if err != nil {
			return kvToken{line: lx.line, err: fmt.Errorf("%w: line %d: an unclosed [conditional]",
				ErrKeyValues, lx.line)}
		}
		sb.WriteByte(b)
		if b == ']' {
			return kvToken{kind: kvCond, text: sb.String(), line: lx.line}
		}
		if sb.Len() > 256 {
			return kvToken{line: lx.line, err: fmt.Errorf("%w: line %d: an implausible [conditional]",
				ErrKeyValues, lx.line)}
		}
	}
}

// bare reads an unquoted token. first is a byte already consumed, or 0.
func (lx *kvLexer) bare(first byte) string {
	var sb strings.Builder
	if first != 0 {
		sb.WriteByte(first)
	}
	for {
		b, err := lx.readByte()
		if err != nil {
			return sb.String()
		}
		switch b {
		case ' ', '\t', '\r', '\n', '{', '}', '"':
			lx.unread()
			if b == '\n' {
				// unread puts the newline back, so the count must not keep it.
				lx.line--
			}
			return sb.String()
		}
		sb.WriteByte(b)
		if sb.Len() > maxKVToken {
			return sb.String()
		}
	}
}

func (lx *kvLexer) skipLine() {
	for {
		b, err := lx.readByte()
		if err != nil || b == '\n' {
			return
		}
	}
}

// AppManifest is `steamapps/appmanifest_<appid>.acf`: Steam's own record of
// what it put on disk, and the only authoritative answer to "did that download
// finish" (D-32).
//
// The three Has* fields exist because absence and zero must not be the same
// answer. An appmanifest with no BytesToDownload key would otherwise pass the
// byte-parity check by having nothing to compare, which is the exact shape of
// bug that makes a verification step worse than none: it reports a verdict it
// never reached.
// The two string fields are VALIDATED rather than copied, because this file is
// third-party input: hostops gives the install tree OwnedByInstance, and
// ARCHITECTURE 18.1 lists it as what a compromised workshop item yields. Both
// fields are interpolated straight into the sentence the CLI prints -- the
// verdict note, the verify steps, Integrity.Facts and, through CheckBuild's
// Summary, the plan preview -- and the lexer materialises Valve's \n, \r and \t
// escapes into real control bytes inside a token bounded only at 1 MB. So
// "18452901\r\n[ OK ] everything verified\n\x1b[2K" is a buildid a mod can
// write, and an unvalidated copy is a forged verdict line.
type AppManifest struct {
	AppID         int
	Name          string
	Universe      int64
	StateFlags    uint64
	HasStateFlags bool
	// BuildID and TargetBuildID hold the value only when it is a plausible
	// build id. The raw bytes stay reachable through Root for diagnostics.
	BuildID       string
	TargetBuildID string
	// BuildIDSuspect is "a build id key was present and gvm refused it".
	//
	// Separate from an empty BuildID for the same reason the three Has* fields
	// below exist, and for one more: "" makes CheckBuild answer
	// BuildNotInstalled, which would turn a tampered appmanifest into a plan
	// to reinstall the game. A refusal and an absence must not read alike.
	BuildIDSuspect bool
	LastUpdated    time.Time
	SizeOnDisk     int64

	BytesToDownload int64
	BytesDownloaded int64
	HasDownloadPair bool

	BytesToStage int64
	BytesStaged  int64
	HasStagePair bool

	// Root is the AppState block, so a caller that needs a fact nothing has
	// needed yet does not have to extend this struct to read it.
	Root *KV
}

// StateInstalled is the only StateFlags value that means "fully installed and
// nothing outstanding". Anything else -- including 1062, which HAS the
// fully-installed bit set alongside "files missing" -- is not an install gvm
// will call good.
const StateInstalled = 4

// stateFlagNames decodes StateFlags for the operator.
//
// This is the difference between "StateFlags is 1062" and "StateFlags 1062 =
// fully installed + update required + files missing + update started", which
// says what is wrong and roughly what to do, at 2am, without a wiki.
var stateFlagNames = []struct {
	bit  uint64
	name string
}{
	{1, "uninstalled"},
	{2, "update required"},
	{4, "fully installed"},
	{8, "encrypted"},
	{16, "locked"},
	{32, "files missing"},
	{64, "app running"},
	{128, "files corrupt"},
	{256, "update running"},
	{512, "update paused"},
	{1024, "update started"},
	{2048, "uninstalling"},
	{4096, "backup running"},
}

// StateDescription renders StateFlags as its set bits.
func StateDescription(flags uint64) string {
	if flags == 0 {
		return "0 (invalid: Steam has no state for this app)"
	}
	var names []string
	rest := flags
	for _, f := range stateFlagNames {
		if flags&f.bit != 0 {
			names = append(names, f.name)
			rest &^= f.bit
		}
	}
	if rest != 0 {
		names = append(names, fmt.Sprintf("unknown bits 0x%X", rest))
	}
	return fmt.Sprintf("%d (%s)", flags, strings.Join(names, " + "))
}

// Installed reports the StateFlags verdict, and nothing more.
func (m *AppManifest) Installed() bool {
	return m != nil && m.HasStateFlags && m.StateFlags == StateInstalled
}

// TargetSettled reports whether Steam has no update outstanding for this app.
//
// Steam writes TargetBuildID when an update begins and leaves it equal to
// buildid when one finishes, so an inequality is Steam's own record that an
// update was started and did not land. An ABSENT TargetBuildID is settled:
// older Steam builds do not write the key at all, and absence is not evidence
// here the way a missing byte pair is -- there is no arithmetic to perform.
func (m *AppManifest) TargetSettled() bool {
	return m != nil && (m.TargetBuildID == "" || m.TargetBuildID == m.BuildID)
}

// Complete is the full D-32 appmanifest verdict.
//
// Installed() is only Steam's StateFlags opinion. The byte pairs are what catch
// "steamcmd printed Success and the tree is half a gigabyte light", and an
// absent pair is not a pass, for the reason the Has* fields exist at all. It
// lives here rather than in Verify so that the thorough reader and the
// thirty-second poll apply one rule: two components reading this one file must
// not reach opposite verdicts.
//
// Verify's own checks stay separate because section 13.5 needs
// ErrBytesDownloaded, ErrBytesStaged and ErrWrongBuild to be distinguishable
// failures; this is the same arithmetic collapsed to a single boolean for the
// callers that only need the verdict.
func (m *AppManifest) Complete() bool {
	return m.Installed() && !m.BuildIDSuspect &&
		m.HasDownloadPair && m.BytesDownloaded == m.BytesToDownload &&
		m.HasStagePair && m.BytesStaged == m.BytesToStage &&
		m.TargetSettled()
}

// BuildLabel is the build id as an operator-facing string.
//
// It never returns the empty string, because "build " in a verdict line reads
// as a rendering bug rather than as the finding it is, and it never returns the
// refused bytes.
func (m *AppManifest) BuildLabel() string {
	switch {
	case m == nil:
		return "(no appmanifest)"
	case m.BuildIDSuspect:
		return "(refused: not a plausible build id)"
	case m.BuildID == "":
		return "(absent)"
	}
	return m.BuildID
}

// maxLabelBytes bounds a display label. The real one is 33 bytes; the token
// the lexer will hand over is bounded at 1 MB, and a megabyte of plausible
// text in a verdict line scrolls the actual verdict out of a terminal.
const maxLabelBytes = 128

// maxBuildIDBytes bounds a build id. Steam's are eight digits; twenty leaves
// room for a decade of them and refuses anything that is not one.
const maxBuildIDBytes = 20

// plausibleBuildID reports whether s is something Steam would have written.
//
// Digits only, which is what the rest of this package already assumes --
// CheckBuild does ParseUint(local, 10, 64) and buildid.go's isAllDigits picks
// depot ids out of a block the same way. A value that is not a run of digits
// means something other than Steam wrote this file, and the right response is
// to refuse it rather than to print it.
//
// It is deliberately not applied inside CheckBuild: that function also compares
// the non-numeric version strings a Minecraft-shaped provider carries, and
// those never came from an attacker-writable file.
func plausibleBuildID(s string) bool {
	if s == "" || len(s) > maxBuildIDBytes {
		return false
	}
	return isAllDigits(s)
}

// sanitizeLabel makes a third-party display string safe to print.
//
// Stripping and not refusing: the name is a label, and a cosmetic oddity in it
// must not fail a verification that is otherwise sound. What goes is the
// control characters -- C0, DEL and the C1 range -- that would let a forged
// name redraw an operator's terminal or forge a line of gvm's own output.
//
// C1 is dropped by RUNE and not by byte, because 0x80-0x9F are also UTF-8
// continuation bytes and a byte-level filter would corrupt every non-ASCII
// game name on the catalogue.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			// Invalid UTF-8, or a literal U+FFFD. Dropped either way: a
			// replacement character in a game name is not information.
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		default:
			if b.Len()+utf8.RuneLen(r) > maxLabelBytes {
				return b.String()
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ParseAppManifest reads one appmanifest.
func ParseAppManifest(r io.Reader) (*AppManifest, error) {
	doc, err := ParseKeyValues(r)
	if err != nil {
		return nil, err
	}
	root, ok := doc.Child("AppState")
	if !ok || !root.Block {
		return nil, fmt.Errorf("%w: the document's top-level keys are %v and an appmanifest's "+
			"is \"AppState\"", ErrNotAppManifest, doc.Keys())
	}

	m := &AppManifest{Root: root}
	appid, ok := root.Int("appid")
	if !ok || appid <= 0 {
		return nil, fmt.Errorf("%w: AppState carries no usable appid (keys: %v)",
			ErrNotAppManifest, root.Keys())
	}
	m.AppID = int(appid)
	m.Name = sanitizeLabel(root.Str("name"))
	m.Universe, _ = root.Int("Universe")
	m.StateFlags, m.HasStateFlags = root.Uint("StateFlags")
	for _, f := range []struct {
		key string
		dst *string
	}{{"buildid", &m.BuildID}, {"TargetBuildID", &m.TargetBuildID}} {
		raw := root.Str(f.key)
		switch {
		case raw == "":
		case plausibleBuildID(raw):
			*f.dst = raw
		default:
			m.BuildIDSuspect = true
		}
	}
	m.SizeOnDisk, _ = root.Int("SizeOnDisk")
	if secs, ok := root.Int("LastUpdated"); ok && secs > 0 {
		m.LastUpdated = time.Unix(secs, 0).UTC()
	}

	toDL, haveToDL := root.Int("BytesToDownload")
	dl, haveDL := root.Int("BytesDownloaded")
	m.BytesToDownload, m.BytesDownloaded = toDL, dl
	m.HasDownloadPair = haveToDL && haveDL

	toStage, haveToStage := root.Int("BytesToStage")
	staged, haveStaged := root.Int("BytesStaged")
	m.BytesToStage, m.BytesStaged = toStage, staged
	m.HasStagePair = haveToStage && haveStaged

	return m, nil
}
