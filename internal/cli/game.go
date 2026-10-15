// `gvm game` — the manifest-facing half of the CLI.
//
// Adding a game is meant to cost zero lines of Go, which only holds if an
// author can check their own file before it touches a real box. These four
// subcommands are that loop: validate says what is wrong and where, which says
// which of the three shadowing layers actually wins, list says what this binary
// can run at all, and hooks says what a manifest is allowed to name.
//
// Everything here reads. Publishing an override is owner-only and gated on a
// recorded sha256 approval, and lives with the privileged verbs.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/gamecfg"
	"github.com/S95F/gameVPSmanager/internal/hook"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// gameSubs is the subcommand table, in the order `gvm game` lists them.
var gameSubs = []struct {
	name    string
	args    string
	summary string
	run     func(*Env, []string) error
}{
	{"validate", "<file.yaml|game-id>...", "check a manifest against the schema, the closed template grammar and the registries", runGameValidate},
	{"list", "", "list every game this box can run, and where each one comes from", runGameList},
	{"which", "<game-id>", "show which layer a game id resolves to, and the layers it shadows", runGameWhich},
	{"hooks", "", "list the hook and validator registries a manifest may name", runGameHooks},
}

func runGame(_ context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		gameUsage(env.Stderr)
		return usageErr(errors.New("name a subcommand"))
	}
	switch args[0] {
	case "-h", "--help", "help":
		gameUsage(env.Stdout)
		return nil
	}
	for _, s := range gameSubs {
		if s.name == args[0] {
			return s.run(env, args[1:])
		}
	}
	gameUsage(env.Stderr)
	return usageErr(fmt.Errorf("unknown subcommand %q", args[0]))
}

func gameUsage(w io.Writer) {
	fmt.Fprint(w, "gvm game — validate and inspect game manifests\n\nUsage:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range gameSubs {
		fmt.Fprintf(tw, "  gvm game %s %s\t%s\n", s.name, s.args, s.summary)
	}
	_ = tw.Flush()
}

// gameCatalog stacks the three layers of section 8.7. The embedded floor is
// always present, so every subcommand here works on a box whose /usr/share was
// emptied — which is the box an operator most needs answers about.
func gameCatalog() *spec.Catalog { return spec.SystemCatalog(games.FS) }

// overrideAuth turns the flag into an authorizer. Denying is the default
// because a manifest is argv plus env plus destruction globs: an unapproved
// override in /etc/gvm/games is skipped with a warning, never silently used
// .
func overrideAuth(unapproved bool) spec.OverrideAuthorizer {
	if unapproved {
		return spec.RootCLIAuthorizer{}
	}
	return spec.DenyOverrides{}
}

func gameFlags(env *Env, name, args string) (*flag.FlagSet, *bool) {
	fs := flag.NewFlagSet("gvm game "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: gvm game %s %s\n", name, args)
		fs.PrintDefaults()
	}
	unapproved := fs.Bool("unapproved-override", false,
		"use an /etc/gvm/games override that has no recorded sha256 approval")
	return fs, unapproved
}

func runGameValidate(env *Env, args []string) error {
	fs, unapproved := gameFlags(env, "validate", "<file.yaml|game-id>...")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	targets := fs.Args()
	if len(targets) == 0 {
		fs.Usage()
		return errors.New("name a manifest file or a game id")
	}

	bad := 0
	for i, t := range targets {
		if i > 0 {
			fmt.Fprintln(env.Stdout)
		}
		ok, err := validateOne(env, t, *unapproved)
		if err != nil {
			return err
		}
		if !ok {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d manifests have errors", bad, len(targets))
	}
	return nil
}

func validateOne(env *Env, target string, unapproved bool) (bool, error) {
	ref, raw, err := readManifestTarget(target, unapproved)
	if err != nil {
		return false, err
	}

	w := env.Stdout
	fmt.Fprintf(w, "%s\n\n", ref)

	m, diags, err := spec.ValidateBytes(ref, raw)
	if err == nil {
		// The index is rebuilt rather than threaded out of ValidateBytes because
		// it is wanted for one cosmetic number, and a decode that a report needs
		// is not worth widening the validation API for.
		ix, ixErr := spec.IndexBytes(ref, raw)
		if ixErr == nil {
			// Merged before the report is rendered, so a bad key turns the
			// config-files line to ✗ and reaches the count and the exit code
			// like any other check-6 finding.
			diags = append(diags, codecKeyDiags(m, ix, diags)...)
			for _, line := range checkLines(m, ix, diags) {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
	}
	for _, d := range diags {
		fmt.Fprint(w, diagLines(d))
	}
	fmt.Fprintf(w, "\n  %s\n", countLine(diags))

	return err == nil && !diags.HasErrors(), nil
}

// codecKeyDiags is the per-codec half of section 8.6 check 6: every key must
// pass its format's own KeySyntax.
//
// internal/spec cannot run it — it must never import gamecfg (see the comment
// on configFormats in validate.go) — so it runs here, where both packages are
// already reachable and where a key the format could never address is still a
// manifest line the author can see, rather than an ErrKeyNotFound on the next
// apply or a file that applies nothing and reports success.
// It obeys the same suppression rule as the checks it extends: a path spec has
// already condemned is left alone, so a key with an '=' in it — refused by both
// halves of check 6 — produces one message and not a paragraph.
func codecKeyDiags(m *spec.Manifest, ix *spec.NodeIndex, prior spec.Diags) spec.Diags {
	condemned := make(map[string]bool, len(prior))
	for _, d := range prior.Errors() {
		condemned[d.Path] = true
	}

	var out spec.Diags
	add := func(path string, err error) {
		if err == nil || condemned[path] {
			return
		}
		out = append(out, ix.Errorf(path, "%s", err.Error()))
	}

	for i, cf := range m.Spec.Configs {
		c, ok := gamecfg.Lookup(cf.Format)
		if !ok {
			// Check 2 already condemned the unknown format; a second message
			// about the same line is noise.
			continue
		}
		base := fmt.Sprintf("spec.config_files[%d]", i)

		keys := make([]string, 0, len(cf.Keys))
		for k := range cf.Keys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(base+".keys."+k, c.KeySyntax(k))
		}
		for j, k := range cf.DenyKeys {
			// A deny key the format cannot address denies nothing: the same typo
			// class, with a silence that reads as protection.
			add(fmt.Sprintf("%s.deny_keys[%d]", base, j), c.KeySyntax(k))
		}
	}

	byName := make(map[string]spec.ConfigFile, len(m.Spec.Configs))
	for _, cf := range m.Spec.Configs {
		byName[cf.Name] = cf
	}
	for i, p := range m.Spec.Ports {
		if p.ConfigKey == "" {
			continue
		}
		cf, ok := byName[p.ConfigFile]
		if !ok {
			if p.ConfigFile != "" || len(m.Spec.Configs) == 0 {
				// Check 6 in spec already reported the dangling or homeless
				// config_key; there is no file whose codec could judge the key.
				continue
			}
			cf = m.Spec.Configs[0]
		}
		c, ok := gamecfg.Lookup(cf.Format)
		if !ok {
			continue
		}
		add(fmt.Sprintf("spec.ports[%d].config_key", i), c.KeySyntax(p.ConfigKey))
	}
	return out
}

// readManifestTarget resolves a validate argument to bytes.
//
// A PATH is read straight off disk, because the file an author most needs to
// check is the one that is not installed anywhere yet. Anything else is a game
// id resolved through the catalog, which is how you check what a box would
// actually load rather than what is in your editor.
func readManifestTarget(target string, unapproved bool) (string, []byte, error) {
	if looksLikePath(target) {
		raw, err := os.ReadFile(target)
		if err != nil {
			return "", nil, err
		}
		return target, raw, nil
	}

	cands, _, err := gameCatalog().Resolve(target, overrideAuth(unapproved))
	if err != nil {
		// A file that exists but does not parse is reported by Resolve as an
		// error, and its bytes are still the thing to validate: refusing to say
		// anything about a broken manifest is the opposite of this command's job.
		for _, c := range cands {
			if c.Err != nil {
				return c.Ref, c.Raw, nil
			}
		}
		return "", nil, err
	}
	for _, c := range cands {
		if c.Winner {
			return c.Ref, c.Raw, nil
		}
	}
	return "", nil, fmt.Errorf("no usable manifest for %q", target)
}

func looksLikePath(s string) bool {
	return strings.ContainsAny(s, `/\`) ||
		strings.HasSuffix(s, ".yaml") || strings.HasSuffix(s, ".yml")
}

// checkLines is the report of section 9.5: one line per thing that was checked,
// stated as the fact it established rather than as the name of a check. A line
// is only a tick when nothing in its part of the document produced an error, so
// the ticks are claims about this file and not decoration.
func checkLines(m *spec.Manifest, ix *spec.NodeIndex, diags spec.Diags) []string {
	failed := func(prefixes ...string) bool {
		for _, d := range diags.Errors() {
			for _, p := range prefixes {
				if d.Path == p || strings.HasPrefix(d.Path, p+".") || strings.HasPrefix(d.Path, p+"[") {
					return true
				}
			}
		}
		return false
	}

	tmpl := spec.TemplateFields(ix)
	templated := make(map[string]bool, len(tmpl))
	for _, p := range tmpl {
		templated[p] = true
	}
	badTemplate := false
	for _, d := range diags.Errors() {
		if templated[d.Path] {
			badTemplate = true
		}
	}

	s := m.Spec
	out := []string{
		mark(!failed("apiVersion", "kind", "metadata", "spec.driverABI"),
			fmt.Sprintf("apiVersion %s, driverABI %d (this build implements %d)",
				m.APIVersion, s.DriverABI, spec.BuiltDriverABI)),
		mark(!failed("spec.provider"), providerLine(s.Provider)),
		mark(!badTemplate, fmt.Sprintf(
			"%s parse, 0 control-flow nodes, 0 undeclared references", plural(len(tmpl), "template"))),
		mark(!failed("spec.ports", "spec.port_stride"), fmt.Sprintf(
			"%s, %s, no expose/bind contradiction", plural(len(s.Ports), "port"), groupSummary(s))),
	}
	files := fmt.Sprintf("%s, %s",
		plural(len(s.Configs), "config file"), plural(len(s.Content), "content source"))
	if len(s.Configs) == 0 && len(s.Content) == 0 && s.Control == nil {
		files += " — console-only game"
	}
	out = append(out, mark(!failed("spec.config_files", "spec.content"), files))

	if hooks := allHooks(s.Hooks); len(hooks) > 0 {
		if failed("spec.hooks") {
			out = append(out, mark(false, fmt.Sprintf("%s %s (%s)",
				plural(len(hooks), "hook"), resolves(len(hooks)), strings.Join(hooks, ", "))))
		} else {
			// Resolve against the closed registry, but this build does not RUN
			// hooks (that lands with config_files), so the line is a note, not a
			// tick an operator would read as "these will fire".
			out = append(out, fmt.Sprintf("! %s %s but this build does not run them (%s)",
				plural(len(hooks), "hook"), resolves(len(hooks)), strings.Join(hooks, ", ")))
		}
	}
	if n := len(s.Validators); n > 0 {
		out = append(out, mark(!failed("spec.validators"), fmt.Sprintf("%s %s (%s)",
			plural(n, "validator"), resolves(n), strings.Join(s.Validators, ", "))))
	}
	if c := s.Control; c != nil {
		out = append(out, mark(!failed("spec.control"), fmt.Sprintf(
			"control %s on port %s, dialect %s", c.Kind, c.Port, c.Dialect)))
	}

	r := s.Readiness
	out = append(out, mark(!failed("spec.readiness"), fmt.Sprintf(
		"%s, hard_deadline %s, stall_timeout %s", plural(len(r.Probes), "readiness probe"),
		r.HardDeadline, r.StallTimeout)))

	var ladder spec.Duration
	for _, st := range s.Shutdown.Steps {
		ladder += st.Wait
	}
	out = append(out, mark(!failed("spec.shutdown"), fmt.Sprintf(
		"shutdown ladder of %s waiting %s, inside the %s budget",
		plural(len(s.Shutdown.Steps), "step"), ladder, s.Shutdown.Budget)))

	return append(out, mark(!failed("spec.sizing", "spec.vars"), sizingLine(s)))
}

// sizingLine reports the cgroup limit the manifest's OWN defaults produce. An
// author reading "MemoryMax 8G" against a box they know the size of catches a
// wrong headroom immediately; reading back the three fields they just typed
// catches nothing.
func sizingLine(s spec.GameSpec) string {
	line := fmt.Sprintf("%s, memory_model %s", plural(len(s.Vars), "var"), s.Sizing.Model)
	vars := spec.VarValues{}
	for name, v := range s.Vars {
		vars[name] = v.Default
	}
	if lim, err := s.Sizing.Memory(vars); err == nil {
		line += fmt.Sprintf(" — MemoryHigh %s, MemoryMax %s at the declared defaults", lim.High, lim.Max)
	}
	return line
}

func providerLine(p spec.ProviderSpec) string {
	if p.Kind == spec.ProviderSteamCMD && p.SteamCMD != nil {
		return fmt.Sprintf("provider steamcmd app %d, sentinel + verify present, `validate` not on update",
			p.SteamCMD.InstallAppID)
	}
	return "provider " + p.Kind
}

// groupSummary names each port group and its stride, because a group that
// shifts by the wrong amount is the failure that produces a server nobody can
// join, and the number is invisible at the port that suffers it.
func groupSummary(s spec.GameSpec) string {
	seen := map[string]bool{}
	var parts []string
	for _, p := range s.Ports {
		if p.Group == "" || seen[p.Group] {
			continue
		}
		seen[p.Group] = true
		parts = append(parts, fmt.Sprintf("group %q stride %d", p.Group, s.Stride(p.Group)))
	}
	if len(parts) == 0 {
		return "no groups"
	}
	return strings.Join(parts, ", ")
}

func allHooks(h spec.HookSpec) []string {
	var out []string
	for _, list := range [][]string{
		h.Validate, h.PostInstall, h.PostContent, h.PreStart, h.PostStart, h.PreBackup, h.PostBackup,
	} {
		out = append(out, list...)
	}
	return out
}

func mark(ok bool, text string) string {
	if ok {
		return "✓ " + text
	}
	return "✗ " + text
}

// diagLines renders one finding. The position keeps the compiler's
// file:line:col shape so an editor's error parser can jump to it; the marker in
// front is what stops a warning from being read as an error in a list of them.
func diagLines(d spec.Diag) string {
	marker := "✗"
	if d.Sev == spec.SevWarning {
		marker = "!"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %s %s", marker, d.File)
	if d.Line > 0 {
		fmt.Fprintf(&b, ":%d", d.Line)
		if d.Col > 0 {
			fmt.Fprintf(&b, ":%d", d.Col)
		}
	}
	b.WriteString(": ")
	if d.Path != "" {
		b.WriteString(d.Path + ": ")
	}
	b.WriteString(d.Msg + "\n")
	if d.Fix != "" {
		b.WriteString("      " + d.Fix + "\n")
	}
	return b.String()
}

func countLine(diags spec.Diags) string {
	errs, warns := 0, 0
	for _, d := range diags {
		if d.Sev == spec.SevWarning {
			warns++
		} else {
			errs++
		}
	}
	return fmt.Sprintf("%s, %s.", plural(errs, "error"), plural(warns, "warning"))
}

func resolves(n int) string {
	if n == 1 {
		return "resolves"
	}
	return "resolve"
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func runGameList(env *Env, args []string) error {
	fs, unapproved := gameFlags(env, "list", "")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cat := gameCatalog()
	ids, listErr := cat.List()
	auth := overrideAuth(*unapproved)

	fmt.Fprintln(env.Stdout)
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	broken := 0
	for _, id := range ids {
		cands, _, err := cat.Resolve(id, auth)
		if err != nil {
			// One unreadable manifest must not take the listing down with it:
			// the whole point of the list is to work when something is wrong.
			broken++
			fmt.Fprintf(tw, "  %s\t\t\tunreadable: %v\n", id, err)
			continue
		}
		for _, c := range cands {
			if !c.Winner {
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\trevision %d\t%s%s\n",
				id, c.Manifest.Metadata.Name, c.Revision, c.Ref, shadowNote(cands))
		}
	}
	_ = tw.Flush()
	fmt.Fprintf(env.Stdout, "\n  %s\n", plural(len(ids), "game"))

	if listErr != nil {
		return listErr
	}
	if broken > 0 {
		return fmt.Errorf("%s could not be read", plural(broken, "manifest"))
	}
	return nil
}

func shadowNote(cands []spec.Candidate) string {
	if n := len(cands) - 1; n > 0 {
		return fmt.Sprintf("  (+%s)", plural(n, "other layer"))
	}
	return ""
}

func runGameWhich(env *Env, args []string) error {
	fs, unapproved := gameFlags(env, "which", "<game-id>")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("name exactly one game id")
	}
	id := fs.Arg(0)

	cands, diags, err := gameCatalog().Resolve(id, overrideAuth(*unapproved))
	w := env.Stdout
	fmt.Fprintln(w)
	for _, c := range cands {
		switch {
		case c.Winner:
			fmt.Fprintf(w, "  WINNER   %s\n", c.Ref)
			fmt.Fprintf(w, "           revision %d   driverABI %d   sha256 %s\n",
				c.Revision, c.Manifest.Spec.DriverABI, short(c.SHA256))
		case c.Err != nil:
			fmt.Fprintf(w, "  BROKEN   %s\n           %v\n", c.Ref, c.Err)
		case c.Skipped != "":
			fmt.Fprintf(w, "  skipped  %s   %s   sha256 %s\n", c.Ref, c.Skipped, short(c.SHA256))
		default:
			fmt.Fprintf(w, "  shadowed %s   revision %d   sha256 %s\n", c.Ref, c.Revision, short(c.SHA256))
		}
	}
	for _, d := range diags {
		fmt.Fprint(w, diagLines(d))
	}
	fmt.Fprintln(w)
	return err
}

func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12] + "…"
}

func runGameHooks(env *Env, args []string) error {
	fs, _ := gameFlags(env, "hooks", "")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	w := env.Stdout
	fmt.Fprint(w, "\n  Hooks — the closed registry spec.hooks may name\n")
	fmt.Fprint(w, "  (registered and documented; this build does not RUN hooks — that lands with\n"+
		"  config_files)\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, h := range hook.All() {
		fmt.Fprintf(tw, "    %s\t%s\tregistered — not run by this build\n", h.Name(), h.Doc())
	}
	_ = tw.Flush()

	fmt.Fprint(w, "\n  Validators — spec.validators, run at apply time against real values\n")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, n := range spec.ValidatorNames() {
		doc, _ := spec.ValidatorDoc(n)
		fmt.Fprintf(tw, "    %s\t%s\n", n, doc)
	}
	_ = tw.Flush()

	fmt.Fprint(w, "\n  A name outside these lists is a load-time error, which is what keeps\n"+
		"  `hooks: [something.clever]` from being a plugin system.\n")
	return nil
}

// flagErr turns the error -h produces into errHelpShown, so the command stops
// (the FlagSet already printed usage) and Main exits 0 rather than continuing
// into the command body with a zero-valued options struct (U7).
func flagErr(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return errHelpShown
	}
	return err
}
