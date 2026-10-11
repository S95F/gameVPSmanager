package steamcmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The runscript, generated from a struct and never hand-edited.
//
// Script mode is more reliable than `+args` for one specific reason: without
// `@ShutdownOnFailedCommand 1`, a failed `app_update` still falls through to
// `+quit` and steamcmd can exit 0 (section 13.5, and the research's pitfall
// list). The directive is therefore not a nicety -- it is half of why the exit
// code is ever worth looking at.
//
// Three more facts are encoded here rather than remembered:
//
//   - `force_install_dir` comes BEFORE `login`. Valve's wiki says so and modern
//     steamcmd prints "Please use force_install_dir before logon!" when it does
//     not; there are field reports of it then installing into Steam's own
//     default directory instead. classify.go treats that warning as a hard
//     failure, because gvm always emits them in this order and seeing it means
//     this function broke.
//
//   - `validate` is POSITIONAL. `app_update 294420 -validate` is parsed as an
//     unknown option and silently does NO validation, so a corrupted install
//     looks like one that validated. It is the single most copy-pasted bug in
//     this area, and the only way to write it here is as a bool.
//
//   - a Steam account password is never written into this file. The operator
//     performs one interactive login on the box; steamcmd caches a refresh
//     token and `login <user>` with no password works afterwards.
//     `@NoPromptForPassword 1` turns an expired token into a fast, classified
//     failure instead of a process that hangs on a prompt nobody can see.

// ErrRunscript is a runscript gvm refuses to generate.
var ErrRunscript = errors.New("steamcmd: refusing to generate this runscript")

// RunscriptMode is the file's mode: readable only by the instance user it runs
// as, because a private-branch password may be in it (section 13.5).
const RunscriptMode = 0o600

// Login is how the run authenticates.
type Login struct {
	// Anonymous covers the large majority of dedicated servers.
	Anonymous bool
	// User is the Steam account for the handful that require a purchase (DayZ,
	// Terraria, Arma 3...). No password field exists; see the package comment.
	User string
}

// AppUpdate is one `app_update` line.
type AppUpdate struct {
	AppID  int
	Branch string
	// BranchPassword is an app's private-beta password -- not an account
	// credential. It is the one secret that can legitimately appear in the
	// generated file, which is why the file is 0600 and why Describe exists.
	BranchPassword string
	// Validate is the positional `validate` keyword. It overwrites files the
	// operator customised, so it is legal only for install, repair and verify
	// (D-32), and Render enforces that against Intent.
	Validate bool
}

// Runscript is one steamcmd invocation as data.
type Runscript struct {
	// Intent is carried so Render can refuse `validate` on an update. The ban
	// is already a load-time error on the manifest's validate_on list; this is
	// the second place it cannot happen, in the code that would have to write
	// the word.
	Intent provider.Intent
	// InstallDir is an absolute force_install_dir. Required whenever Apps is
	// non-empty: steamcmd resolves a relative one against its own directory,
	// and an absent one installs into Steam's default steamapps tree.
	InstallDir string
	// Platform is the manifest's platform. Anything other than "" or linux
	// emits @sSteamCmdForcePlatformType, which is how a Windows-only server
	// (V Rising, Enshrouded, ARK: Survival Ascended) is fetched onto a Linux
	// box for Proton.
	Platform string
	Login    Login
	Apps     []AppUpdate
	// AppInfo are the app ids to print metadata for: the CHECK phase, which
	// downloads nothing.
	AppInfo []int
}

// CarriesSecret reports whether the rendered file would contain a credential.
// Everything that logs, diffs or echoes a runscript asks this first.
func (r Runscript) CarriesSecret() bool {
	for _, a := range r.Apps {
		if a.BranchPassword != "" {
			return true
		}
	}
	return false
}

// Render produces the file's bytes.
func (r Runscript) Render() (string, error) { return r.render(false) }

// Describe produces the same script with any private-branch password masked.
//
// This is what reaches `gvm apply --diff`, the job log and the audit row.
// Render's output goes to a 0600 file owned by the instance user and nowhere
// else.
func (r Runscript) Describe() (string, error) { return r.render(true) }

func (r Runscript) render(mask bool) (string, error) {
	if len(r.Apps) == 0 && len(r.AppInfo) == 0 {
		return "", fmt.Errorf("%w: it would log in and quit", ErrRunscript)
	}
	switch {
	case r.Login.Anonymous && strings.TrimSpace(r.Login.User) != "":
		return "", fmt.Errorf("%w: the login is both anonymous and named (%q)",
			ErrRunscript, r.Login.User)
	case !r.Login.Anonymous && strings.TrimSpace(r.Login.User) == "":
		return "", fmt.Errorf("%w: no login; a manifest that is not anonymous must name the "+
			"Steam account", ErrRunscript)
	}
	if err := checkToken("login", r.Login.User); err != nil {
		return "", err
	}
	if p := r.Platform; p != "" && p != spec.PlatformLinux && p != spec.PlatformWindows &&
		p != spec.PlatformMacOS {
		return "", fmt.Errorf("%w: %q is not a steamcmd platform", ErrRunscript, p)
	}

	var b strings.Builder
	// Without this, a failed app_update falls through to quit and steamcmd can
	// exit 0. It is the first line of every script gvm writes.
	b.WriteString("@ShutdownOnFailedCommand 1\n")
	// And without this, an expired cached credential waits forever on a prompt
	// that no terminal is attached to.
	b.WriteString("@NoPromptForPassword 1\n")
	if r.Platform != "" && r.Platform != spec.PlatformLinux {
		b.WriteString("@sSteamCmdForcePlatformType " + r.Platform + "\n")
	}

	if len(r.Apps) > 0 {
		dir := strings.TrimSpace(r.InstallDir)
		if dir == "" {
			return "", fmt.Errorf("%w: app_update with no force_install_dir would install into "+
				"Steam's own default directory", ErrRunscript)
		}
		// filepath.IsAbs as well as the leading slash: the target is Linux, and
		// the development machine's install trees are Windows paths, so a rule
		// that only accepted "/..." would be a rule the tests could not run.
		if !strings.HasPrefix(dir, "/") && !filepath.IsAbs(dir) {
			return "", fmt.Errorf("%w: force_install_dir %q is not absolute; steamcmd resolves a "+
				"relative path against its own directory", ErrRunscript, dir)
		}
		if err := checkToken("force_install_dir", dir); err != nil {
			return "", err
		}
		// BEFORE login. Valve's documented ordering.
		b.WriteString("force_install_dir " + dir + "\n")
	}

	b.WriteString(r.loginLine() + "\n")

	for _, a := range r.Apps {
		line, err := r.appUpdateLine(a, mask)
		if err != nil {
			return "", err
		}
		b.WriteString(line + "\n")
	}

	for _, id := range r.AppInfo {
		if id <= 0 {
			return "", fmt.Errorf("%w: %d is not an app id", ErrRunscript, id)
		}
		b.WriteString("app_info_request " + strconv.Itoa(id) + "\n")
		// The second login is LinuxGSM's workaround, reproduced deliberately:
		// app_info_update after a single login returns the cached metadata often
		// enough that every production implementation logs in twice. It is a
		// no-op when it is not needed.
		b.WriteString(r.loginLine() + "\n")
		b.WriteString("app_info_update 1\n")
		b.WriteString("app_info_print " + strconv.Itoa(id) + "\n")
	}

	b.WriteString("quit\n")
	return b.String(), nil
}

func (r Runscript) loginLine() string {
	if r.Login.Anonymous {
		return "login anonymous"
	}
	return "login " + r.Login.User
}

func (r Runscript) appUpdateLine(a AppUpdate, mask bool) (string, error) {
	if a.AppID <= 0 {
		return "", fmt.Errorf("%w: %d is not an app id", ErrRunscript, a.AppID)
	}
	if a.Validate && r.Intent == provider.IntentUpdate {
		return "", fmt.Errorf("%w: `validate` on an update overwrites every file the operator "+
			"customised (D-32); the repair intent is the only one that may emit it", ErrRunscript)
	}
	if a.Branch != "" && !plausibleBranch(a.Branch) {
		return "", fmt.Errorf("%w: %q is not a branch name", ErrRunscript, a.Branch)
	}
	if err := checkToken("betapassword", a.BranchPassword); err != nil {
		// checkToken never puts the value in the message; see its comment.
		return "", err
	}

	line := "app_update " + strconv.Itoa(a.AppID)
	if a.Branch != "" {
		line += " -beta " + a.Branch
	}
	if a.BranchPassword != "" {
		pw := a.BranchPassword
		if mask {
			pw = spec.Redacted
		}
		line += " -betapassword " + pw
	}
	if a.Validate {
		// Positional, never "-validate".
		line += " validate"
	}
	return line, nil
}

// checkToken refuses anything that could add a line to the script.
//
// The runscript is a line-oriented command file, so a newline inside an
// interpolated value is command injection against a process that downloads and
// executes third-party code. A control character is refused for the same reason
// a shell argument containing one is: nobody can read what the file will do.
//
// The value is never quoted into the message -- one of the tokens this checks
// is a branch password, and an error string is the least controllable place in
// the program.
func checkToken(what, v string) error {
	for _, r := range v {
		if r == '\n' || r == '\r' {
			return fmt.Errorf("%w: the %s value contains a line break", ErrRunscript, what)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: the %s value contains a control character", ErrRunscript, what)
		}
		if r == ' ' || r == '\t' {
			return fmt.Errorf("%w: the %s value contains whitespace, which steamcmd would read "+
				"as the start of another argument", ErrRunscript, what)
		}
	}
	return nil
}

// plausibleBranch is the charset Steam branch names use. It is a whitelist
// because this value is interpolated into a command line.
func plausibleBranch(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return s != ""
}
