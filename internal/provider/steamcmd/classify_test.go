package steamcmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/provider"
)

func classifyFixture(t *testing.T, name string, exit Exit) Verdict {
	t.Helper()
	v, err := Classify(strings.NewReader(string(fixture(t, name))), exit)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

// The golden fixtures, with the exit statuses the real tool reports alongside
// them. The 0x602 row is the whole reason this package exists.
func TestClassifyGoldenFixtures(t *testing.T) {
	cases := []struct {
		file      string
		exit      Exit
		want      Class
		wantCodes []string
		wantPhase string // a substring of the leading finding's explanation
		installed bool
	}{
		{
			file: "steamcmd_ok.stdout", exit: Exit{Code: 0}, want: ClassOK,
			installed: true,
		},
		{
			// EXIT 0, after Error! … 0x602. An installer that trusted the exit
			// code would report this as a successful install.
			file: "steamcmd_0x602.stdout", exit: Exit{Code: 0}, want: ClassRetry,
			wantCodes: []string{"0x602"}, wantPhase: "update is required",
		},
		{
			file: "steamcmd_0x202.stdout", exit: Exit{Code: 8}, want: ClassHardFail,
			wantCodes: []string{"0x202"}, wantPhase: "disk",
		},
		{
			file: "steamcmd_no_subscription.stdout", exit: Exit{Code: 1}, want: ClassHardFail,
			wantPhase: "does not own the app",
		},
		{
			file: "steamcmd_2fa.stdout", exit: Exit{Code: 1}, want: ClassHardFail,
			wantPhase: "Steam Guard",
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			v := classifyFixture(t, tc.file, tc.exit)
			if v.Class != tc.want {
				t.Fatalf("class = %s, want %s (findings: %v)", v.Class, tc.want, v.Findings)
			}
			if v.Installed != tc.installed {
				t.Errorf("Installed = %t, want %t", v.Installed, tc.installed)
			}
			for _, code := range tc.wantCodes {
				if !slices.ContainsFunc(v.Findings, func(f Finding) bool { return f.Code == code }) {
					t.Errorf("no finding for %s: %v", code, v.Findings)
				}
			}
			if tc.wantPhase != "" {
				top, ok := v.Top()
				if !ok {
					t.Fatal("no leading finding")
				}
				if !strings.Contains(top.Why, tc.wantPhase) {
					t.Errorf("the leading finding explains %q, want something about %q",
						top.Why, tc.wantPhase)
				}
			}
			switch tc.want {
			case ClassOK:
				if v.Err() != nil {
					t.Errorf("Err() = %v for an ok run", v.Err())
				}
			case ClassRetry:
				if !errors.Is(v.Err(), ErrRetry) {
					t.Errorf("Err() = %v, want ErrRetry", v.Err())
				}
				if v.Terminal() {
					t.Error("a retryable verdict reports terminal")
				}
			case ClassHardFail:
				if !errors.Is(v.Err(), ErrHardFail) {
					t.Errorf("Err() = %v, want ErrHardFail", v.Err())
				}
				if !v.Terminal() {
					t.Error("a hard failure does not report terminal")
				}
			}
		})
	}
}

// Stated on its own, because it is the trap: the exit code says success and the
// output says otherwise, and the output wins.
func TestExitZeroAfterAnErrorIsNotSuccess(t *testing.T) {
	v := classifyFixture(t, "steamcmd_0x602.stdout", Exit{Code: 0})
	if v.OK() {
		t.Fatal("a run that printed `Error! … 0x602` and exited 0 was classified as ok; an " +
			"installer that does this reports a successful install of a broken game")
	}
	if !v.Exit.OK() {
		t.Fatal("the fixture's exit status is meant to be a clean 0, or this test proves nothing")
	}
	// And the message has to put the two next to each other, because that
	// contradiction is the thing an operator needs to see.
	msg := v.Err().Error()
	if !strings.Contains(msg, "0x602") || !strings.Contains(msg, "exit status 0") {
		t.Errorf("the error does not show both halves of the contradiction: %q", msg)
	}
}

// The in-progress codes appear in every healthy download. Reading one as a
// failure would abort every install.
func TestInProgressStatesAreNotFailures(t *testing.T) {
	v := classifyFixture(t, "steamcmd_ok.stdout", Exit{Code: 0})
	if !v.OK() {
		t.Fatalf("a clean install was classified %s: %v", v.Class, v.Findings)
	}
	if len(v.Findings) != 0 {
		t.Errorf("findings on a clean run: %v", v.Findings)
	}
	joined := strings.Join(v.Progress, " ")
	for _, want := range []string{"0x3", "0x11", "0x61", "0x101"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress states = %v, missing %s", v.Progress, want)
		}
	}
	if len(v.Progress) != 4 {
		t.Errorf("progress states = %v; each state should be recorded once however many lines "+
			"carry it", v.Progress)
	}
}

func TestClassifyExitStatusIsOnlyASecondarySignal(t *testing.T) {
	cases := []struct {
		name string
		exit Exit
		want Class
	}{
		{"a clean exit", Exit{Code: 0}, ClassOK},
		{"a non-zero exit nothing explains", Exit{Code: 8}, ClassRetry},
		{"a timeout", Exit{Code: -1, Timeout: true}, ClassRetry},
		{"a signal", Exit{Code: -1, Signaled: true}, ClassRetry},
	}
	// Output with nothing recognisable in it at all.
	const bland = "Steam Console Client (c) Valve Corporation - version 1757457223\n" +
		"Loading Steam API...OK\n"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Classify(strings.NewReader(bland), tc.exit)
			if err != nil {
				t.Fatal(err)
			}
			if v.Class != tc.want {
				t.Fatalf("class = %s, want %s", v.Class, tc.want)
			}
			if tc.want == ClassRetry {
				if len(v.Findings) == 0 {
					t.Fatal("a retry with no finding leaves the job log with nothing to print")
				}
				if !strings.Contains(v.Err().Error(), "exit-code table") &&
					!strings.Contains(v.Err().Error(), "killed") {
					t.Errorf("the finding does not explain itself: %v", v.Err())
				}
			}
		})
	}
}

func TestClassifyLineRules(t *testing.T) {
	cases := []struct {
		name string
		line string
		want Class
		code string
	}{
		{"an unknown code on an error line", "Error! App '4020' state is 0x9F2 after update job.",
			ClassRetry, "0x9F2"},
		{"a bare Error!", "Error! Something went wrong somewhere.", ClassRetry, ""},
		{"a transient state ON an error line is not progress",
			"Error! App '380870' state is 0x61 after update job.", ClassRetry, "0x61"},
		{"lower-case hex is the same code", "Error! App 'x' state is 0x6a6 after update job.",
			ClassRetry, "0x6A6"},
		{"leading zeros are the same code", "Error! App 'x' state is 0x0202 after update job.",
			ClassHardFail, "0x202"},
		{"invalid platform", "ERROR! Failed to install app '1829350' (Invalid platform)",
			ClassHardFail, ""},
		{"a wrong beta password", "Password check for AppId 380870 failed", ClassHardFail, ""},
		{"an expired cached credential", "Cached credentials not found", ClassHardFail, ""},
		{"a login rate limit", "FAILED (Rate Limit Exceeded)", ClassHardFail, ""},
		{"disk quota", "Error! App '294420' state is 0x212 after update job.",
			ClassHardFail, "0x212"},
		{"a content server flake", "Error! App '90' state is 0x10E after update job.",
			ClassRetry, "0x10E"},
		{"progress", " Update state (0x61) downloading, progress: 2.00 (1 / 2)", ClassOK, ""},
		{"no connection yet", " Update state (0x6) no connection", ClassOK, ""},

		// The four rules nothing exercised. Each line is chosen so that it
		// matches ONLY the rule under test: no hex code, and no `ERROR!`
		// alongside a code, or it would prove something about a different rule.
		{"a wrong password or stale refresh token",
			"FAILED login with user 'gvm-steam' : Invalid Password", ClassHardFail, ""},
		{"a disk write failure named in words", "Error! Disk write failure", ClassRetry, ""},
		{"a stalled content server", "Timeout downloading item", ClassRetry, ""},
		{"failed to install app with no reason gvm knows",
			"Failed to install app '380870'", ClassRetry, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Classify(strings.NewReader(tc.line+"\n"), Exit{})
			if err != nil {
				t.Fatal(err)
			}
			if v.Class != tc.want {
				t.Fatalf("class = %s, want %s (%v)", v.Class, tc.want, v.Findings)
			}
			if tc.code != "" {
				top, _ := v.Top()
				if top.Code != tc.code {
					t.Errorf("code = %q, want %q", top.Code, tc.code)
				}
			}
		})
	}
}

// A run that was supposed to download something and printed nothing that says
// anything did.
//
// "It printed nothing at all" is not the signal: steamcmd prints its banner and
// "Loading Steam API" whatever happens, including when it cannot open the
// runscript it was pointed at. The signal is that all three traces a real
// app_update leaves -- an outcome line, an already-up-to-date claim, and at
// least one `Update state (0x..)` -- are absent at once, which is why no single
// Valve wording change can trip this.
func TestARunThatNeverRanAppUpdateIsNotClassOK(t *testing.T) {
	const banner = "Redirecting stderr to '/var/lib/gvm/.../stderr.txt'\n" +
		"Steam Console Client (c) Valve Corporation - version 1757457223\n" +
		"-- type 'quit' to exit --\n" +
		"Loading Steam API...OK\n" +
		"Connecting anonymously to Steam Public...OK\n"

	t.Run("an app_update run", func(t *testing.T) {
		c := NewAppUpdateClassifier()
		if _, err := c.Write([]byte(banner)); err != nil {
			t.Fatal(err)
		}
		v := c.Verdict(Exit{Code: 0})
		if v.OK() {
			t.Fatalf("an exit-0 run of pure banner output was classified %s; that is what "+
				"`steamcmd could not read its runscript` looks like, and ClassOK makes it a "+
				"silent no-op reported as a successful update", v.Class)
		}
		if v.Class != ClassRetry {
			t.Errorf("class = %s, want ClassRetry", v.Class)
		}
		if v.Lines == 0 {
			t.Error("the fixture is meant to print the banner, or this test proves nothing about " +
				"the `no output at all` case being the wrong signal")
		}
		top, ok := v.Top()
		if !ok || !strings.Contains(top.Why, "runscript") {
			t.Errorf("the finding does not point at the runscript: %+v", top)
		}
	})

	t.Run("a metadata run is untouched", func(t *testing.T) {
		// app_info_print prints no outcome line and no download state by
		// design, so the plain classifier must not learn this rule.
		v, err := Classify(strings.NewReader(banner), Exit{Code: 0})
		if err != nil {
			t.Fatal(err)
		}
		if !v.OK() {
			t.Fatalf("a metadata read was classified %s: %v", v.Class, v.Findings)
		}
	})

	t.Run("one download state is enough", func(t *testing.T) {
		c := NewAppUpdateClassifier()
		if _, err := c.Write([]byte(banner +
			" Update state (0x61) downloading, progress: 2.00 (1 / 2)\n")); err != nil {
			t.Fatal(err)
		}
		if v := c.Verdict(Exit{Code: 0}); !v.OK() {
			t.Fatalf("a run that downloaded and printed no Success line was classified %s; the "+
				"appmanifest is what decides that one, not the absence of a sentence",
				v.Class)
		}
	})
}

// gvm always writes force_install_dir before login, so this warning means the
// runscript generator broke -- and the install may have gone into Steam's own
// default directory.
func TestTheForceInstallDirWarningIsAHardFail(t *testing.T) {
	v, err := Classify(strings.NewReader("Please use force_install_dir before logon!\n"), Exit{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Class != ClassHardFail {
		t.Fatalf("class = %s", v.Class)
	}
	top, _ := v.Top()
	if !strings.Contains(top.Why, "gvm bug") {
		t.Errorf("the finding does not say whose bug it is: %q", top.Why)
	}
}

// SteamCMD overwrites its progress line with a carriage return and no newline.
// A classifier that split on \n alone would hold a whole download in one line
// and would not see the error at the end of it.
func TestCarriageReturnsSeparateLines(t *testing.T) {
	out := " Update state (0x61) downloading, progress: 1.00 (1 / 2)\r" +
		" Update state (0x61) downloading, progress: 2.00 (1 / 2)\r" +
		"Error! App '380870' state is 0x602 after update job.\r\n"
	v, err := Classify(strings.NewReader(out), Exit{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Class != ClassRetry {
		t.Fatalf("class = %s, findings %v", v.Class, v.Findings)
	}
	if v.Lines != 3 {
		t.Errorf("lines = %d, want 3", v.Lines)
	}
}

// The hard failure is printed early and the rest of the output is progress, so
// a "keep the last N lines and grep at the end" classifier would miss it.
func TestAHardFailureEarlyInAHugeStreamIsStillFound(t *testing.T) {
	c := NewClassifier()
	fmt.Fprint(c, "ERROR! Failed to install app '223350' (No subscription)\n")
	for i := range 20000 {
		fmt.Fprintf(c, " Update state (0x61) downloading, progress: %d.00 (%d / 100000)\n",
			i%100, i)
	}
	v := c.Verdict(Exit{Code: 0})
	if v.Class != ClassHardFail {
		t.Fatalf("class = %s", v.Class)
	}
	if len(v.Tail) > tailLines {
		t.Errorf("the tail grew to %d lines", len(v.Tail))
	}
	if len(v.Findings) > maxFindings {
		t.Errorf("findings grew to %d", len(v.Findings))
	}
}

func TestClassifierSurvivesArbitraryChunkBoundaries(t *testing.T) {
	out := string(fixture(t, "steamcmd_0x602.stdout"))
	c := NewClassifier()
	for i := 0; i < len(out); i++ {
		if _, err := c.Write([]byte{out[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if v := c.Verdict(Exit{Code: 0}); v.Class != ClassRetry {
		t.Fatalf("byte-at-a-time classification = %s, want retry", v.Class)
	}
}

func TestAnUnterminatedFinalLineIsStillClassified(t *testing.T) {
	c := NewClassifier()
	fmt.Fprint(c, "Error! App '380870' state is 0x602 after update job.")
	if v := c.Verdict(Exit{Code: 0}); v.Class != ClassRetry {
		t.Fatalf("class = %s; the last line of a killed run has no newline", v.Class)
	}
}

func TestAnEnormousLineIsTruncatedNotAccumulated(t *testing.T) {
	c := NewClassifier()
	// A progress line written with no terminator for megabytes.
	fmt.Fprint(c, "Error! App '380870' state is 0x602 ")
	fmt.Fprint(c, strings.Repeat("x", 4*maxLine))
	fmt.Fprint(c, "\n")
	v := c.Verdict(Exit{Code: 0})
	if v.Class != ClassRetry {
		t.Fatalf("class = %s", v.Class)
	}
	if v.Lines != 1 {
		t.Errorf("lines = %d, want 1", v.Lines)
	}
	for _, f := range v.Findings {
		if len(f.Text) > maxLine+64 {
			t.Errorf("a finding carries %d bytes of text", len(f.Text))
		}
	}
}

func TestBackoffLadder(t *testing.T) {
	want := []time.Duration{30 * time.Second, 2 * time.Minute, 8 * time.Minute}
	for i, d := range want {
		if got := Backoff(i + 1); got != d {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, d)
		}
	}
	if got := Backoff(0); got != 0 {
		t.Errorf("Backoff(0) = %v", got)
	}
	if got := Backoff(99); got != want[len(want)-1] {
		t.Errorf("Backoff(99) = %v, want the last rung %v", got, want[len(want)-1])
	}
	if MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d; section 13.5 says three", MaxAttempts)
	}
}

// The table and section 13.5 must not drift apart: this is the list the
// document gives, spelled out so a code that moves between classes has to move
// here too.
func TestTheCodeTableIsSection135sTable(t *testing.T) {
	want := map[string]Class{
		"0x202": ClassHardFail,
		"0x212": ClassHardFail,
		"0x402": ClassRetry,
		"0x602": ClassRetry,
		"0x606": ClassRetry,
		"0x626": ClassRetry,
		"0x6A6": ClassRetry,
		"0x10E": ClassRetry,
	}
	progress := []string{"0x3", "0x5", "0x6", "0x11", "0x61", "0x101"}

	for code, class := range want {
		entry, ok := stateCodes[code]
		if !ok {
			t.Errorf("%s is not in the table", code)
			continue
		}
		if entry.progress || entry.class != class {
			t.Errorf("%s is %s (progress %t), want %s", code, entry.class, entry.progress, class)
		}
		if entry.what == "" {
			t.Errorf("%s has no explanation, and Valve documents none of these", code)
		}
	}
	for _, code := range progress {
		entry, ok := stateCodes[code]
		if !ok {
			t.Errorf("%s is not in the table", code)
			continue
		}
		if !entry.progress {
			t.Errorf("%s is classified %s and section 13.5 calls it in-progress", code, entry.class)
		}
	}
	if got, want := len(KnownStateCodes()), len(want)+len(progress); got != want {
		t.Errorf("the table holds %d codes and this test knows about %d; add the new one here",
			got, want)
	}
}

func TestAnAlreadyCurrentRunIsNotAChange(t *testing.T) {
	v, err := Classify(strings.NewReader("Success! App '380870' already up to date.\n"), Exit{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK() {
		t.Fatalf("class = %s", v.Class)
	}
	if !v.AlreadyCurrent {
		t.Error("`already up to date` was not recognised, so a no-op update would report a change")
	}
	if v.Installed {
		t.Error("`already up to date` reported as a fresh install")
	}
}

// The job layer asks one question of an error: run again, or stop and tell the
// operator. Getting it wrong on a credential failure locks the Steam account.
func TestTerminal(t *testing.T) {
	terminal := []struct {
		name string
		err  error
	}{
		{"a hard failure", ErrHardFail},
		{"a retry ladder that is spent", ErrRetry},
		{"StateFlags", ErrStateFlags},
		{"a short download", ErrBytesDownloaded},
		{"a short staging step", ErrBytesStaged},
		{"a missing sentinel", ErrSentinel},
		{"two appmanifests", ErrManyAppManifests},
		{"no appmanifest", ErrNoAppManifest},
		{"no sandbox", ErrNoRunner},
		{"no steamcmd", ErrNoSteamCMD},
		{"an unusable manifest", ErrManifest},
		{"a game needing an account", ErrCredentialed},
		{"a branch that does not exist", ErrNoSuchBranch},
		{"a runscript gvm will not write", ErrRunscript},
		// The shared sentinels still answer through provider.Terminal, so the
		// adopted refusal does not need a second entry in two tables.
		{"the adopted refusal", provider.ErrAdopted},
		{"an unauthorized plan", provider.ErrUnauthorized},
	}
	for _, tc := range terminal {
		t.Run(tc.name, func(t *testing.T) {
			if !Terminal(fmt.Errorf("wrapped: %w", tc.err)) {
				t.Errorf("%v is not terminal, so a job would run it again", tc.err)
			}
		})
	}

	// Infrastructure is not terminal: a sandbox that could not start today may
	// start tomorrow, and the nightly is the retry.
	for _, err := range []error{
		nil,
		errors.New("systemd-run: connection reset"),
		context.DeadlineExceeded,
	} {
		if Terminal(err) {
			t.Errorf("%v was called terminal", err)
		}
	}
}

// The phrase table, closed the way the hex table is.
//
// TestTheCodeTableIsSection135sTable does exactly this for the state codes,
// including the count assertion that forces a new entry to be declared here --
// and the phrase table, which is where the credential failures live, had
// nothing at all. Three of its eleven rules were exercised by no test and no
// fixture, and one of those is `invalid password`: without it a wrong-password
// run falls through to the exit-code arm, becomes ClassRetry, and the ladder
// logs in three times. classify.go's own header names the consequence -- "Steam
// rate-limits and then locks the account".
//
// NOT named after section 13.5, because the table is a deliberate SUPERSET of
// it and the name would claim provenance it does not have. 13.5 lists four
// hard-fail phrases and no retry phrases; the rest come from
// docs/research/steamcmd.md, and the three retry phrases from neither.
func TestThePhraseTableIsClosed(t *testing.T) {
	want := map[string]Class{
		// Section 13.5's four, verbatim.
		"no subscription":          ClassHardFail,
		"invalid platform":         ClassHardFail,
		"two-factor code mismatch": ClassHardFail,
		"password check for appid": ClassHardFail,

		// docs/research/steamcmd.md's evidence: the token expiry class
		// ("Cached Credentials Not Found", line 51), the login rate limit, the
		// wrong password, and Valve's own ordering warning (line 10).
		"invalid password":                          ClassHardFail,
		"cached credentials not found":              ClassHardFail,
		"rate limit exceeded":                       ClassHardFail,
		"please use force_install_dir before logon": ClassHardFail,

		// The three retry phrases, which come from neither document: they are
		// the driver's reading of what a transient failure prints when it
		// prints words instead of a hex code.
		"failed to install app": ClassRetry,
		"disk write failure":    ClassRetry,
		"timeout downloading":   ClassRetry,
	}

	byMatch := map[string]Class{}
	for _, r := range phraseRules {
		if r.what == "" {
			t.Errorf("the rule for %q has no explanation, and Valve documents none of this: the "+
				"`what` is the only place an operator learns what it means", r.match)
		}
		if r.match != strings.ToLower(r.match) {
			t.Errorf("the rule for %q is not lower-case, and classifyLine matches against a "+
				"lower-cased line, so it can never fire", r.match)
		}
		if _, dup := byMatch[r.match]; dup {
			t.Errorf("%q appears twice in the table", r.match)
		}
		byMatch[r.match] = r.class
	}
	for match, class := range want {
		got, ok := byMatch[match]
		if !ok {
			t.Errorf("%q is not in the table", match)
			continue
		}
		if got != class {
			t.Errorf("%q is %s, want %s", match, got, class)
		}
	}
	if len(phraseRules) != len(want) {
		t.Errorf("the table holds %d rules and this test knows about %d; add the new one here, "+
			"with where it came from", len(phraseRules), len(want))
	}
}

// A credential failure never becomes three logins.
//
// The unit test above cannot see the retry loop, and TestAHardFailureStopsAfter
// OneAttempt pins the ladder only through the no-subscription fixture -- whose
// line carries `ERROR!` and would be caught by the bare-Error! rule even with
// its own phrase rule gone. A wrong password carries no hex code and, in the
// spelling steamcmd uses, no `error!` either, so with the rule removed
// classifyLine returns nothing and Verdict falls to the exit-code arm:
// ClassRetry, three logins, 30s and 2m apart.
func TestAWrongPasswordStopsAfterOneAttempt(t *testing.T) {
	h := newHarness(t, true)
	h.fake.Outcomes = []FakeOutcome{{
		Stdout: string(fixture(t, "steamcmd_invalid_password.stdout")),
		Exit:   Exit{Code: 1},
	}}

	_, err := h.apply(h.plan(provider.IntentInstall))
	if !errors.Is(err, ErrHardFail) {
		t.Fatalf("err = %v, want ErrHardFail; retrying a credential failure is how Steam "+
			"rate-limits and then locks the account", err)
	}
	updates := 0
	for _, inv := range h.fake.Runs() {
		if inv.Purpose == "app_update" {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("app_update ran %d times after a wrong password", updates)
	}
	if len(h.slept) != 0 {
		t.Errorf("the ladder waited %v before giving up on a credential failure", h.slept)
	}
}
