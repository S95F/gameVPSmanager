package steamcmd

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/provider"
)

// SteamCMD lies about its exit code, and this file is the answer.
//
// A run can print
//
//	Error! App '380870' state is 0x602 after update job.
//
// and then EXIT 0. It self-updates and re-execs on many runs, and with
// `+`-style arguments a failed `app_update` still falls through to `+quit`.
// ValveSoftware/steam-for-linux#4341 asked for an exit-code table and was never
// answered; LinuxGSM's production answer is to tee stdout and grep it, using $?
// only as a secondary signal. gvm does the same, because the alternative is an
// installer that reports a successful install of a broken game: the server then
// fails to boot for reasons that look nothing like a download problem, and the
// operator debugs the wrong layer for hours.
//
// So the verdict comes from STDOUT, and the exit status is only consulted when
// stdout said nothing recognisable. Every rule here is pure and runs over the
// golden fixtures in testdata on the development machine.
//
// Classification is a ladder, not a verdict (section 13.5): hard-fail (nothing
// about retrying can help, and for the credential cases retrying actively
// harms), retry with backoff (a content-server or disk-write blip), and the
// in-progress states, which are ordinary mid-download noise and must never be
// mistaken for failures.

// Class is what the output says about retrying.
type Class uint8

const (
	// ClassOK is "nothing in the output says this failed". It is NOT "the
	// install is good" -- only the appmanifest can say that, which is why
	// verify.go runs after every single app_update (D-32).
	ClassOK Class = iota
	// ClassRetry is a transient failure: retry with backoff.
	ClassRetry
	// ClassHardFail is terminal. Retrying a credential failure is worse than
	// pointless: Steam rate-limits and then locks the account.
	ClassHardFail
)

func (c Class) String() string {
	switch c {
	case ClassOK:
		return "ok"
	case ClassRetry:
		return "retry"
	case ClassHardFail:
		return "hard-fail"
	}
	return "class(" + strconv.Itoa(int(c)) + ")"
}

// The two outcomes of a classified run, as sentinels so the retry ladder asks
// errors.Is rather than comparing strings.
var (
	ErrHardFail = errors.New("steamcmd: the run failed in a way retrying cannot fix")
	ErrRetry    = errors.New("steamcmd: the run failed transiently")
)

// Retry policy, from section 13.5. The bash suite had no retry at all; three
// attempts is where a Steam outage is an outage rather than a blip, and
// "start the server anyway and tell the operator" is the right response to an
// outage.
const MaxAttempts = 3

var backoff = [...]time.Duration{30 * time.Second, 2 * time.Minute, 8 * time.Minute}

// Backoff is how long to wait before attempt n (1-based: Backoff(1) is the wait
// after the first failure).
func Backoff(attempt int) time.Duration {
	switch {
	case attempt < 1:
		return 0
	case attempt > len(backoff):
		return backoff[len(backoff)-1]
	}
	return backoff[attempt-1]
}

// Exit is how the steamcmd process ended, as the sandbox reports it.
//
// Signaled and Timeout are separate from Code because they are not steamcmd's
// verdict at all: they are gvm's, and the correct response differs. A code of 0
// after an Error! line is the trap this file exists for; a SIGKILL from
// RuntimeMaxSec is a budget gvm chose.
type Exit struct {
	Code     int
	Signaled bool
	Timeout  bool
}

// OK reports a clean process exit, which is a far weaker claim than ClassOK.
func (e Exit) OK() bool { return e.Code == 0 && !e.Signaled && !e.Timeout }

func (e Exit) String() string {
	switch {
	case e.Timeout:
		return "killed on gvm's time budget"
	case e.Signaled:
		return "killed by a signal"
	}
	return "exit status " + strconv.Itoa(e.Code)
}

// Finding is one classified line of output.
type Finding struct {
	Class Class
	// Code is the steamcmd state code ("0x602"), canonicalised to upper-case
	// hex with no leading zeros, or "" for a phrase match.
	Code string
	// Phrase is the matched text for a phrase rule, or "".
	Phrase string
	Line   int
	// Text is the line, trimmed. It is quoted into the error, because the
	// operator will search the web for it.
	Text string
	// Why is the sentence that explains the code. SteamCMD's codes are
	// undocumented by Valve, so this is the only place an operator can learn
	// what 0x626 means without leaving the output.
	Why string
}

func (f Finding) String() string {
	id := f.Code
	if id == "" {
		id = f.Phrase
	}
	return fmt.Sprintf("%s: %s (%s) in %q", f.Class, id, f.Why, f.Text)
}

// Verdict is the whole run's reading.
type Verdict struct {
	Class    Class
	Findings []Finding
	// Progress lists the in-progress states seen, in first-seen order. It is
	// kept because "we saw 0x61 downloading and then nothing" and "we never
	// connected at all" are different outages, and the job log should say which.
	Progress []string
	// Installed is a `Success! App '…' fully installed` line, AlreadyCurrent an
	// `already up to date` one. Both are claims by steamcmd, not verdicts: the
	// appmanifest check is what decides (D-32).
	Installed      bool
	AlreadyCurrent bool
	Exit           Exit
	// Tail is the last lines of output, for the job record. Bounded, because a
	// 65 GB download prints progress lines forever.
	Tail []string
	// Lines is how many lines were read, so an empty Tail can be told apart
	// from a run that printed nothing at all.
	Lines int
}

// OK reports whether the output contains nothing that says the run failed.
func (v Verdict) OK() bool { return v.Class == ClassOK }

// Terminal reports whether retrying is pointless.
func (v Verdict) Terminal() bool { return v.Class == ClassHardFail }

// Top is the finding the message should lead with: the first hard fail if there
// is one, otherwise the first retryable finding.
func (v Verdict) Top() (Finding, bool) {
	for _, f := range v.Findings {
		if f.Class == ClassHardFail {
			return f, true
		}
	}
	for _, f := range v.Findings {
		if f.Class == ClassRetry {
			return f, true
		}
	}
	return Finding{}, false
}

// Err turns the verdict into an error, or nil for ClassOK.
//
// It wraps ErrHardFail or ErrRetry so the ladder can branch on errors.Is, and
// it names the exit status alongside the finding precisely because the two
// disagree so often: "exit status 0" next to "0x602" is the sentence that
// teaches an operator not to trust the exit code either.
func (v Verdict) Err() error {
	if v.Class == ClassOK {
		return nil
	}
	base := ErrRetry
	if v.Class == ClassHardFail {
		base = ErrHardFail
	}
	top, ok := v.Top()
	if !ok {
		return fmt.Errorf("%w: steamcmd ended with %s and printed nothing gvm recognises",
			base, v.Exit)
	}
	id := top.Code
	if id == "" {
		id = strconv.Quote(top.Phrase)
	}
	return fmt.Errorf("%w: %s (%s), and steamcmd reported %s: %q",
		base, id, top.Why, v.Exit, top.Text)
}

// stateCode is one of steamcmd's undocumented state codes.
type stateCode struct {
	class Class
	// progress marks a state that is ordinary mid-download noise. It is
	// separate from class==ClassOK because the same code appearing on an
	// `Error!` line is NOT noise -- see classifyLine.
	progress bool
	what     string
}

// The code table, from LinuxGSM's steamcmd/errors.md plus section 13.5.
//
// Keys are canonical: upper-case hex, no leading zeros, so "0x602" and "0x0602"
// are the same entry and a lower-case "0x6a6" cannot slip past.
var stateCodes = map[string]stateCode{
	"0x202": {class: ClassHardFail, what: "out of disk space or over quota"},
	"0x212": {class: ClassHardFail, what: "insufficient disk space"},

	"0x402": {class: ClassRetry, what: "the connection dropped and the update did not complete"},
	"0x602": {class: ClassRetry, what: "an update is required and did not complete"},
	"0x606": {class: ClassRetry, what: "disk write failure, usually a permission problem on the install tree"},
	"0x626": {class: ClassRetry, what: "update files are missing"},
	"0x6A6": {class: ClassRetry, what: "update files are corrupt"},
	"0x10E": {class: ClassRetry, what: "the content server flaked"},

	"0x3":   {progress: true, what: "reconfiguring"},
	"0x5":   {progress: true, what: "validating"},
	"0x6":   {progress: true, what: "no connection to the content servers yet"},
	"0x11":  {progress: true, what: "preallocating"},
	"0x61":  {progress: true, what: "downloading"},
	"0x101": {progress: true, what: "committing"},
}

// phraseRules are the plain-text failures, matched case-insensitively.
//
// Case-insensitively because steamcmd's own casing varies across versions and
// between the two places it reports a login failure, and a classifier that
// missed "no subscription" because the build said "No Subscription" would
// silently fall through to "retry" -- which for a credential failure is the
// response that locks the account.
//
// Order matters only for the message: the first matching rule of the winning
// class is what the error leads with.
var phraseRules = []struct {
	match string
	class Class
	what  string
}{
	{"no subscription", ClassHardFail,
		"this Steam account does not own the app, so it cannot install it; an anonymous login " +
			"cannot install a game that requires a purchase"},
	{"invalid platform", ClassHardFail,
		"this app has no depot for the requested platform; a Windows-only server needs " +
			"platform: windows plus a compatibility layer"},
	{"two-factor code mismatch", ClassHardFail,
		"Steam Guard rejected the code; an operator has to run one interactive steamcmd login " +
			"on the box to refresh the cached token"},
	{"invalid password", ClassHardFail,
		"the Steam account's password or cached refresh token is wrong; retrying locks the " +
			"account out"},
	{"cached credentials not found", ClassHardFail,
		"the cached Steam refresh token has expired, which happens silently months after it was " +
			"created; an operator has to log in interactively once"},
	{"rate limit exceeded", ClassHardFail,
		"Steam is rate-limiting logins for this account; retrying extends the lockout"},
	{"password check for appid", ClassHardFail,
		"the -betapassword for this private branch is wrong"},
	{"please use force_install_dir before logon", ClassHardFail,
		"steamcmd was given force_install_dir after login, so the app may have installed into " +
			"Steam's own default directory instead of the instance's install tree. gvm generates " +
			"this script from a struct that always orders it correctly, so seeing this is a gvm bug"},
	{"failed to install app", ClassRetry,
		"steamcmd could not install the app and did not say why in terms gvm recognises"},
	{"disk write failure", ClassRetry,
		"steamcmd could not write to the install tree"},
	{"timeout downloading", ClassRetry,
		"a content server stalled"},
}

// hexCode matches a state code. The bound is deliberate: an unbounded run of
// hex digits in a mod name or a path should not be read as a state code.
var hexCode = regexp.MustCompile(`0[xX][0-9a-fA-F]{1,8}`)

// maxFindings bounds what one run can accumulate. A loop that prints the same
// error ten thousand times is one finding repeated, and the tenth copy teaches
// nobody anything.
const maxFindings = 32

// tailLines is how much output the job record keeps.
const tailLines = 40

// maxLine bounds one line. SteamCMD's progress output is written with carriage
// returns and no newline for minutes at a time, so "one line" can be
// megabytes; the classifier needs the text, not all of it.
const maxLine = 64 << 10

// Classifier reads steamcmd's output as it arrives.
//
// It is an io.Writer so the driver can hand it straight to the sandbox
// alongside the console ring and the log file, and so classification does not
// require buffering a whole run: a hard failure in the first ten lines of a
// multi-hour download is found and reported from the same pass that tails it.
// That is also why it cannot be "keep the last N lines and grep at the end" --
// "No subscription" is printed early and the tail is all progress.
type Classifier struct {
	buf      []byte
	line     int
	dropping bool // the current line passed maxLine; drop the rest of it
	// expectUpdate marks a run that was supposed to move bytes, which is what
	// makes "no outcome line and no download state" a failure rather than the
	// ordinary shape of an app_info_print.
	expectUpdate bool

	findings []Finding
	seen     map[string]bool
	progress []string
	seenProg map[string]bool
	tail     []string

	installed      bool
	alreadyCurrent bool
	dropped        int
}

// NewClassifier returns a Classifier ready to be written to.
func NewClassifier() *Classifier {
	return &Classifier{seen: map[string]bool{}, seenProg: map[string]bool{}}
}

// NewAppUpdateClassifier is the classifier for a run that was supposed to
// download something.
//
// It adds one rule the metadata read cannot have: a run that printed no
// outcome line AND saw no download state at all did not perform an app_update,
// whatever it exited with. That is the shape "steamcmd started, could not read
// its runscript and exited 0" produces -- the banner and the API load are
// printed whatever happens, so "it printed nothing" is not the signal. All
// three signals have to be absent at once, which is why a Valve wording change
// to one of them cannot trip it.
func NewAppUpdateClassifier() *Classifier {
	c := NewClassifier()
	c.expectUpdate = true
	return c
}

// Write consumes output. It never returns an error, because a classifier that
// could fail a write would abort the download it is only watching.
func (c *Classifier) Write(p []byte) (int, error) {
	for _, b := range p {
		switch {
		case b == '\n' || b == '\r':
			// Both terminate a line, and a \r\n pair yields one empty line that
			// costs nothing. SteamCMD overwrites its progress line with \r, so a
			// reader that split on \n alone would hold the entire download in
			// one "line".
			c.flush()
		case c.dropping:
			continue
		case len(c.buf) >= maxLine:
			// Keep the head, drop the rest until the line ends. The head is
			// where "No subscription" lives; the tail of an over-long line is
			// always progress bytes.
			c.dropping = true
		default:
			c.buf = append(c.buf, b)
		}
	}
	return len(p), nil
}

func (c *Classifier) flush() {
	line := string(c.buf)
	c.buf = c.buf[:0]
	trunc := c.dropping
	c.dropping = false
	if strings.TrimSpace(line) == "" {
		return
	}
	c.line++
	if trunc {
		line += " …[truncated]"
	}
	c.consume(line)
}

func (c *Classifier) consume(line string) {
	text := strings.TrimSpace(line)
	c.tail = append(c.tail, text)
	if len(c.tail) > tailLines {
		c.tail = c.tail[len(c.tail)-tailLines:]
	}

	lower := strings.ToLower(text)
	if strings.Contains(lower, "success! app") {
		c.installed = c.installed || strings.Contains(lower, "fully installed")
		c.alreadyCurrent = c.alreadyCurrent || strings.Contains(lower, "already up to date")
	}

	for _, f := range classifyLine(text, c.line) {
		if f.Class == ClassOK {
			if f.Code != "" && !c.seenProg[f.Code] {
				c.seenProg[f.Code] = true
				c.progress = append(c.progress, f.Code+" "+f.Why)
			}
			continue
		}
		key := f.Class.String() + "|" + f.Code + "|" + f.Phrase
		if c.seen[key] {
			continue
		}
		if len(c.findings) >= maxFindings {
			c.dropped++
			continue
		}
		c.seen[key] = true
		c.findings = append(c.findings, f)
	}
}

// Verdict closes the reading and applies the exit status.
//
// The exit status is consulted ONLY when stdout said nothing recognisable,
// which is the whole lesson of this file. A non-zero exit with no finding is
// retried once and then reported, because steamcmd's exit codes are
// undocumented and the one published data point (exit 8 beside 0x602) is a
// transient.
func (c *Classifier) Verdict(exit Exit) Verdict {
	c.flush()

	v := Verdict{
		Findings:       c.findings,
		Progress:       c.progress,
		Installed:      c.installed,
		AlreadyCurrent: c.alreadyCurrent,
		Exit:           exit,
		Tail:           c.tail,
		Lines:          c.line,
	}
	for _, f := range c.findings {
		if f.Class > v.Class {
			v.Class = f.Class
		}
	}
	if v.Class != ClassOK {
		return v
	}

	if c.expectUpdate && !c.installed && !c.alreadyCurrent && len(c.progress) == 0 {
		v.Class = ClassRetry
		v.Findings = append(v.Findings, Finding{
			Class: ClassRetry, Phrase: "no app_update", Line: c.line,
			Text: "steamcmd printed no outcome line and no download state",
			Why: "steamcmd never ran app_update: it printed neither `fully installed` nor " +
				"`already up to date` and never reported a download state, which is what a run " +
				"that could not read its runscript looks like. Check that the runscript is " +
				"readable by the account the unit runs as",
		})
		return v
	}

	switch {
	case exit.Timeout:
		v.Class = ClassRetry
		v.Findings = append(v.Findings, Finding{
			Class: ClassRetry, Phrase: "timeout", Line: c.line,
			Text: "steamcmd ran past gvm's time budget",
			Why:  "the run was killed on gvm's own budget, not by Steam",
		})
	case exit.Signaled:
		v.Class = ClassRetry
		v.Findings = append(v.Findings, Finding{
			Class: ClassRetry, Phrase: "signal", Line: c.line,
			Text: "steamcmd was killed by a signal",
			Why:  "the process did not exit on its own, so nothing it printed is a verdict",
		})
	case exit.Code != 0:
		v.Class = ClassRetry
		v.Findings = append(v.Findings, Finding{
			Class: ClassRetry, Phrase: "exit", Line: c.line,
			Text: "steamcmd exited " + strconv.Itoa(exit.Code),
			Why: "steamcmd printed nothing gvm recognises and Valve publishes no exit-code " +
				"table, so this is treated as transient and verified on disk either way",
		})
	}
	return v
}

// classifyLine is the whole rule set for one line.
func classifyLine(text string, lineNo int) []Finding {
	lower := strings.ToLower(text)
	// An error line is what makes a state code a verdict. SteamCMD prints the
	// same codes twice over: `Update state (0x61) downloading` is progress,
	// while `Error! App '380870' state is 0x602` is the failure. Reading the
	// first as a failure would abort every normal download.
	isError := strings.Contains(lower, "error!")

	var out []Finding
	for _, raw := range hexCode.FindAllString(text, -1) {
		code, ok := canonCode(raw)
		if !ok {
			continue
		}
		entry, known := stateCodes[code]
		switch {
		case known && entry.progress && !isError:
			out = append(out, Finding{Class: ClassOK, Code: code, Line: lineNo, Text: text, Why: entry.what})
		case known && entry.progress && isError:
			// A transient state reported on an error line is a failed job that
			// stopped in that state, not progress.
			out = append(out, Finding{
				Class: ClassRetry, Code: code, Line: lineNo, Text: text,
				Why: "the job stopped while " + entry.what,
			})
		case known:
			out = append(out, Finding{Class: entry.class, Code: code, Line: lineNo, Text: text, Why: entry.what})
		case isError:
			out = append(out, Finding{
				Class: ClassRetry, Code: code, Line: lineNo, Text: text,
				Why: "a state code gvm has no entry for; Valve documents none of them, so it is " +
					"retried and then reported verbatim",
			})
		}
	}

	for _, rule := range phraseRules {
		if strings.Contains(lower, rule.match) {
			out = append(out, Finding{
				Class: rule.class, Phrase: rule.match, Line: lineNo, Text: text, Why: rule.what,
			})
		}
	}

	// A bare `Error!` with nothing recognisable on it is still a failure.
	if isError && len(out) == 0 {
		out = append(out, Finding{
			Class: ClassRetry, Phrase: "error!", Line: lineNo, Text: text,
			Why: "steamcmd reported an error gvm cannot classify",
		})
	}
	return out
}

// canonCode normalises a matched hex token to the table's spelling.
func canonCode(raw string) (string, bool) {
	v, err := strconv.ParseUint(raw[2:], 16, 64)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("0x%X", v), true
}

// Classify reads a whole stream and classifies it. This is the form the
// fixtures are tested through; the driver uses a Classifier directly so it can
// classify a running download.
func Classify(r io.Reader, exit Exit) (Verdict, error) {
	c := NewClassifier()
	if r != nil {
		if _, err := io.Copy(c, r); err != nil {
			return c.Verdict(exit), fmt.Errorf("steamcmd: read output: %w", err)
		}
	}
	return c.Verdict(exit), nil
}

// Terminal reports whether re-running the JOB could change this error.
//
// provider.Terminal is the shared classifier and this is the steamcmd-shaped
// extension of it, kept here for the reason section 13.5 gives: the hex codes
// are this driver's knowledge and provider has no business learning them. The
// driver's own ladder (runUpdate) has already retried what was worth retrying
// by the time an error comes out of Apply, so everything below is terminal with
// respect to the job:
//
//   - ErrHardFail: a credential or disk failure. Re-running locks the account
//     or fills the disk again.
//   - ErrRetry: the three attempts are spent. A nightly timer will try again
//     tomorrow; re-running the job now is the same three attempts.
//   - the verification failures: D-32 forbids auto-remediation, so the next
//     step is `gvm update <n> --repair` and not another app_update.
//   - ErrNoRunner, ErrNoSteamCMD, ErrManifest, ErrCredentialed: nothing about
//     retrying supplies a sandbox, a binary, a manifest or an account.
func Terminal(err error) bool {
	switch {
	case err == nil:
		return false
	case provider.Terminal(err):
		return true
	case errors.Is(err, ErrHardFail), errors.Is(err, ErrRetry),
		errors.Is(err, ErrNoAppManifest), errors.Is(err, ErrManyAppManifests),
		errors.Is(err, ErrStateFlags), errors.Is(err, ErrBytesDownloaded),
		errors.Is(err, ErrBytesStaged), errors.Is(err, ErrSentinel),
		errors.Is(err, ErrWrongBuild), errors.Is(err, ErrUndeclaredCheck),
		errors.Is(err, ErrBuildID),
		errors.Is(err, ErrNoRunner), errors.Is(err, ErrNoSteamCMD),
		errors.Is(err, ErrManifest), errors.Is(err, ErrCredentialed),
		errors.Is(err, ErrRunscript), errors.Is(err, ErrNoSuchBranch),
		errors.Is(err, ErrNotAppManifest), errors.Is(err, ErrPurgeRoot):
		return true
	}
	// Everything else -- a sandbox that could not start, an unreadable
	// appinfo.vdf, a context deadline -- is infrastructure, and the job may
	// legitimately run again.
	return false
}

// KnownStateCodes lists the codes this build classifies, sorted, for `gvm
// doctor` and for the test that keeps the table and section 13.5 in agreement.
func KnownStateCodes() []string {
	out := make([]string, 0, len(stateCodes))
	for k := range stateCodes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
