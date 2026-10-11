package hook

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/provider/steamcmd"
)

// The three shims a SteamCMD-installed server needs, as three named hooks.
//
// They are separate names rather than one "steam.fixups" because a manifest has
// to be able to take two of them and not the third: generic-steam.yaml wants
// the HOME layout and the sdk links and no appid shim, valheim.yaml wants all
// three, and `gvm game validate` prints which of them resolve for an instance
// (section 9's "3 hooks resolve (steam.home_layout, steam.link_sdk,
// steam.appid_shim)"). One hook doing three things could not be reported that
// way and could not be turned off a third at a time.
//
// All three delegate to internal/provider/steamcmd's shims.go. The derivation
// -- which paths, which targets, which modes, which of sdk32 and sdk64 -- lives
// there because the provider also applies it during Apply, and a second copy
// here would be a second set of rules about which link points where.
//
// The apply half reports a refusal as a PROBLEM, not an error. A real
// steamclient.so an operator copied into ~/.steam/sdk64 by hand works
// perfectly, and replacing a file gvm did not create is not a hook's call to
// make; returning an error for it would abort a post_install phase over
// something that is already correct.

// shimInput builds the provider's ShimInput from a hook context.
//
// Home and Work come from the jailed Paths rather than from the manifest's
// templates: a hook that re-rendered "{{ .Home }}" could disagree with the
// value the provider installed against, and the two are supposed to be the same
// directory by construction.
func shimInput(h *Ctx) (steamcmd.ShimInput, error) {
	if h == nil || h.Manifest == nil {
		return steamcmd.ShimInput{}, fmt.Errorf("%w: no manifest", ErrNoSeam)
	}
	if !h.Paths.Home.Valid() {
		return steamcmd.ShimInput{}, fmt.Errorf("%w: no HOME for this instance; a server with "+
			"no stable HOME cannot be given a steamclient.so to dlopen", ErrNoSeam)
	}
	in := steamcmd.ShimInput{
		Home:        h.Paths.Home.Path(),
		SteamCMDDir: h.steamCMDDir(),
		Arch:        h.Manifest.Spec.Runtime.Arch,
	}
	if h.Paths.Work.Valid() {
		in.Work = h.Paths.Work.Path()
	}
	if id, ok := steamcmd.RuntimeAppID(h.Manifest); ok {
		in.RuntimeAppID = id
	}
	return in, nil
}

// steamCMDDir is where the steamcmd tarball was extracted.
//
// Defaulted rather than refused when unset: the sdk symlink's target is the
// only thing it is used for, and ApplyShims already reports a target that is
// not there as a problem naming the path -- which is a better failure than a
// hook that refuses to run and says nothing about what it was looking for.
func (h *Ctx) steamCMDDir() string {
	if d := strings.TrimSpace(h.SteamCMDDir); d != "" {
		return d
	}
	return steamcmd.DefaultSteamCMDDir
}

// applyShims runs a shim plan and folds the result into the hook's report.
func applyShims(h *Ctx, shims []steamcmd.Shim) error {
	res, err := steamcmd.ApplyShims(shims, h.links())
	if res != nil {
		for _, s := range res.Created {
			h.Note("created %s", s)
		}
		for _, s := range res.Already {
			h.Note("already correct: %s", s)
		}
		for _, p := range res.Problems {
			h.Problem("%s", p)
		}
	}
	return err
}

// steamHomeLayout is steam.home_layout.
type steamHomeLayout struct{}

func (steamHomeLayout) Name() string { return "steam.home_layout" }

// Phases is post_install only, deliberately.
//
// Re-asserting the layout before every start would be defensible and is not
// what the shipped manifests ask for, and a hook that declares a phase no
// manifest uses is a phase nobody tests. A future revision that wants it in
// pre_start changes both the manifest and this list, together.
func (steamHomeLayout) Phases() []Phase { return []Phase{PhasePostInstall} }

func (steamHomeLayout) Doc() string {
	return "Creates ~/.local/share/Steam and points ~/.steam/root and ~/.steam/steam at it, " +
		"for the game's HOME and for SteamCMD's own (D-30). steamcmd writes to both locations " +
		"and uses them inconsistently (ValveSoftware/steam-for-linux#6976), so a server whose " +
		"HOME has only one of them works until the day it does not."
}

func (steamHomeLayout) Run(_ context.Context, h *Ctx) error {
	in, err := shimInput(h)
	if err != nil {
		return err
	}
	layout, err := layoutOnly(in)
	if err != nil {
		return err
	}
	return applyShims(h, layout)
}

// steamLinkSDK is steam.link_sdk.
type steamLinkSDK struct{}

func (steamLinkSDK) Name() string    { return "steam.link_sdk" }
func (steamLinkSDK) Phases() []Phase { return []Phase{PhasePostInstall} }

func (steamLinkSDK) Doc() string {
	return "Links $HOME/.steam/sdk64/steamclient.so (sdk32 for a 32-bit engine) at the " +
		"steamcmd installation's copy. Servers that call SteamAPI_Init dlopen it from there " +
		"and SteamCMD never puts it there; without the link the server prints [S_API FAIL] " +
		"SteamAPI_Init() failed and is invisible in the server browser."
}

func (steamLinkSDK) Run(_ context.Context, h *Ctx) error {
	in, err := shimInput(h)
	if err != nil {
		return err
	}
	sdk, err := sdkOnly(in)
	if err != nil {
		return err
	}
	if len(sdk) == 0 {
		h.Note("no steamclient.so to link: no steamcmd installation is configured")
	}
	return applyShims(h, sdk)
}

// layoutOnly and sdkOnly split HomeShims' output into the two hooks.
//
// By difference rather than by re-deriving either half: SteamCMDDir is the only
// input the sdk links depend on, so the plan without it is exactly the layout,
// and the rest is exactly the links. Filtering on a path substring would be the
// alternative, and it would silently stop matching the day a path changes.
func layoutOnly(in steamcmd.ShimInput) ([]steamcmd.Shim, error) {
	bare := in
	bare.SteamCMDDir = ""
	return steamcmd.HomeShims(bare)
}

func sdkOnly(in steamcmd.ShimInput) ([]steamcmd.Shim, error) {
	layout, err := layoutOnly(in)
	if err != nil {
		return nil, err
	}
	all, err := steamcmd.HomeShims(in)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(layout))
	for _, s := range layout {
		seen[s.String()] = true
	}
	var out []steamcmd.Shim
	for _, s := range all {
		if !seen[s.String()] {
			out = append(out, s)
		}
	}
	return out, nil
}

// steamAppIDShim is steam.appid_shim.
type steamAppIDShim struct{}

func (steamAppIDShim) Name() string    { return "steam.appid_shim" }
func (steamAppIDShim) Phases() []Phase { return []Phase{PhasePostInstall} }

func (steamAppIDShim) Doc() string {
	return "Records the appid the server must ANNOUNCE, which is not always the appid it was " +
		"installed under (Valheim installs as 896660 and announces 892970; Core Keeper " +
		"1963720 and 1621690). The SteamAppId environment pair is the first answer and the " +
		"unit generator emits it; this hook writes the steam_appid.txt fallback next to the " +
		"binary for a server that does not read the environment."
}

// appIDFile is where the fallback goes: beside the launcher named by
// runtime.exec, relative to the install tree.
//
// Beside the binary and not at the tree's root, because that is where the Steam
// API looks -- it reads steam_appid.txt from the process's working directory,
// which for these games is the directory the launcher lives in.
func appIDFile(h *Ctx) string {
	exec := strings.TrimSpace(h.Manifest.Spec.Runtime.Exec)
	if exec == "" || strings.Contains(exec, "{{") {
		// A templated exec is the renderer's business and not this hook's; a
		// guess at the directory would put the file where nothing reads it.
		return ""
	}
	dir := path.Dir(path.Clean(strings.TrimPrefix(exec, "./")))
	if dir == "." || dir == "/" {
		return "steam_appid.txt"
	}
	return path.Join(dir, "steam_appid.txt")
}

func (steamAppIDShim) Run(_ context.Context, h *Ctx) error {
	fsys, err := h.fsOf()
	if err != nil {
		return err
	}
	in, err := shimInput(h)
	if err != nil {
		return err
	}

	// The env pair first, because it is the answer that works for every server
	// that reads its environment, and recording it is what lets a missing
	// SteamAppId be reported as a shim problem rather than discovered as a
	// server that will not boot.
	env := steamcmd.RuntimeEnv(in)
	if id, ok := env["SteamAppId"]; ok {
		h.Note("SteamAppId=%s is the appid this server announces; the unit generator emits it "+
			"from the manifest's runtime.env", id)
	} else {
		h.Note("this game announces the appid it was installed under, so no shim is needed")
		return nil
	}

	in.AppIDFileRel = appIDFile(h)
	rel, content, ok := steamcmd.AppIDShim(in)
	if !ok {
		h.Problem("the SteamAppId fallback file was not written: runtime.exec (%q) does not "+
			"name a directory to put it beside", h.Manifest.Spec.Runtime.Exec)
		return nil
	}

	// Through the provider's STRICT install jail as well as through Ctx.Fs. The
	// strict jail refuses a destination that reaches its target through a
	// symlink even when it stays inside the tree, which is zip-slip's second
	// act, and the install tree is the one place third-party archives have
	// written. A refusal is a problem and not an error: the env pair above is
	// already the primary answer.
	if _, err := h.Paths.Dest(rel); err != nil {
		h.Problem("%s was not written: %v", rel, err)
		return nil
	}

	if cur, err := fsys.ReadFile(RootInstall, rel); err == nil && string(cur) == string(content) {
		h.Note("already correct: %s contains %s", rel, strings.TrimSpace(string(content)))
		return nil
	}
	// 0644: the Steam API reads this from inside the game process, and there is
	// nothing secret about an appid that is printed in every server listing.
	if err := fsys.WriteFile(RootInstall, rel, content, fs.FileMode(0o644)); err != nil {
		return err
	}
	h.Note("wrote %s = %s", rel, strings.TrimSpace(string(content)))
	return nil
}

// SteamShimReport is what `gvm doctor` prints for the steam hooks.
//
// It exists so the dashboard and doctor can render the shims as FACTS about an
// instance -- the section 9.4 line "steam.home_layout ✓ steam.link_sdk ✓
// steam.appid_shim (SteamAppId=1621690) ✓" -- without running the hooks, which
// write.
func SteamShimReport(h *Ctx) ([]string, error) {
	in, err := shimInput(h)
	if err != nil {
		return nil, err
	}
	shims, err := steamcmd.HomeShims(in)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(shims)+1)
	for _, s := range shims {
		out = append(out, s.String())
	}
	if in.RuntimeAppID > 0 {
		out = append(out, fmt.Sprintf("SteamAppId=%d", in.RuntimeAppID))
	}
	sort.Strings(out)
	return out, nil
}
