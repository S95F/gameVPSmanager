package hook

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/gamecfg"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// ErrConfig is a config file a hook may not open or may not change.
var ErrConfig = errors.New("hook: config file refused")

// ConfigAccess is what NewConfigAccess needs.
type ConfigAccess struct {
	Manifest *spec.Manifest
	Render   *spec.Renderer
	Fs       JailedFS
	// Active reports whether the instance's unit is running right now.
	//
	// A function and not a bool, because it is asked twice -- once when the
	// document is opened and again before the write lands -- and the answer can
	// change in between: a post_start hook opens a file with the server
	// starting and commits with it up.
	//
	// nil means "nothing can say", which is treated as ACTIVE for an
	// owner:server file. Assuming stopped would make the one dangerous case the
	// default.
	Active func() (bool, error)
}

// NewConfigAccess builds Ctx.Config from the manifest's config_files list.
//
// Three rules live here rather than in each hook, because a rule repeated per
// hook is a rule one hook gets wrong:
//
//  1. A file is addressed by NAME. A hook cannot name a path, so it cannot
//     touch a file the manifest does not declare -- and the manifest is the
//     thing that is pinned per instance (D-17).
//  2. deny_keys are refused even to hooks. PZ's SteamVAC and ServerPlayerID
//     are settings gvm writing would break a server in ways the operator
//     cannot see from the dashboard; the declaration says so, and this is
//     where the declaration becomes true.
//  3. An owner:server file is refused while the unit is active. The game
//     rewrites those on shutdown, so a hook's edit under a live server is a
//     write that silently vanishes -- which looks exactly like gvm working.
func NewConfigAccess(in ConfigAccess) (func(string) (gamecfg.Doc, Commit, error), error) {
	if in.Manifest == nil {
		return nil, fmt.Errorf("%w: no manifest, so no config file is declared", ErrNoSeam)
	}
	if in.Render == nil {
		return nil, fmt.Errorf("%w: no renderer; a config file's path is a template", ErrNoSeam)
	}
	if in.Fs == nil {
		return nil, fmt.Errorf("%w: no filesystem", ErrNoSeam)
	}
	return func(name string) (gamecfg.Doc, Commit, error) { return openConfig(in, name) }, nil
}

func openConfig(in ConfigAccess, name string) (gamecfg.Doc, Commit, error) {
	cf, ok := findConfigFile(in.Manifest, name)
	if !ok {
		return nil, nil, fmt.Errorf("%w: this game declares no config file called %q; it has "+
			"%s", ErrConfig, name, orNone(configFileNames(in.Manifest)))
	}

	// Rule 3, at open. Refusing here rather than at commit means a hook never
	// gets a document it is not allowed to write back, which is the difference
	// between a clear refusal and twenty lines of work thrown away.
	if cf.Owner == "server" {
		active, err := isActive(in.Active)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %q is owned by the server and gvm cannot tell "+
				"whether the unit is running: %v", ErrConfig, name, err)
		}
		if active {
			return nil, nil, fmt.Errorf("%w: %q is rewritten by the game on shutdown and the "+
				"server is running, so an edit now would be silently discarded. Stop the "+
				"instance first", ErrConfig, name)
		}
	}

	codec, ok := gamecfg.Lookup(cf.Format)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q declares format %q, which this build has no codec "+
			"for", ErrConfig, name, cf.Format)
	}

	root, rel, err := renderConfigPath(in, cf)
	if err != nil {
		return nil, nil, err
	}
	perm, err := parseMode(cf.Mode)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %q: %v", ErrConfig, name, err)
	}

	raw, readErr := in.Fs.ReadFile(root, rel)
	var doc gamecfg.Doc
	switch {
	case readErr == nil:
		if doc, err = codec.Parse(raw); err != nil {
			return nil, nil, fmt.Errorf("%w: parsing %q (%s): %v", ErrConfig, name, rel, err)
		}
	case errors.Is(readErr, fs.ErrNotExist) && cf.CreateIfMissing:
		if doc, err = codec.New(); err != nil {
			// A format whose file only the game can produce reports
			// ErrCannotCreate, which makes create_if_missing:true on such a
			// file a manifest error rather than a half-written config the
			// server refuses to load.
			return nil, nil, fmt.Errorf("%w: %q: %v", ErrConfig, name, err)
		}
	case errors.Is(readErr, fs.ErrNotExist):
		return nil, nil, fmt.Errorf("%w: %q is not there yet (%s) and create_if_missing is "+
			"false, so the game has to generate it first", ErrConfig, name, rel)
	default:
		return nil, nil, fmt.Errorf("%w: reading %q: %v", ErrConfig, name, readErr)
	}

	guarded := &denyDoc{Doc: doc, name: name, deny: denySet(cf.DenyKeys)}
	commit := func() error {
		if cf.Owner == "server" {
			// Asked again: a pre_start hook opens this before exec and a
			// post_start hook opens it with the server already up, so the
			// answer can differ from the one at open.
			active, err := isActive(in.Active)
			if err != nil {
				return fmt.Errorf("%w: %q: %v", ErrConfig, name, err)
			}
			if active {
				return fmt.Errorf("%w: %q: the server started while the hook was working; the "+
					"write would be overwritten when it shuts down", ErrConfig, name)
			}
		}
		return in.Fs.WriteFile(root, rel, guarded.Bytes(), perm)
	}
	return guarded, commit, nil
}

// ConfigTarget resolves a declared config file to the pair that addresses it
// plus the mode the manifest wants it at.
//
// Separate from Ctx.Config because the two callers want different things. A
// hook that EDITS a file wants the parsed document, and the accessor refuses a
// file that is not there yet. A hook that SEEDS one is running precisely
// because it is not there, and needs the path the manifest declares rather than
// a path of its own -- so that moving a config file in a manifest revision does
// not leave a hook writing the shipped default to the old location.
func ConfigTarget(h *Ctx, name string) (Root, string, fs.FileMode, error) {
	if h == nil || h.Manifest == nil || h.Render == nil || h.Fs == nil {
		return "", "", 0, fmt.Errorf("%w: resolving config file %q needs a manifest, a "+
			"renderer and a filesystem", ErrNoSeam, name)
	}
	cf, ok := findConfigFile(h.Manifest, name)
	if !ok {
		return "", "", 0, fmt.Errorf("%w: this game declares no config file called %q; it has "+
			"%s", ErrConfig, name, orNone(configFileNames(h.Manifest)))
	}
	root, rel, err := renderConfigPath(ConfigAccess{
		Manifest: h.Manifest, Render: h.Render, Fs: h.Fs,
	}, cf)
	if err != nil {
		return "", "", 0, err
	}
	perm, err := parseMode(cf.Mode)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: %q: %v", ErrConfig, name, err)
	}
	return root, rel, perm, nil
}

// renderConfigPath expands the file's path template and maps it back to a
// (root, rel) pair.
func renderConfigPath(in ConfigAccess, cf spec.ConfigFile) (Root, string, error) {
	field := "spec.config_files." + cf.Name + ".path"
	abs, taint, err := in.Render.Render(field, cf.Path)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if taint.Has(spec.TaintSecret) {
		// The same refusal spec.RenderFile makes, repeated because this is a
		// second route to the same act: a masked path is not enough when the
		// password would still be on disk as a filename.
		return "", "", fmt.Errorf("%w: %s renders a secret into a file path", ErrConfig, field)
	}
	root, rel, err := in.Fs.Locate(abs)
	if err != nil {
		return "", "", fmt.Errorf("%w: %q resolves to %s: %w", ErrConfig, cf.Name, abs, err)
	}
	return root, rel, nil
}

func isActive(f func() (bool, error)) (bool, error) {
	if f == nil {
		// Nothing can say. Treated as running, because the alternative is
		// making the dangerous answer the default for a caller that forgot to
		// wire the check.
		return true, errors.New("nothing was wired to report whether the unit is active")
	}
	return f()
}

func findConfigFile(m *spec.Manifest, name string) (spec.ConfigFile, bool) {
	for _, cf := range m.Spec.Configs {
		if cf.Name == name {
			return cf, true
		}
	}
	return spec.ConfigFile{}, false
}

func configFileNames(m *spec.Manifest) []string {
	out := make([]string, 0, len(m.Spec.Configs))
	for _, cf := range m.Spec.Configs {
		out = append(out, cf.Name)
	}
	sort.Strings(out)
	return out
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

// parseMode reads a manifest's "0640".
//
// Base 8 explicitly, and an empty value defaults rather than becoming 0:
// strconv with base 0 would read "640" as decimal 640, which is mode 01200 --
// setgid, setuid and nothing else. A config file nobody can read is a server
// that will not boot, and the cause would be a missing leading zero.
func parseMode(s string) (fs.FileMode, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0o640, nil
	}
	v, err := strconv.ParseUint(t, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal", s)
	}
	// The owner has to be able to read it at least. A config file the account
	// the server runs as cannot open is a server that will not boot, and the
	// cause -- one wrong digit in a manifest -- is nowhere near the symptom.
	if v&0o400 == 0 {
		return 0, fmt.Errorf("mode %q does not let the owner read the file", s)
	}
	return fs.FileMode(v).Perm(), nil
}

func denySet(keys []string) map[string]bool {
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[strings.ToLower(k)] = true
	}
	return out
}

// denyDoc is a gamecfg.Doc with the manifest's deny_keys made unwritable.
//
// It wraps rather than filters on commit, so the refusal reaches the hook at
// the Set call that caused it -- naming the key -- instead of arriving as a
// whole document that will not save. Reads are not blocked: a hook may well
// need to know what the server put in SteamVAC, and the declaration is about
// what gvm WRITES.
//
// Case-insensitive, because the formats behind it are: PZ's ini and the Unreal
// option settings both match keys without regard to case, so a deny list that
// did not would be bypassed by changing one letter.
type denyDoc struct {
	gamecfg.Doc
	name string
	deny map[string]bool
}

func (d *denyDoc) refuse(key, verb string) error {
	return fmt.Errorf("%w: %s is in %q's deny_keys, so gvm will not %s it -- not from a hook "+
		"either (section 8.8)", ErrConfig, key, d.name, verb)
}

func (d *denyDoc) denied(key string) bool {
	return d.deny != nil && d.deny[strings.ToLower(key)]
}

func (d *denyDoc) Set(key, value string) error {
	if d.denied(key) {
		return d.refuse(key, "write")
	}
	return d.Doc.Set(key, value)
}

// Delete reports 0 for a denied key rather than refusing, because Doc.Delete
// returns a count and not an error.
//
// Zero is the truthful answer: nothing was removed. A hook that wanted to
// remove a denied key and saw 0 has the same information it would get from a
// key that was not there, which is the right shape -- the manifest has declared
// that gvm does not own this key, so "gvm removed none of it" is correct.
func (d *denyDoc) Delete(key string) int {
	if d.denied(key) {
		return 0
	}
	return d.Doc.Delete(key)
}
