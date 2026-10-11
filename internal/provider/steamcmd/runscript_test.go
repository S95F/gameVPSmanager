package steamcmd

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func lines(t *testing.T, r Runscript) []string {
	t.Helper()
	s, err := r.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func installScript(validate bool) Runscript {
	return Runscript{
		Intent:     provider.IntentInstall,
		InstallDir: "/opt/pzserver",
		Platform:   spec.PlatformLinux,
		Login:      Login{Anonymous: true},
		Apps:       []AppUpdate{{AppID: 380870, Validate: validate}},
	}
}

// The ordering is Valve's: the two @ directives, then force_install_dir, then
// login, then the work, then quit.
func TestRenderOrdering(t *testing.T) {
	got := lines(t, installScript(true))
	want := []string{
		"@ShutdownOnFailedCommand 1",
		"@NoPromptForPassword 1",
		"force_install_dir /opt/pzserver",
		"login anonymous",
		"app_update 380870 validate",
		"quit",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("script =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Without @ShutdownOnFailedCommand a failed app_update falls through to quit
// and steamcmd can exit 0. It is the first line of every script.
func TestEveryScriptShutsDownOnAFailedCommand(t *testing.T) {
	for _, r := range []Runscript{
		installScript(false),
		{Intent: provider.IntentVerify, Login: Login{Anonymous: true}, AppInfo: []int{380870}},
	} {
		got := lines(t, r)
		if got[0] != "@ShutdownOnFailedCommand 1" {
			t.Errorf("first line = %q", got[0])
		}
		if !slices.Contains(got, "@NoPromptForPassword 1") {
			t.Error("a script with no @NoPromptForPassword can hang on a prompt nobody can see")
		}
		if got[len(got)-1] != "quit" {
			t.Errorf("last line = %q", got[len(got)-1])
		}
	}
}

// `validate` is positional. `-validate` is parsed as an unknown option and
// silently does no validation, so a corrupted install looks like one that
// validated.
func TestValidateIsPositionalAndNeverAFlag(t *testing.T) {
	s, err := installScript(true).Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "app_update 380870 validate") {
		t.Errorf("script = %q", s)
	}
	if strings.Contains(s, "-validate") {
		t.Fatal("the script carries `-validate`, which steamcmd ignores")
	}

	plain, err := installScript(false).Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "validate") {
		t.Errorf("a non-validating script carries the keyword: %q", plain)
	}
}

// D-32, in the code that would have to write the word.
func TestValidateIsRefusedOnAnUpdate(t *testing.T) {
	r := installScript(true)
	r.Intent = provider.IntentUpdate
	_, err := r.Render()
	if !errors.Is(err, ErrRunscript) {
		t.Fatalf("err = %v, want ErrRunscript", err)
	}
	if !strings.Contains(err.Error(), "D-32") {
		t.Errorf("the refusal does not cite the decision: %v", err)
	}
}

func TestPlatformDirective(t *testing.T) {
	r := installScript(false)
	r.Platform = spec.PlatformWindows
	got := lines(t, r)
	if got[2] != "@sSteamCmdForcePlatformType windows" {
		t.Fatalf("line 3 = %q; the platform has to be set before the login", got[2])
	}
	// Linux is the default and emitting it on every script would put a line
	// that changes nothing into every diff.
	if slices.ContainsFunc(lines(t, installScript(false)), func(l string) bool {
		return strings.HasPrefix(l, "@sSteamCmdForcePlatformType")
	}) {
		t.Error("the linux platform emitted a directive")
	}
	r.Platform = "amiga"
	if _, err := r.Render(); !errors.Is(err, ErrRunscript) {
		t.Errorf("an unknown platform rendered: %v", err)
	}
}

func TestBranchAndBranchPassword(t *testing.T) {
	r := installScript(false)
	r.Apps[0].Branch = "unstable"
	r.Apps[0].BranchPassword = "s3cr3t"

	if !r.CarriesSecret() {
		t.Error("CarriesSecret is false for a script with a betapassword in it")
	}
	rendered, err := r.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "app_update 380870 -beta unstable -betapassword s3cr3t") {
		t.Errorf("script = %q", rendered)
	}

	described, err := r.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(described, "s3cr3t") {
		t.Fatal("Describe leaked the branch password, and Describe is what reaches the diff, " +
			"the job log and the audit row")
	}
	if !strings.Contains(described, spec.Redacted) {
		t.Errorf("Describe did not mask it with the shared redaction: %q", described)
	}
}

func TestRunscriptRefusals(t *testing.T) {
	cases := []struct {
		name string
		r    Runscript
		says string
	}{
		{
			name: "nothing to do",
			r:    Runscript{Login: Login{Anonymous: true}},
			says: "log in and quit",
		},
		{
			name: "app_update with no force_install_dir",
			r: Runscript{Intent: provider.IntentInstall, Login: Login{Anonymous: true},
				Apps: []AppUpdate{{AppID: 380870}}},
			says: "Steam's own default directory",
		},
		{
			name: "a relative install dir",
			r: Runscript{Intent: provider.IntentInstall, InstallDir: "pzserver",
				Login: Login{Anonymous: true}, Apps: []AppUpdate{{AppID: 380870}}},
			says: "not absolute",
		},
		{
			name: "a newline in the install dir",
			r: Runscript{Intent: provider.IntentInstall,
				InstallDir: "/opt/pz\nlogin anonymous\napp_update 4020",
				Login:      Login{Anonymous: true}, Apps: []AppUpdate{{AppID: 380870}}},
			says: "line break",
		},
		{
			name: "a space in the install dir",
			r: Runscript{Intent: provider.IntentInstall, InstallDir: "/opt/pz server",
				Login: Login{Anonymous: true}, Apps: []AppUpdate{{AppID: 380870}}},
			says: "whitespace",
		},
		{
			name: "no login at all",
			r: Runscript{Intent: provider.IntentInstall, InstallDir: "/opt/pz",
				Apps: []AppUpdate{{AppID: 380870}}},
			says: "must name the Steam account",
		},
		{
			name: "both anonymous and named",
			r: Runscript{Intent: provider.IntentInstall, InstallDir: "/opt/pz",
				Login: Login{Anonymous: true, User: "someone"},
				Apps:  []AppUpdate{{AppID: 380870}}},
			says: "both anonymous and named",
		},
		{
			name: "an injected login name",
			r: Runscript{Intent: provider.IntentInstall, InstallDir: "/opt/pz",
				Login: Login{User: "me\nquit"}, Apps: []AppUpdate{{AppID: 380870}}},
			says: "line break",
		},
		{
			name: "a branch name that is not one",
			r: Runscript{Intent: provider.IntentInstall, InstallDir: "/opt/pz",
				Login: Login{Anonymous: true},
				Apps:  []AppUpdate{{AppID: 380870, Branch: "beta; rm -rf /"}}},
			says: "is not a branch name",
		},
		{
			name: "a betapassword with whitespace",
			r: Runscript{Intent: provider.IntentInstall, InstallDir: "/opt/pz",
				Login: Login{Anonymous: true},
				Apps:  []AppUpdate{{AppID: 380870, BranchPassword: "two words"}}},
			says: "whitespace",
		},
		{
			name: "not an app id",
			r: Runscript{Intent: provider.IntentInstall, InstallDir: "/opt/pz",
				Login: Login{Anonymous: true}, Apps: []AppUpdate{{AppID: 0}}},
			says: "is not an app id",
		},
		{
			name: "an app_info for nothing",
			r:    Runscript{Login: Login{Anonymous: true}, AppInfo: []int{-1}},
			says: "is not an app id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.r.Render()
			if !errors.Is(err, ErrRunscript) {
				t.Fatalf("err = %v, want ErrRunscript", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want something about %q", err, tc.says)
			}
		})
	}
}

// A refusal must not quote the value it refused: one of the tokens checked is a
// password, and an error string is the least controllable place in the program.
func TestARefusedPasswordIsNotInTheMessage(t *testing.T) {
	r := installScript(false)
	r.Apps[0].BranchPassword = "hunter2 and a newline\n"
	_, err := r.Render()
	if err == nil {
		t.Fatal("rendered")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the refusal quotes the password: %v", err)
	}
}

// The CHECK phase's script, including the second login: LinuxGSM logs in twice
// before app_info_update because once is not reliably enough, and both halves
// of that workaround are deliberate.
func TestAppInfoScript(t *testing.T) {
	got := lines(t, Runscript{
		Intent:  provider.IntentVerify,
		Login:   Login{Anonymous: true},
		AppInfo: []int{380870},
	})
	want := []string{
		"@ShutdownOnFailedCommand 1",
		"@NoPromptForPassword 1",
		"login anonymous",
		"app_info_request 380870",
		"login anonymous",
		"app_info_update 1",
		"app_info_print 380870",
		"quit",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("script =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A check downloads nothing, so it needs no install directory.
	if slices.ContainsFunc(got, func(l string) bool {
		return strings.HasPrefix(l, "force_install_dir")
	}) {
		t.Error("the check script sets an install directory it will not write to")
	}
}

// The fake parses the same bytes the real tool would, so a script this package
// generates has to survive that parse. It is also where the fake's own refusals
// (a `-validate`, a force_install_dir after the login) are proven to apply to
// real output of Render.
func TestRenderedScriptsParseBackCleanly(t *testing.T) {
	for _, r := range []Runscript{
		installScript(true),
		installScript(false),
		{Intent: provider.IntentVerify, Login: Login{Anonymous: true}, AppInfo: []int{380870, 1007}},
	} {
		s, err := r.Render()
		if err != nil {
			t.Fatal(err)
		}
		call, err := ParseRunscript([]byte(s))
		if err != nil {
			t.Fatalf("the fake cannot read a script gvm generated: %v\n%s", err, s)
		}
		if call.DirAfterLogin {
			t.Error("force_install_dir landed after the login")
		}
		if !call.ShutdownOnFailed || !call.NoPrompt {
			t.Error("the @ directives did not survive the round trip")
		}
		if len(r.Apps) > 0 {
			if got := call.Updates[0].AppID; got != r.Apps[0].AppID {
				t.Errorf("app id = %d", got)
			}
			if call.Updates[0].Validate != r.Apps[0].Validate {
				t.Errorf("validate = %t, want %t", call.Updates[0].Validate, r.Apps[0].Validate)
			}
			if call.InstallDir != r.InstallDir {
				t.Errorf("install dir = %q", call.InstallDir)
			}
		}
		if len(r.AppInfo) > 0 && !slices.Equal(call.AppInfo, r.AppInfo) {
			t.Errorf("app_info = %v, want %v", call.AppInfo, r.AppInfo)
		}
	}
}

func TestTheFakeRefusesTheSilentlyWrongScripts(t *testing.T) {
	cases := []struct {
		name   string
		script string
		says   string
	}{
		{
			name: "-validate instead of validate",
			script: "@ShutdownOnFailedCommand 1\nforce_install_dir /opt/pz\nlogin anonymous\n" +
				"app_update 380870 -validate\nquit\n",
			says: "does NO validation",
		},
		{
			name: "no @ShutdownOnFailedCommand",
			script: "@NoPromptForPassword 1\nforce_install_dir /opt/pz\nlogin anonymous\n" +
				"app_update 380870\nquit\n",
			says: "fall through to quit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRunscript([]byte(tc.script)); err == nil {
				t.Fatal("the fake accepted it")
			} else if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want something about %q", err, tc.says)
			}
		})
	}
}

// Valve's ordering, caught by the fake rather than by a string match: a script
// with the login first makes the fake print the warning steamcmd prints, which
// classify.go calls a hard failure.
func TestTheFakeDetectsTheOrderingBug(t *testing.T) {
	call, err := ParseRunscript([]byte("@ShutdownOnFailedCommand 1\nlogin anonymous\n" +
		"force_install_dir /opt/pz\napp_update 380870\nquit\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !call.DirAfterLogin {
		t.Fatal("a force_install_dir after the login went unnoticed")
	}
}
