package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/gamecfg"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The three Project Zomboid hooks, and the reason D-21 draws the line where it
// does.
//
// Mod resolution is a scoring problem over files on disk: many workshop items
// ship a Build 41 and a Build 42 variant side by side, each with its own
// mod.info and its own internal id, and enabling the B41 id on a B42 server
// SILENTLY OMITS THE MOD while the server boots normally. A YAML predicate like
// `field('versionMin') >= 42` has unspecified numeric-versus-lexicographic
// semantics against a real versionMin=42.0.1, so a wrong answer would be
// invisible until a player noticed a mod missing -- which is a regression from
// sync-mods.sh's visible `check` warning into silent data loss.
//
// So it is Go, and the Go already exists. The division of labour between the
// three packages involved is the whole of section 12 as it reaches a hook:
//
//   - internal/pz owns the SCORING -- which variant inside a dual-build item is
//     this server's -- and the section 12.5 tag table. It is tested against the
//     live 33-item mods.txt, byte-identical round trip included.
//   - internal/content owns the TWO-LIST MODEL: AcquireID and ActivationID as
//     incompatible Go types, the fail-closed gate, the compiled schema that
//     proves the two write keys are distinct, and the verbatim-activation
//     switch. pz.resolve_mod_ids is a name in ITS closed resolver registry.
//   - these hooks resolve paths and write config keys, and re-implement no part
//     of either. The acquisition and activation lines a hook writes come out of
//     content.Activate, which is the one implementation of that contract: a
//     second one here would be a second place verbatim activation could be
//     forgotten, and the cost of forgetting it is an adopted server's
//     hand-pruned Mods= replaced without being asked.
//
// pz.apply_sandbox is the exception that proves the boundary is real: it edits
// the sandbox document and touches neither namespace, so nothing in it goes
// through internal/content at all.

// pzFirstBoot is pz.first_boot.
//
// Project Zomboid generates <servername>.ini and <servername>_SandboxVars.lua
// itself, on its first run, from its own schema. gvm cannot edit a file the
// server has not written yet, and it must not invent one: the generated file is
// correct for THIS build of the game, and a hand-made one is correct for
// whichever build the person who wrote it was looking at.
//
// Contract rule 3 says a hook must not start the game, and this hook needs a
// boot to have happened. Those are reconciled by having it ASK: it records a
// BootRequest, and the pipeline -- which owns the runner, the readiness
// deadline and the shutdown ladder -- decides whether and how to honour it. The
// hook has no handle that could start anything, which is what makes rule 3
// structural rather than a promise.
type pzFirstBoot struct{}

func (pzFirstBoot) Name() string    { return "pz.first_boot" }
func (pzFirstBoot) Phases() []Phase { return []Phase{PhasePostInstall} }

func (pzFirstBoot) Doc() string {
	return "Asks the pipeline for one boot-and-quit cycle when the server's own " +
		"<servername>.ini does not exist yet. Project Zomboid generates it from its own " +
		"schema on first run; gvm never invents it, because a generated file is correct for " +
		"this build of the game and a hand-made one is correct for whichever build its author " +
		"was looking at. Does nothing once the file is there."
}

func (pzFirstBoot) Run(_ context.Context, h *Ctx) error {
	fsys, err := h.fsOf()
	if err != nil {
		return err
	}
	root, rel, mode, err := ConfigTarget(h, "server_ini")
	if err != nil {
		return err
	}

	if _, err := fsys.Stat(root, rel); err == nil {
		h.Note("%s/%s already exists, so no first boot is needed", root, rel)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("hook: checking %s/%s: %w", root, rel, err)
	}

	// The directory, but not the file. PZ writes the .ini itself and gvm
	// creating an empty one would be worse than leaving it absent: the server
	// reads an empty config as a config, boots with its compiled-in defaults
	// and writes nothing back.
	if dir := path.Dir(rel); dir != "." {
		if err := fsys.MkdirAll(root, dir, dirModeFor(mode)); err != nil {
			return err
		}
		h.Note("created %s/%s for the server to write its config into", root, dir)
	}

	marker := logMarker(h.Manifest)
	stop := stopCommand(h.Manifest)
	if marker == "" || stop == "" {
		// Both come from the manifest rather than from here, so a game whose
		// marker or stop verb changed is a manifest revision. Without them the
		// pipeline would have to guess when the boot had gone far enough, and
		// guessing wrong means quitting a server mid-world-generation.
		h.Problem("%s/%s has to be generated by a first boot, but this manifest declares no "+
			"%s, so gvm cannot tell when the boot has gone far enough", root, rel,
			missingBootFact(marker, stop))
		return nil
	}

	h.RequestBoot(BootRequest{
		Why: fmt.Sprintf("Project Zomboid generates %s on its first run. This boot creates it "+
			"and then quits; the console is live throughout.%s", rel, acquisitionBootFact(h)),
		WaitFor: marker,
		Then:    stop,
	})
	h.Note("requested a first boot: wait for %q, then send %q", marker, stop)
	return nil
}

// acquisitionBootFact is the second half of the sentence first_boot's
// BootRequest carries, when the game acquires its own content.
//
// The number comes from the content block's declared boot_timeout rather than
// from a sentence in this file, because it is the SAME budget section 12.6 step
// 5 spends: on Project Zomboid the first boot is also the first acquisition
// boot, the server downloads what the acquisition key lists, and "40-60
// minutes" hardcoded here would be a second, drifting copy of a manifest field
// that an operator can change.
func acquisitionBootFact(h *Ctx) string {
	block, ok := workshopBlock(h.Manifest)
	if !ok {
		return ""
	}
	// Compiled rather than read field by field, so the two write keys are
	// proved distinct and both separators present before a number out of this
	// block is quoted at an operator. A block that cannot be compiled is
	// reported here and refused for real by pz.resolve_mod_ids, which is the
	// hook that writes those keys.
	sc, err := content.Compile(block, spec.ContentList{}, nil)
	if err != nil {
		h.Problem("this game's content block %q does not compile, so the acquisition boot's "+
			"budget cannot be stated and `gvm content sync` will refuse: %v", block.Name, err)
		return ""
	}
	if !sc.ServerBoot() {
		return ""
	}
	return fmt.Sprintf(" It is also this instance's first ACQUISITION boot: the server "+
		"downloads whatever %s lists, which with a large workshop list is most of the "+
		"%s budget the manifest allows.", sc.Acquisition.Key, sc.Acquisition.BootTimeout)
}

// logMarker is the manifest's log_marker readiness pattern.
func logMarker(m *spec.Manifest) string {
	if m == nil {
		return ""
	}
	for _, p := range m.Spec.Readiness.Probes {
		if p.Kind == "log_marker" && strings.TrimSpace(p.Pattern) != "" {
			return p.Pattern
		}
	}
	return ""
}

// stopCommand is the manifest's stop verb, as a CONSOLE command.
//
// Console and not control: PZ's RCON does not bind until the server is fully
// booted, and during a first boot with a large workshop list that is the whole
// 40-60 minutes. The channel that always works is the one the stop path uses
// (D-08).
func stopCommand(m *spec.Manifest) string {
	if m == nil {
		return ""
	}
	if v, ok := m.Spec.Verbs["stop"]; ok && strings.TrimSpace(v.Console) != "" {
		return v.Console
	}
	return ""
}

func missingBootFact(marker, stop string) string {
	switch {
	case marker == "" && stop == "":
		return "readiness log_marker and no stop verb with a console command"
	case marker == "":
		return "readiness probe of kind log_marker"
	default:
		return "stop verb with a console command"
	}
}

// pzApplySandbox is pz.apply_sandbox.
//
// It applies a preset the SERVER ITSELF ships, plus per-key overrides, to the
// SandboxVars file the server generated. Three decisions are load-bearing:
//
// The preset list is a glob over the install tree, so it is whatever this build
// of the game actually ships. A hardcoded table of preset names drifts the
// first time Indie Stone adds one, and the symptom is an enum that refuses a
// value the server would have accepted.
//
// Every key is SET on the live document rather than the preset file being
// copied over it. A preset's own file is a `return { ... }` table from the same
// build, but the live file is the one the server wrote from its current schema:
// setting keys into it preserves its structure, its comments and the keys the
// preset does not mention, and it makes a preset key the live file does not
// have a skipped line with a warning rather than a file the game cannot load.
//
// A key the live file lacks is never invented, for either the preset or the
// overrides. The game generates SandboxVars from its own schema and silently
// ignores a key it does not know, so an invented key looks like it worked and
// does nothing.
type pzApplySandbox struct{}

func (pzApplySandbox) Name() string    { return "pz.apply_sandbox" }
func (pzApplySandbox) Phases() []Phase { return []Phase{PhasePreStart} }

func (pzApplySandbox) Doc() string {
	return "Applies the sandbox_preset var -- one of the preset files the server itself ships " +
		"under media/lua/shared/Sandbox -- and then the sandbox_overrides lines, to the " +
		"server's generated <servername>_SandboxVars.lua. A key the live file does not " +
		"contain is skipped with a warning and never invented, because the game ignores a key " +
		"it does not know and an invented one looks like it worked."
}

func (pzApplySandbox) Run(_ context.Context, h *Ctx) error {
	fsys, err := h.fsOf()
	if err != nil {
		return err
	}
	target := presetTarget(h.Manifest)
	if target == "" {
		return fmt.Errorf("%w: this manifest declares no presets.target, so there is no "+
			"document to apply a preset to", ErrHook)
	}

	doc, commit, err := h.openConfig(target)
	if err != nil {
		// Not fatal, and this is the one judgement in the hook that matters.
		// pre_start runs BEFORE exec, and the SandboxVars file is generated by
		// the server itself -- so on an instance whose first boot has not
		// happened yet, the file is legitimately absent. Failing here would
		// prevent the start that creates it, which is a deadlock: the settings
		// would never be applied because the file would never exist.
		if errors.Is(err, ErrConfig) {
			h.Problem("the sandbox settings were not applied: %v. They will apply on the next "+
				"start, once the server has generated the file", err)
			return nil
		}
		return err
	}

	before := doc.Bytes()

	if name := strings.TrimSpace(h.Vars["sandbox_preset"]); name != "" {
		if err := applyPreset(h, fsys, doc, name); err != nil {
			return err
		}
	} else {
		h.Note("no sandbox_preset is set, so the server's generated settings are left alone " +
			"and only the overrides are applied")
	}

	applyOverrides(h, doc, h.Vars["sandbox_overrides"])

	if bytes.Equal(before, doc.Bytes()) {
		// Nothing changed, so nothing is written. A pre_start hook runs on
		// every single start, and rewriting an identical file each time would
		// churn the .bak and make the file's mtime useless for working out
		// when a setting last changed.
		h.Note("the sandbox settings are already as declared; nothing was written")
		return nil
	}
	return commit()
}

func presetTarget(m *spec.Manifest) string {
	if m == nil || m.Spec.Presets == nil {
		return ""
	}
	return m.Spec.Presets.Target
}

// applyPreset sets every key of one shipped preset onto the live document.
func applyPreset(h *Ctx, fsys JailedFS, doc gamecfg.Doc, want string) error {
	file, err := findPreset(h, want)
	if err != nil {
		return err
	}
	raw, err := fsys.ReadFile(RootInstall, file)
	if err != nil {
		return fmt.Errorf("hook: reading preset %s: %w", file, err)
	}

	// The transform the manifest declares. A preset ships as `return { ... }`,
	// which is a Lua chunk and not an assignment, so nothing can address a key
	// inside it until it is spelled the way the live file is.
	src, err := PresetToSandboxVars(raw)
	if err != nil {
		return fmt.Errorf("hook: preset %s: %w", file, err)
	}
	preset := gamecfg.ParseSandboxVars(src)

	keys := preset.Keys()
	if len(keys) == 0 {
		h.Problem("preset %s yielded no settings; it was read but nothing in it looked like a "+
			"SandboxVars table", file)
		return nil
	}

	var set, skipped int
	for _, k := range keys {
		v, ok := preset.Get(k)
		if !ok {
			continue
		}
		if err := doc.Set(k, v); err != nil {
			skipped++
			h.Note("preset key %s was skipped: %v", k, err)
			continue
		}
		set++
	}
	h.Note("applied preset %s: %d settings set, %d skipped", path.Base(file), set, skipped)
	if skipped > 0 {
		// Reported, not swallowed. A preset from a different build of the game
		// is the normal cause, and the operator is the only one who can decide
		// whether that matters.
		h.Problem("%d of preset %s's settings are not in this server's generated SandboxVars "+
			"file and were skipped rather than invented; the game ignores a key it does not "+
			"know", skipped, path.Base(file))
	}
	return nil
}

// findPreset locates one shipped preset by name.
//
// The list comes from the glob the manifest declares, so it is whatever this
// build ships, and the exclusions come from the manifest too -- PZ's own
// SandboxVars.lua lives in the same directory and is the schema, not a preset.
func findPreset(h *Ctx, want string) (string, error) {
	files, err := presetFiles(h)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if strings.EqualFold(presetName(f), want) {
			return f, nil
		}
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, presetName(f))
	}
	return "", fmt.Errorf("%w: this server ships no sandbox preset called %q; it ships %s",
		ErrHook, want, orNone(names))
}

// PresetFiles lists the preset files one instance's install tree ships.
//
// Exported because the `sandbox_preset` var declares `values_from: presets`,
// and whatever renders that enum has to read the same list from the same place
// -- otherwise the dashboard offers a value this hook then refuses.
func PresetFiles(h *Ctx) ([]string, error) { return presetFiles(h) }

func presetFiles(h *Ctx) ([]string, error) {
	fsys, err := h.fsOf()
	if err != nil {
		return nil, err
	}
	if h.Manifest == nil || h.Manifest.Spec.Presets == nil {
		return nil, fmt.Errorf("%w: this manifest declares no presets block", ErrHook)
	}
	ps := h.Manifest.Spec.Presets
	if h.Render == nil {
		return nil, fmt.Errorf("%w: no renderer; presets.source_glob is a template", ErrNoSeam)
	}
	abs, taint, err := h.Render.Render("spec.presets.source_glob", ps.SourceGlob)
	if err != nil {
		return nil, err
	}
	if taint.Has(spec.TaintSecret) {
		return nil, fmt.Errorf("%w: presets.source_glob renders a secret into a path", ErrHook)
	}
	root, pattern, err := fsys.Locate(abs)
	if err != nil {
		return nil, err
	}
	matches, err := fsys.Glob(root, pattern)
	if err != nil {
		return nil, err
	}

	excluded := make(map[string]bool, len(ps.Exclude))
	for _, e := range ps.Exclude {
		excluded[strings.ToLower(e)] = true
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if excluded[strings.ToLower(presetName(m))] {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func presetName(rel string) string {
	b := path.Base(rel)
	return strings.TrimSuffix(b, path.Ext(b))
}

// PresetToSandboxVars is the `pz_return_to_sandboxvars` transform.
//
// A preset ships as a bare Lua chunk:
//
//	return {
//	    Zombies = 1,
//	    ...
//	}
//
// while the live settings file is an assignment:
//
//	SandboxVars = {
//	    Zombies = 4,
//	    ...
//	}
//
// The two are the same table with different first lines, and gamecfg's
// addressing starts at the assignment. So the transform rewrites exactly that
// one token, on the FIRST `return {` only -- a nested `return` inside the table
// would be Lua gvm has no business rewriting -- and refuses a file where it
// cannot find one, because a preset that silently yielded no keys would read as
// "this preset changes nothing".
func PresetToSandboxVars(src []byte) ([]byte, error) {
	lines := bytes.Split(src, []byte("\n"))
	for i, line := range lines {
		trimmed := bytes.TrimSpace(bytes.TrimSuffix(line, []byte("\r")))
		if !bytes.HasPrefix(trimmed, []byte("return")) {
			continue
		}
		rest := bytes.TrimSpace(trimmed[len("return"):])
		if !bytes.HasPrefix(rest, []byte("{")) {
			continue
		}
		// The leading whitespace is kept, so a diff against the original
		// preset shows one changed token rather than a reindented file.
		indent := line[:len(line)-len(bytes.TrimLeft(line, " \t"))]
		eol := ""
		if bytes.HasSuffix(line, []byte("\r")) {
			eol = "\r"
		}
		lines[i] = append(append(append([]byte(nil), indent...),
			[]byte("SandboxVars = "+string(rest))...), []byte(eol)...)
		return bytes.Join(lines, []byte("\n")), nil
	}
	return nil, fmt.Errorf("%w: no `return {` to rewrite, so this file is not a sandbox preset",
		ErrHook)
}

// applyOverrides applies the operator's per-key lines.
//
// The FORMAT is gamecfg.ParseOverrides', including its one wart -- because
// comments are stripped first, a `#` inside a quoted value truncates it --
// which is bug-compatible with the bash the operators' existing sandbox.txt
// files were written against. Reimplementing the parse here would silently
// change what their files mean.
//
// Keys are then set one at a time rather than through SandboxVars.ApplyOverrides,
// which is the same loop: the document handed in here has passed through the
// manifest's deny_keys guard, and only its Doc methods are reachable. Setting
// per key is also what makes the report per LINE, which is the half that
// matters: an override naming a key the live file does not have is skipped and
// never invented, and "skipped line 7" is the difference between an operator
// fixing a typo and an operator wondering why their server plays differently
// than they think it does.
func applyOverrides(h *Ctx, doc gamecfg.Doc, raw string) {
	for _, o := range gamecfg.ParseOverrides([]byte(raw)) {
		if err := doc.Set(o.Key, o.Value); err != nil {
			// A warning and not a failure, so one bad line does not stop the
			// other settings from applying.
			h.Problem("override line %d (%s = %s) was skipped: %v", o.Line, o.Key, o.Value, err)
			continue
		}
		h.Note("override %s = %s", o.Key, o.Value)
	}
}

// pzResolveModIDs is pz.resolve_mod_ids.
//
// It maps the acquisition namespace (numeric Steam workshop ids) onto the
// activation namespace (mod.info `id=` values) by reading the downloaded files,
// and writes both lists into the server's config. The two are different
// identifiers for the same mod and both must persist, which is the two-list
// model the content lock exists for.
//
// The mapping itself is internal/content's: this hook compiles the manifest's
// block into a content.Schema, hands the acquired ids to content.Resolve --
// which dispatches through the closed resolver registry to the same
// `pz.resolve_mod_ids` name the manifest declares, applies
// resolver_vars.target_build and layers the section 12.5 tag table -- and
// renders the two lines with content.Activate. What is left here is the part
// that is genuinely a hook's: which config file, which key, and the two
// refusals below.
type pzResolveModIDs struct{}

func (pzResolveModIDs) Name() string    { return "pz.resolve_mod_ids" }
func (pzResolveModIDs) Phases() []Phase { return []Phase{PhasePostContent} }

func (pzResolveModIDs) Doc() string {
	return "Resolves Steam workshop ids to the internal mod ids that belong in Mods=, by " +
		"scanning each downloaded item's mod.info files and scoring the Build 41 and Build 42 " +
		"variants many items ship side by side. Writes Mods= and WorkshopItems= into the " +
		"server's ini, keeps ids that have not downloaded yet listed so the server retries " +
		"them, and reports every item it could not resolve with confidence."
}

func (r pzResolveModIDs) Run(ctx context.Context, h *Ctx) error {
	fsys, err := h.fsOf()
	if err != nil {
		return err
	}
	if h.Content == nil {
		return fmt.Errorf("%w: no content lock; activation ids are only writable in %s",
			ErrNoSeam, PhasePostContent)
	}
	block, ok := workshopBlock(h.Manifest)
	if !ok {
		return fmt.Errorf("%w: this manifest declares no steam_workshop content block for "+
			"this resolver to act on", ErrHook)
	}

	// Before Acquire, not just before the writes. An undeclared block and a
	// declared-but-empty one both answer nil from Acquire, so every sentence
	// below this point would assert the wrong fact: the note used to say "no
	// workshop items are acquired", which claims the list is EMPTY when the
	// truth is that it was never read -- the exact conflation section 12 exists
	// to prevent.
	//
	// Until this guard existed the only thing that stopped an undeclared block
	// emptying Mods= was where ContentState.SetActivation happens to sit inside
	// writeModLists, below the lock build and above the first doc.Set. That is
	// an accident of call order, not a rule, and a later author moving the
	// recording step below the commits -- to fix the real inconsistency where
	// the lock records ids a failed commit never wrote -- would commit both
	// keys empty with the whole suite green.
	if !h.Content.Declares(block.Name) {
		return fmt.Errorf("%w: this instance declares no content block named %q, although game "+
			"%s does, so there is no acquisition list to resolve. An unread list and an empty "+
			"one are the same bytes, and writing %s from either would be indistinguishable from "+
			"an operator disabling every mod -- so nothing was written. Add a `spec.content.%s`"+
			" block to the instance, or check that its key is spelled the way the manifest "+
			"names it", ErrHook, block.Name, gameOf(h.Manifest), block.Activation.WriteKey,
			block.Name)
	}

	ids := h.Content.Acquire(block.Name)
	if len(ids) == 0 {
		h.Note("this instance declares %q with no items, so there is nothing to resolve and "+
			"both keys are written empty on purpose", block.Name)
	}

	roots, err := workshopRoots(h, fsys, block)
	if err != nil {
		return err
	}

	sc, err := contentSchema(h, block)
	if err != nil {
		return err
	}

	if tb := strings.TrimSpace(sc.Activation.Vars[targetBuildVar]); tb != "" {
		// Acted on, and this is the sentence that says so. The declared target
		// decides WHICH BUILD'S MARKERS COUNT; it never decides which variant
		// wins, because selection inside an item stays purely file-based
		// (highest score wins, losers land in Dropped). So a B41 instance asks
		// the same question about its own files instead of being handed B42's
		// answer, and a declaration that disagrees with the files still cannot
		// enable a variant the files contradict.
		h.Note("resolver_vars.target_build is %q, and that is the build this resolve scored "+
			"against; selection within an item stays file-based (versionMin, 42/ and common/ "+
			"directories), so the declaration chooses the question and the files choose the "+
			"answer", tb)
	}

	p, err := r.plan(ctx, sc, ids, roots)
	if err != nil {
		return err
	}
	reportItems(h, sc, p)

	// THE GATE (section 12.2). Something in the declared set could not be
	// classified at all, so neither list is written: an unaccounted item is not
	// an ambiguous one, and the two get different responses. An ambiguous item
	// is enabled with a warning -- today's behaviour, preserved exactly -- and
	// an unaccounted one aborts, because a plan that lost track of its own
	// input cannot say what either list should contain.
	if err := content.Materializable(p); err != nil {
		return fmt.Errorf("%w: %w", ErrHook, err)
	}

	// Items are declared and NOTHING resolved. Writing the activation list
	// would start the server with every mod disabled, and PZ rewrites the ini
	// on shutdown, so the empty list becomes permanent and there is no second
	// pass that could put it back.
	//
	// The refusal covers the ACTIVATION half only. PZ's acquisition mechanism
	// is server_boot: it downloads what the acquisition key lists, and this
	// resolver is the only writer of that key. Suppressing it too would mean a
	// newly pinned id never reaches the server, so it is never downloaded, so
	// the next resolve fails identically -- which turns a recoverable state
	// into a stall and starves `keep_missing: true` of its purpose.
	if len(p.ActivationIDs()) == 0 && len(ids) > 0 {
		if len(roots) == 0 {
			// The more specific diagnosis, when it applies: no root is on disk
			// at all, so this is a download that has not started rather than
			// one whose items cannot be read.
			h.Problem("none of this game's workshop roots exist yet, so %d acquired item(s) "+
				"could not be resolved. %s was left alone", len(ids), sc.Activation.Key)
		} else {
			h.Problem("none of the %d acquired item(s) resolved to a mod id, although %d "+
				"workshop root(s) are on disk, so %s was left alone. %s still lists them, so "+
				"the server retries the download on its next boot",
				len(ids), len(roots), sc.Activation.Key, sc.Acquisition.Key)
		}
		return writeModLists(h, sc, p, false)
	}

	return writeModLists(h, sc, p, true)
}

// plan turns the acquired ids into a content.Plan.
//
// The two records it makes itself are the two it can see without reading a
// file, and both are things that would otherwise break the plan's own
// arithmetic: an entry with no id, and an id declared twice. A duplicate is not
// cosmetic -- two entries for one id make the derived activation list depend on
// which one is read, and the two can carry different notes and requires -- so it
// closes the gate with a reason rather than being de-duplicated into silence.
//
// Everything else is internal/content's: content.Resolve dispatches through the
// closed registry and records a reason for any item its resolver returned
// neither a result nor an explanation for.
func (r pzResolveModIDs) plan(ctx context.Context, sc *content.Schema, ids []string,
	roots []content.Root) (*content.Plan, error) {

	p := &content.Plan{Block: sc.Block, Source: sc.Source, Declared: len(ids)}

	items := make([]content.ResolverItem, 0, len(ids))
	// first records the declared position PLUS ONE, so the zero value means
	// "not seen". Storing the position itself would make position 0 -- the
	// library that has to load before everything else -- invisible here.
	first := make(map[content.AcquireID]int, len(ids))
	for i, raw := range ids {
		id := content.AcquireID(strings.TrimSpace(raw))
		switch {
		case id == "":
			p.Unaccounted = append(p.Unaccounted, content.Unaccounted{
				Reason: fmt.Sprintf("the item declared at position %d has no id", i),
				Hint:   "remove the entry, or give it a Steam Workshop publishedfileid",
			})
		case first[id] > 0:
			p.Unaccounted = append(p.Unaccounted, content.Unaccounted{
				ID: id,
				Reason: fmt.Sprintf("it is declared twice, at positions %d and %d",
					first[id]-1, i),
				Hint: "two entries for one id make the derived activation list depend on " +
					"which one is read, and the two can carry different notes and requires",
			})
		default:
			first[id] = i + 1
			items = append(items, content.ResolverItem{Ordinal: i, ID: id})
		}
	}
	if len(items) == 0 {
		return p, nil
	}

	// No Upstream is passed, and that is section 12.5's degradation rather than
	// an omission: a hook has no business making a Steam request in the job
	// process, so every item is scored from its downloaded files alone -- which
	// is exactly what an unreachable API produces. The metadata path is the
	// job layer's, which fetches once per sync and hands it to the source.
	res, err := content.Resolve(ctx, sc, items, roots)
	if err != nil {
		return nil, err
	}
	p.Unaccounted = append(p.Unaccounted, res.Unaccounted...)
	p.Warnings = append(p.Warnings, res.Warnings...)

	refused := make(map[content.AcquireID]bool, len(p.Unaccounted))
	for _, u := range p.Unaccounted {
		if u.ID != "" {
			refused[u.ID] = true
		}
	}
	byID := make(map[content.AcquireID]content.ResolvedItem, len(res.Items))
	for _, it := range res.Items {
		byID[it.ID] = it
	}

	for _, want := range items {
		if refused[want.ID] {
			continue
		}
		it, ok := byID[want.ID]
		if !ok {
			continue // the registry wrapper's cross-check already recorded a reason
		}
		p.Entries = append(p.Entries, content.PlanEntry{
			Ordinal:    want.Ordinal,
			Ref:        content.Ref{ID: string(want.ID)},
			Activation: it.Activation,
			Dropped:    it.Dropped,
			Evidence:   it.Evidence,
			Ambiguous:  it.Ambiguous,
			// Materialized is presence on disk, which on a server_boot source
			// is the only thing that decides whether an acquisition boot is
			// still owed. The ARTIFACT is deliberately not recorded here: this
			// hook does not write the lockfile, and a size and member count
			// taken in the job process would be a second reading of the same
			// tree that the source's Materialize takes properly.
			Materialized: !it.Missing,
			Resolution:   pzResolution(sc),
		})
	}
	return p, nil
}

// pzResolution records HOW an entry's bytes arrive.
//
// server_boot means the SERVER downloads them and gvm never holds them, which
// is why the artifact is a tree with no digest until `gvm content verify
// --deep`. Any other mechanism on this block is refused by contentSchema's
// compile, so there is no third answer to give.
func pzResolution(sc *content.Schema) content.ResolutionPath {
	if sc.ServerBoot() {
		return content.ResolutionServerBoot
	}
	return content.ResolutionAPI
}

// contentSchema compiles the manifest's block plus the instance's activation
// policy into the schema every later step reads fields off.
//
// It is where the two-namespace rules become refusals instead of review
// comments: one key cannot carry both lists, a missing separator is refused
// rather than guessed (PZ's lists are semicolon-separated and a comma breaks
// them with NO error), and the activation resolver this manifest names has to
// exist in the build that is about to run it.
//
// The roots are not passed in. content.Schema records them for a source that
// has to open them; this hook opens its own through Ctx.Fs, which is the only
// filesystem a hook may touch.
func contentSchema(h *Ctx, block spec.ContentSpec) (*content.Schema, error) {
	vars, err := resolverVars(h, block)
	if err != nil {
		return nil, err
	}
	// A copy, with the rendered values in place of the templates. The block
	// itself is the pinned manifest and nothing here may edit it.
	block.Activation.ResolverVars = vars

	sc, err := content.Compile(block, spec.ContentList{
		// The instance's half, and the only field of it a hook can see.
		// `preserve` is the adoption switch: the live activation line is the
		// operator's own and content.Activate hands it back untouched.
		ActivationPolicy: h.Content.ActivationPolicy(block.Name),
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHook, err)
	}
	return sc, nil
}

// reportItems is the per-item half of the report: the notes and problems an
// operator reads, from the plan rather than from a second walk of the disk.
func reportItems(h *Ctx, sc *content.Schema, p *content.Plan) {
	for _, e := range p.Entries {
		id := e.AcquireID()
		switch {
		case !e.Materialized && hasEvidence(e.Evidence, "modinfo:no-usable-mod-info"):
			h.Problem("%s is on disk but contains no usable mod.info, so nothing from it was "+
				"enabled", id)
		case !e.Materialized:
			h.Problem("%s has not downloaded yet; it stays listed in %s so the server retries "+
				"it on the next boot", id, sc.Acquisition.Key)
		case e.Ambiguous:
			h.Problem("%s has no build marker anywhere in it, so every mod id inside it was "+
				"enabled (%s) and a Build 41 variant may have slipped in. Check it against the "+
				"workshop page", id, joinActivation(e.Activation, ", "))
		case len(e.Activation) > 1:
			h.Note("%s contains %d mods, all enabled: %s", id, len(e.Activation),
				joinActivation(e.Activation, ", "))
		}
		for _, d := range e.Dropped {
			h.Note("%s: %s was dropped as a lower-scoring build variant", id, d)
		}
	}
	for _, u := range p.Unaccounted {
		h.Problem("%s", u.String())
	}
	// Section 12.5's doctor lines. They are MESSAGES: nothing branches on one,
	// and every finding that has a warning also has a machine-readable record
	// of itself in the entry's evidence.
	for _, w := range p.Warnings {
		h.Problem("%s", w)
	}
}

func hasEvidence(evidence []string, want string) bool {
	for _, e := range evidence {
		if e == want {
			return true
		}
	}
	return false
}

// joinActivation renders activation ids for a message. It takes
// []content.ActivationID and nothing else, so no acquisition id can be
// formatted as one by mistake.
func joinActivation(ids []content.ActivationID, sep string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id.String())
	}
	return strings.Join(parts, sep)
}

// writeModLists puts both namespaces into the config file the manifest names.
//
// activation false writes only the acquisition key: see the refusal above.
//
// The two VALUES come from content.Activate, which is the one implementation of
// section 12.2's activation contract -- the fail-closed gate and verbatim
// activation -- and the lines it returns are typed per namespace, so there is
// no call site here at which the activation ids could be written under the
// acquisition key.
func writeModLists(h *Ctx, sc *content.Schema, p *content.Plan, activation bool) error {
	// Grouped by file, so a manifest writing both keys into the same ini
	// commits once. Two commits to one file would mean two atomic replaces and
	// two .bak rotations for one logical change.
	type pending struct {
		doc    gamecfg.Doc
		commit Commit
		before []byte
	}
	open := map[string]*pending{}
	file := func(ns content.Namespace, name string) (*pending, error) {
		if name == "" {
			// A write key with no file writes nowhere. Refused rather than
			// skipped: a block that declared one key and no file would produce
			// a run that reported success and changed nothing, which on the
			// acquisition half means a server that never downloads.
			return nil, fmt.Errorf("%w: content block %q names a %s write key with no "+
				"write_file, so there is nothing to write it into", ErrHook, sc.Block, ns)
		}
		if p := open[name]; p != nil {
			return p, nil
		}
		doc, commit, err := h.openConfig(name)
		if err != nil {
			return nil, err
		}
		pd := &pending{doc: doc, commit: commit, before: doc.Bytes()}
		open[name] = pd
		return pd, nil
	}

	acqDoc, err := file(content.NamespaceAcquisition, sc.Acquisition.File)
	if err != nil {
		return err
	}
	var actDoc *pending
	if activation {
		if actDoc, err = file(content.NamespaceActivation, sc.Activation.File); err != nil {
			return err
		}
	}

	sl, err := p.Lock(sc)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrHook, err)
	}
	if sc.VerbatimActivation {
		// The adoption switch, fed from the file itself. The live line is the
		// ONLY copy of the operator's list -- the entries cannot reproduce a
		// hand-pruned ordering -- so recording anything else here, including
		// nothing, is how that edit gets lost.
		if actDoc != nil {
			live, _ := actDoc.doc.Get(sc.Activation.Key)
			sl.ActivationLine = live
		}
	}

	type write struct {
		into *pending
		file string
		key  string
		val  string
	}
	var writes []write

	if activation {
		l := content.NewLock(&content.Input{Instance: h.Name, Game: gameOf(h.Manifest)})
		if err := l.Put(sc.Block, sl); err != nil {
			return err
		}
		// The accept form only when the DECLARED list is empty, which is the
		// one shape of "write an empty activation line" that is intent: the
		// operator removed every mod. Keyed on the declared count and not on
		// the live line, because an adopted server whose operator just removed
		// every mod has a non-empty live Mods= and an empty declared list, and
		// that is legitimate. Everything else -- including a list nobody read
		// -- takes content.Activate and its refusal.
		activate := content.Activate
		if p.Declared == 0 {
			activate = content.ActivateAllowingEmpty
		}
		act, err := activate(sc, l)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrHook, err)
		}
		if act.Verbatim() {
			h.Note("%s", act.Note)
		}
		// Activation first, so the declared order of the two keys in a shared
		// file is unchanged from when both were always written.
		writes = []write{
			{actDoc, sc.Activation.File, act.Activate.Key, act.Activate.Value()},
			{acqDoc, sc.Acquisition.File, act.Acquisition.Key, act.Acquisition.Value()},
		}

		if !act.Verbatim() {
			ids := make([]string, 0, len(act.Activate.IDs))
			for _, id := range act.Activate.IDs {
				ids = append(ids, id.String())
			}
			// SetActivation also refuses an undeclared block, but that is no
			// longer what makes an undeclared block safe: the Declares guard at
			// the top of Run is. Recorded here because this call's POSITION
			// used to be the only defence, which made moving it -- a change
			// with a legitimate motivation, since a failed commit below leaves
			// the lock recording ids that never reached disk -- quietly
			// catastrophic.
			if err := h.Content.SetActivation(sc.Block, ids); err != nil {
				return err
			}
		}
	} else {
		acq := sc.Acquisition.Line(sl.AcquireIDs(sc.Acquisition.KeepMissing))
		writes = []write{{acqDoc, sc.Acquisition.File, acq.Key, acq.Value()}}
	}

	for _, w := range writes {
		if err := w.into.doc.Set(w.key, w.val); err != nil {
			return fmt.Errorf("hook: setting %s in %q: %w", w.key, w.file, err)
		}
		h.Note("%s = %s", w.key, w.val)
	}

	for name, pd := range open {
		if bytes.Equal(pd.before, pd.doc.Bytes()) {
			h.Note("%s already lists these mods; nothing was written", name)
			continue
		}
		if err := pd.commit(); err != nil {
			return err
		}
	}
	return nil
}

func gameOf(m *spec.Manifest) string {
	if m == nil {
		return ""
	}
	return m.Metadata.ID
}

// workshopBlock finds the content block this resolver is declared on.
//
// By the declaration and not by position: a manifest with two content blocks
// (a workshop list and an http one) must not have its resolver applied to
// whichever happens to be first.
func workshopBlock(m *spec.Manifest) (spec.ContentSpec, bool) {
	if m == nil {
		return spec.ContentSpec{}, false
	}
	for _, c := range m.Spec.Content {
		if c.Activation.Resolver == "pz.resolve_mod_ids" {
			return c, true
		}
	}
	for _, c := range m.Spec.Content {
		if c.Source == "steam_workshop" {
			return c, true
		}
	}
	return spec.ContentSpec{}, false
}

// targetBuildVar is the resolver_var Project Zomboid's manifest declares, as
// `target_build: "{{ .Var.pz_build }}"`. It is spelled the same in
// internal/content's resolver, which is the package that decodes it.
const targetBuildVar = "target_build"

// resolverVars renders the block's resolver_vars.
//
// A resolver NEVER renders: rendering needs the instance's vars, ports and
// secrets, and a resolver that could render could read a secret into a path.
// So the hook renders, and refuses a value that expanded a secret -- the same
// refusal workshopRoots makes below, for the same reason: this map is handed
// to a resolver whose whole output is written into a config file an operator
// pastes into support threads.
func resolverVars(h *Ctx, block spec.ContentSpec) (map[string]string, error) {
	raw := block.Activation.ResolverVars
	if len(raw) == 0 {
		return nil, nil
	}
	if h.Render == nil {
		return nil, fmt.Errorf("%w: no renderer; resolver_vars are templates", ErrNoSeam)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		field := fmt.Sprintf("spec.content[%s].activation.resolver_vars.%s", block.Name, k)
		val, taint, err := h.Render.Render(field, v)
		if err != nil {
			return nil, err
		}
		if taint.Has(spec.TaintSecret) {
			return nil, fmt.Errorf("%w: %s renders a secret into a resolver variable", ErrHook,
				field)
		}
		out[k] = val
	}
	return out, nil
}

// workshopRoots renders the block's roots and opens each one that exists.
//
// In declared order, and a missing one is skipped rather than fatal: the server
// writes workshop content to different locations depending on version and both
// may exist on the same box, which is why the manifest lists two. internal/pz
// searches them in order and the first holding an item wins.
func workshopRoots(h *Ctx, fsys JailedFS, block spec.ContentSpec) ([]content.Root, error) {
	var out []content.Root
	for i, r := range block.Roots {
		field := fmt.Sprintf("spec.content[%s].roots[%d]", block.Name, i)
		abs, taint, err := h.Render.Render(field, r)
		if err != nil {
			return nil, err
		}
		if taint.Has(spec.TaintSecret) {
			return nil, fmt.Errorf("%w: %s renders a secret into a path", ErrHook, field)
		}
		root, rel, err := fsys.Locate(abs)
		if err != nil {
			// A root outside the instance's own trees is a manifest bug, not a
			// missing download, and resolving against the three that are left
			// would quietly produce a shorter mod list.
			return nil, fmt.Errorf("%w: %s (%s): %w", ErrHook, field, abs, err)
		}
		if st, err := fsys.Stat(root, rel); err != nil || !st.IsDir() {
			h.Note("workshop root %s is not there yet", abs)
			continue
		}
		sub, err := fsys.Sub(root, rel)
		if err != nil {
			return nil, err
		}
		out = append(out, content.Root{Name: abs, FS: sub})
	}
	return out, nil
}
