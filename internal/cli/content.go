// `gvm content` — the two-list model from the terminal. `gvm mods` is an alias,
// because operators say "mods".
//
// Everything here reads through one loader, contentView, and that is the point:
// the declared list, the pinned manifest's block, the compiled schema, the
// lockfile and the live config are five documents that can disagree, and a
// command that assembled its own subset of them would be a sixth opinion. Four
// of the subcommands are read-only (list, diff, why, blocked), three write a
// list and nothing else (add, remove, move), and the rest refuse to guess.
//
// Two decisions shape the file:
//
// THE READ-ONLY VERBS DO NOT FETCH. The source is built Offline, so `gvm
// content diff` on a box with no egress answers exactly what it answers with
// egress. Section 12.5's rule is that the files the server downloaded are the
// authority and upstream metadata is evidence; a diff that needed Steam would
// be a diff that fails during an outage, which is when it is most wanted. The
// one command that fetches is `collection sync` (content_collection.go), and
// it says so in its own output.
//
// THE APPLYING HALF OF A SYNC IS NOT HERE. Section 12.6's ladder -- stop,
// write the acquisition key, boot for the download, stop again, resolve, write
// the activation key, start -- is one job holding the per-instance flock under
// a defer that guarantees the instance ends up started. That is internal/jobs
// (phase 10), and a second copy of it in the CLI would be a second thing that
// can leave a server stopped. `--dry-run` is the whole of the decision half,
// it is read-only, and it is safe with players online.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/S95F/gameVPSmanager/internal/avail"
	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/content/steamworkshop"
	"github.com/S95F/gameVPSmanager/internal/gamecfg"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/version"
)

var contentSubs = []struct {
	name    string
	args    string
	summary string
	run     func(context.Context, *Env, []string) error
}{
	{"list", "<instance>", "the declared list, what the lock records, and what is on disk", runContentList},
	{"diff", "<instance>", "declared vs locked vs observed — the three-way view (section 12.9)", runContentDiff},
	{"why", "<instance> <activation-id>", "explain how one mod id came to be enabled, or why it is not", runContentWhy},
	{"add", "<instance> <id>...", "declare items, at a chosen position", runContentAdd},
	{"remove", "<instance> <id>...", "undeclare items", runContentRemove},
	{"move", "<instance> <id>", "change one item's load position; nothing is ever reordered automatically", runContentMove},
	{"sync", "<instance>", "resolve and apply the declared list (--dry-run decides, read-only)", runContentSync},
	{"update", "<instance> [<id>...]", "report items whose internal ids have moved since the lock", runContentUpdate},
	{"lock", "<instance>", "show, check or re-stamp the recorded intent", runContentLock},
	{"collection", "sync <instance>", "reconcile the declared list against a Steam collection", runContentCollection},
	{"blocked", "<instance>", "items awaiting a manual download (section 12.8)", runContentBlocked},
}

func runContent(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		contentUsage(env.Stderr)
		return errors.New("name a subcommand")
	}
	switch args[0] {
	case "-h", "--help", "help":
		contentUsage(env.Stdout)
		return nil
	}
	for _, s := range contentSubs {
		if s.name == args[0] {
			return s.run(ctx, env, args[1:])
		}
	}
	contentUsage(env.Stderr)
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func contentUsage(w io.Writer) {
	fmt.Fprint(w, "gvm content — manage mods and workshop items (`gvm mods` is an alias)\n\nUsage:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range contentSubs {
		fmt.Fprintf(tw, "  gvm content %s %s\t%s\n", s.name, s.args, s.summary)
	}
	_ = tw.Flush()
	fmt.Fprint(w, "\n  Two namespaces, never interchangeable: the ACQUISITION id is what gets\n"+
		"  downloaded (a Steam publishedfileid) and the ACTIVATION id is what the game\n"+
		"  enables (a mod.info id=). `add`, `remove` and `move` take acquisition ids;\n"+
		"  `why` takes an activation id, because that is what a player's missing mod\n"+
		"  looks like in the config file.\n")
}

// contentOptions are the flags every content verb shares.
type contentOptions struct {
	Source string
	JSON   bool
	Remote string
}

// contentFlags builds one subcommand's flag set with the shared flags on it.
func contentFlags(env *Env, sub, argspec string) (*flag.FlagSet, *contentOptions) {
	fset := flag.NewFlagSet("gvm content "+sub, flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	fset.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: gvm content %s %s\n", sub, argspec)
		fset.PrintDefaults()
	}
	opt := &contentOptions{}
	fset.StringVar(&opt.Source, "source", "",
		"which content block to act on, by block name or by source kind; "+
			"required only when the game declares more than one")
	fset.BoolVar(&opt.JSON, "json", false, "print the finding as JSON")
	fset.StringVar(&opt.Remote, "remote", "",
		"dashboard URL to run this against instead of the local host")
	return fset, opt
}

// contentView is the five documents a content verb reads, loaded once.
//
// It carries the compiled schema rather than the manifest block, because every
// later step needs the RECONCILED answer: which key each namespace is written
// to, which separator, which resolver, and whether this instance preserves its
// activation line. Re-deriving those per command is how two commands end up
// disagreeing about which key gvm owns.
type contentView struct {
	Host     *HostEnv
	Name     string
	Instance *spec.Instance
	// Raw is the instance file's own bytes, kept so a list edit can replace
	// one sequence in the DOCUMENT instead of re-serialising the struct and
	// discarding every comment the operator wrote (see content_yaml.go).
	Raw      []byte
	Manifest *spec.Manifest
	Render   *spec.Renderer
	Paths    instance.Paths

	// Block and List are the manifest's half and the instance's half, matched
	// by NAME and never by position.
	Block spec.ContentSpec
	List  spec.ContentList

	Schema *content.Schema
	// Refs is the declared acquisition list, in DECLARED ORDER, which is load
	// order and which nothing in this package reorders on its own.
	Refs []content.Ref

	// Lock is the recorded intent, or nil. Nil is a normal state: a fresh
	// instance, or one whose game declares no content, has never been synced.
	Lock *content.Lock

	// LiveAbsent is "the game has not generated its config yet", recorded the
	// first time the live values are read. It is NOT the same as an empty mod
	// list: both leave the two raw values empty, and only one of them is a
	// reason to write anything.
	LiveAbsent bool

	// Roots are the declared roots that EXIST; RootsMissing are the ones that
	// do not, reported rather than silently dropped.
	Roots        []content.Root
	RootsMissing []string

	// docs caches one parsed config document per manifest config_files name,
	// so a block writing both keys into one ini reads it once.
	docs map[string]gamecfg.Doc
}

// openContent resolves the transport, loads the five documents and compiles the
// block.
//
// The Linux gate is the transport's and HostEnv's, not this function's: a test
// that supplies its own HostEnv is not touching this machine's /etc, so it
// runs, and a real invocation off Linux gets platform.ErrNotSupported in the
// same words as every other host verb.
func openContent(ctx context.Context, env *Env, opt *contentOptions, pos []string) (
	*contentView, error) {

	name, err := oneInstance(pos)
	if err != nil {
		return nil, err
	}
	t, err := Resolve(env, "content", opt.Remote)
	if err != nil {
		return nil, err
	}
	if err := t.RequireLocal(); err != nil {
		return nil, err
	}
	h, err := env.host(ctx)
	if err != nil {
		return nil, err
	}

	in, raw, err := loadInstance(h, name)
	if err != nil {
		return nil, err
	}
	m, err := pinnedManifest(h, in)
	if err != nil {
		return nil, err
	}

	block, list, err := pickContentBlock(m, in, opt.Source)
	if err != nil {
		return nil, err
	}

	v := &contentView{
		Host:     h,
		Name:     name,
		Instance: in,
		Raw:      raw,
		Manifest: m,
		Render:   spec.NewRenderer(renderData(name, in, spec.SecretValues{})),
		Paths:    h.paths(name),
		Block:    block,
		List:     list,
		Refs:     append([]content.Ref(nil), list.Acquire...),
		docs:     map[string]gamecfg.Doc{},
	}

	roots, err := v.renderRoots()
	if err != nil {
		return nil, err
	}
	// resolver_vars are TEMPLATES and a resolver never renders: rendering
	// needs this instance's vars, ports and secrets, and anything that could
	// render could read a secret into a path. So the caller renders, here,
	// and refuses a value that expanded one.
	vars, err := v.renderResolverVars()
	if err != nil {
		return nil, err
	}
	block.Activation.ResolverVars = vars

	sc, err := content.Compile(block, list, roots)
	if err != nil {
		return nil, err
	}
	v.Schema = sc

	// Minecraft mod management (Modrinth, CurseForge) is not in this build. The
	// block compiles — a jar dropped in a root loads by presence — but gvm will
	// not resolve, download or diff these sources yet, so every content verb on
	// such an instance says so in one sentence rather than four schema errors or
	// a half-working command (content-mods-3). The jars already on disk still
	// load; gvm simply does not manage them.
	if src := sc.Source; src == content.KindModrinth || src == content.KindCurseForge {
		root := "the mods directory"
		if len(roots) > 0 {
			root = roots[0]
		}
		return nil, avail.New(fmt.Sprintf("Minecraft mod management is not in this build; "+
			"jars under %s load as-is", root))
	}

	if err := v.openRoots(roots); err != nil {
		return nil, err
	}
	if v.Lock, err = v.loadLock(); err != nil {
		return nil, err
	}
	return v, nil
}

// pickContentBlock matches the manifest's block with the instance's list, BY
// NAME.
//
// A game with two content blocks -- a workshop list and an http one -- must not
// have one instance list applied to whichever happens to be first, so an
// ambiguous selection is refused with both names rather than defaulted to
// index 0.
func pickContentBlock(m *spec.Manifest, in *spec.Instance, want string) (
	spec.ContentSpec, spec.ContentList, error) {

	blocks := m.Spec.Content
	if len(blocks) == 0 {
		return spec.ContentSpec{}, spec.ContentList{}, fmt.Errorf(
			"%s declares no content sources, so there is nothing to manage; "+
				"`gvm game which %s` shows which manifest that is",
			m.Metadata.ID, m.Metadata.ID)
	}

	pick := func(b spec.ContentSpec) (spec.ContentSpec, spec.ContentList, error) {
		return b, in.Spec.Content[b.Name], nil
	}
	if w := strings.TrimSpace(want); w != "" {
		for _, b := range blocks {
			if b.Name == w {
				return pick(b)
			}
		}
		var bySource []spec.ContentSpec
		for _, b := range blocks {
			if b.Source == w {
				bySource = append(bySource, b)
			}
		}
		if len(bySource) == 1 {
			return pick(bySource[0])
		}
		return spec.ContentSpec{}, spec.ContentList{}, fmt.Errorf(
			"%s declares no content block %q; it declares %s", m.Metadata.ID, w,
			strings.Join(contentBlockNames(blocks), ", "))
	}
	if len(blocks) == 1 {
		return pick(blocks[0])
	}
	return spec.ContentSpec{}, spec.ContentList{}, fmt.Errorf(
		"%s declares %d content blocks (%s); name one with --source",
		m.Metadata.ID, len(blocks), strings.Join(contentBlockNames(blocks), ", "))
}

func contentBlockNames(blocks []spec.ContentSpec) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, fmt.Sprintf("%s (%s)", b.Name, b.Source))
	}
	sort.Strings(out)
	return out
}

// renderRoots expands the block's root templates to absolute paths.
//
// A root that renders a secret is refused rather than masked: the path reaches
// the lockfile's artifact field and the diff's header, both of which are meant
// to be safe to paste into a support thread.
func (v *contentView) renderRoots() ([]string, error) {
	out := make([]string, 0, len(v.Block.Roots))
	for i, r := range v.Block.Roots {
		field := fmt.Sprintf("spec.content[%s].roots[%d]", v.Block.Name, i)
		abs, taint, err := v.Render.Render(field, r)
		if err != nil {
			return nil, err
		}
		if taint.Has(spec.TaintSecret) {
			return nil, fmt.Errorf("%s renders a secret into a path", field)
		}
		out = append(out, abs)
	}
	return out, nil
}

func (v *contentView) renderResolverVars() (map[string]string, error) {
	raw := v.Block.Activation.ResolverVars
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for k, tmpl := range raw {
		field := fmt.Sprintf("spec.content[%s].activation.resolver_vars.%s", v.Block.Name, k)
		val, taint, err := v.Render.Render(field, tmpl)
		if err != nil {
			return nil, err
		}
		if taint.Has(spec.TaintSecret) {
			return nil, fmt.Errorf("%s renders a secret into a resolver variable", field)
		}
		out[k] = val
	}
	return out, nil
}

// openRoots opens every declared root that exists, in declared order.
//
// A missing root is SKIPPED with a note and a root that cannot be read is an
// ERROR, and the difference is the whole reason this is not one branch: "no
// items are downloaded" sends a server_boot instance into an hour-long
// acquisition boot it does not need, while "this directory could not be read"
// is a permissions problem with a one-line fix.
func (v *contentView) openRoots(roots []string) error {
	for _, abs := range roots {
		f, err := v.Host.contentRoot(abs)
		switch {
		case err == nil:
			v.Roots = append(v.Roots, content.Root{Name: abs, FS: f})
		case errors.Is(err, fs.ErrNotExist):
			v.RootsMissing = append(v.RootsMissing, abs)
		default:
			return fmt.Errorf("content root %s: %w", abs, err)
		}
	}
	return nil
}

// loadLock reads the lockfile through the Files seam.
//
// content.Load would read it with os.ReadFile, which cannot be driven on the
// development machine; the bytes and the validation are the same either way,
// because both end in content.Unmarshal.
func (v *contentView) loadLock() (*content.Lock, error) {
	raw, err := v.Host.files().ReadFile(v.Paths.ContentLock)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A normal state, not a failure: a fresh instance has never been
			// synced. Every caller that needs a lock says so in its own words.
			return nil, nil
		}
		return nil, err
	}
	l, err := content.Unmarshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", v.Paths.ContentLock, err)
	}
	return l, nil
}

// sourceLock is the block's recorded intent, or nil with the sentence a
// command should print.
func (v *contentView) sourceLock() (*content.SourceLock, string) {
	if v.Lock == nil {
		return nil, fmt.Sprintf("no content lock yet (%s does not exist), so nothing has been "+
			"recorded as intent; `gvm content sync %s` writes one", v.Paths.ContentLock, v.Name)
	}
	sl, err := v.Lock.SourceFor(v.Schema.Block)
	if err != nil {
		return nil, err.Error()
	}
	return sl, ""
}

// configDoc parses one of the manifest's declared config files.
//
// By NAME, never by path: a content verb cannot address a file the manifest
// does not declare, and the manifest is what is pinned per instance (D-17).
// The RCON password lives in the same ini as Mods=, which is why this is the
// only reader and why it hands back a parsed document rather than bytes.
func (v *contentView) configDoc(name string) (gamecfg.Doc, error) {
	if doc, ok := v.docs[name]; ok {
		return doc, nil
	}
	var cf spec.ConfigFile
	found := false
	for _, c := range v.Manifest.Spec.Configs {
		if c.Name == name {
			cf, found = c, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%s declares no config file called %q", v.Manifest.Metadata.ID,
			name)
	}
	codec, ok := gamecfg.Lookup(cf.Format)
	if !ok {
		return nil, fmt.Errorf("config file %q declares format %q, which this build has no "+
			"codec for", name, cf.Format)
	}
	field := "spec.config_files." + cf.Name + ".path"
	abs, taint, err := v.Render.Render(field, cf.Path)
	if err != nil {
		return nil, err
	}
	if taint.Has(spec.TaintSecret) {
		return nil, fmt.Errorf("%s renders a secret into a file path", field)
	}
	raw, err := v.Host.files().ReadFile(abs)
	if err != nil {
		return nil, err
	}
	doc, err := codec.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing %s (%s): %w", name, abs, err)
	}
	v.docs[name] = doc
	return doc, nil
}

// live reads the two write keys as the config file has them right now.
//
// "The game has not generated its config yet" travels on Live.Absent, and is
// mirrored on v.LiveAbsent for the report that prints the footnote. It is NOT
// the same as an empty list and is reported separately. Passing nil would mean
// "nobody read the config", which content.Resolve refuses by design: an unread
// config and an empty one are the same bytes, and only one of them is a reason
// to write anything.
//
// There is deliberately no second return value for that answer any more. Both
// call sites discarded it, the only reader was v.LiveAbsent, and on the error
// path the two channels already disagreed -- the bool was returned false while
// v.LiveAbsent kept whatever it held. Two ways to ask one question is how the
// absent reading got lost on the way to Observe.
func (v *contentView) live() (*content.Live, error) {
	out := &content.Live{ReadAt: v.Host.now().UTC()}
	absent := false
	for _, r := range []struct {
		file string
		key  string
		dst  *string
	}{
		{v.Schema.Acquisition.File, v.Schema.Acquisition.Key, &out.Acquisition},
		{v.Schema.Activation.File, v.Schema.Activation.Key, &out.Activation},
	} {
		doc, err := v.configDoc(r.file)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				absent = true
				continue
			}
			return nil, err
		}
		if val, ok := doc.Get(r.key); ok {
			// Raw, unparsed, untrimmed. Normalising it here -- sorting,
			// de-duplicating, dropping an id gvm does not recognise -- would
			// destroy the evidence that there was a hand edit at all.
			*r.dst = val
		}
	}
	out.Absent = absent
	v.LiveAbsent = absent
	return out, nil
}

// source builds the content source for this block.
//
// Offline, always: see the file comment. The source that fetches is built in
// content_collection.go, which is the one command that says it is fetching.
func (v *contentView) source() (content.Source, error) {
	switch v.Schema.Source {
	case content.KindSteamWorkshop:
		return steamworkshop.New(steamworkshop.Config{
			WorkshopAppID: v.Block.WorkshopAppID,
			Offline:       true,
		})
	default:
		return nil, fmt.Errorf("block %q is a %s block, and this build implements the content "+
			"phases for %s only; Modrinth is v1.1 and CurseForge is v1.2", v.Schema.Block,
			v.Schema.Source, content.KindSteamWorkshop)
	}
}

// input is what a Source is allowed to see.
func (v *contentView) input(live *content.Live) *content.Input {
	return &content.Input{
		Instance:       v.Name,
		Game:           v.Instance.Spec.Game,
		ManifestSHA256: v.Instance.Spec.Manifest.SHA256,
		Schema:         v.Schema,
		Roots:          v.Roots,
		Live:           live,
		Clock:          func() time.Time { return v.Host.now().UTC() },
	}
}

// plan is one read-only resolve: the declared list, classified against the
// disk and the live config. It is safe with players online.
func (v *contentView) plan(ctx context.Context) (*content.Plan, error) {
	src, err := v.source()
	if err != nil {
		return nil, err
	}
	live, err := v.live()
	if err != nil {
		return nil, err
	}
	p, err := src.Resolve(ctx, v.input(live), v.Refs)
	if err != nil {
		return nil, err
	}
	if v.Lock != nil {
		if sl, lerr := v.Lock.SourceFor(v.Schema.Block); lerr == nil {
			p.Locked = len(sl.Entries)
		}
	}
	return p, nil
}

// observe is the local reading on its own, for the commands that do not need a
// plan. It makes no request and reads no manifest decision.
func (v *contentView) observe(ctx context.Context) (*content.Observed, error) {
	src, err := v.source()
	if err != nil {
		return nil, err
	}
	live, err := v.live()
	if err != nil {
		return nil, err
	}
	return src.Observe(ctx, v.input(live))
}

// printRootNotes says which declared roots are not there.
//
// Printed rather than swallowed: on a server_boot instance an absent root is
// the ordinary pre-download state, and on an adopted one it is the symptom of a
// manifest pointing at the wrong tree. The operator is the only one who can
// tell those apart.
func (v *contentView) printRootNotes(w io.Writer) {
	if len(v.RootsMissing) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, r := range v.RootsMissing {
		fmt.Fprintf(w, "  ! content root %s is not there yet\n", r)
	}
}

// writeInstance validates and writes the edited instance YAML.
//
// Validation runs on the bytes this would WRITE, not on the flags and not on
// the struct it assembled. Two reasons, and the second is what makes a node
// edit safe: a list that is legal on its own can be illegal next to the rest of
// the instance and only the whole document can say so (the reason `gvm create`
// gives), and a surgical edit that produced a document saying something other
// than the struct intended is caught here rather than by hostops.
//
// added and header are the collection-sync extras: which refs were appended
// rather than kept, and the dated head comment to render above them (section
// 12.5). Every other verb passes nothing.
func (v *contentView) writeInstance(ctx context.Context, env *Env, refs []content.Ref,
	added []content.AcquireID, header string) error {

	body, err := v.renderInstance(refs, added, header)
	if err != nil {
		return err
	}
	t, err := Resolve(env, "content", "")
	if err != nil {
		return err
	}
	_, err = v.Host.Ops.Do(ctx, hostops.Request{
		Verb:     hostops.VerbFileWrite,
		Actor:    actorFor(t),
		Instance: v.Name,
		File:     hostops.FileInstanceSpec,
		Body:     body,
	})
	return err
}

// renderInstance produces the bytes writeInstance would put down, and validates
// them.
//
// The DOCUMENT edit first, so the operator's section comments survive; the
// full marshal only when the file has no such path to edit, which is the
// ordinary post-create state because `gvm create` writes no content block.
func (v *contentView) renderInstance(refs []content.Ref, added []content.AcquireID,
	header string) ([]byte, error) {

	in := v.Instance
	list := v.List
	list.Acquire = refs
	if list.Source == "" {
		// Recorded on the way out, so the written YAML says which namespace
		// these ids are in rather than relying on the manifest's block order.
		list.Source = v.Block.Source
	}
	if in.Spec.Content == nil {
		in.Spec.Content = map[string]spec.ContentList{}
	}
	in.Spec.Content[v.Schema.Block] = list

	body, edited, err := editAcquireSequence(v.Raw, v.Schema.Block, v.Block.Source, refs,
		added, header)
	if err != nil {
		return nil, err
	}
	if !edited {
		if body, err = spec.MarshalYAML(in); err != nil {
			return nil, err
		}
	}

	// Validation on the PRODUCED bytes. Strictly stronger than validating the
	// struct: it is the only thing that can catch a node edit whose document
	// means something other than what was intended, and it is what lets this
	// path be surgical at all.
	got, diags, err := spec.ParseInstance(v.Paths.Spec, body)
	if err != nil {
		return nil, fmt.Errorf("the instance this would write does not parse: %w", err)
	}
	if bad := diags.Errors(); len(bad) > 0 {
		return nil, fmt.Errorf("the instance this would write does not parse:\n%s", bad)
	}
	vd := spec.ValidateInstance(got, v.Manifest, spec.ValidatorInput{
		Instance: v.Name,
		Vars:     spec.VarValues(got.Spec.Vars),
		Ports:    spec.PortValues(got.Spec.Ports),
	}, nil)
	if vd.HasErrors() {
		return nil, fmt.Errorf("the instance this would write does not validate:\n%s",
			vd.Errors())
	}
	if edited {
		// The edit replaced one sequence; this is the assertion that the
		// document it produced says what the struct said, so a silent
		// divergence between the two writers is impossible rather than
		// unlikely.
		if err := sameAcquireList(got.Spec.Content[v.Schema.Block].Acquire, refs); err != nil {
			return nil, fmt.Errorf("the document edit and the declared list disagree: %w", err)
		}
	}
	return body, nil
}

// sameAcquireList compares what was written against what was intended, id for
// id and in order.
func sameAcquireList(got, want []content.Ref) error {
	if len(got) != len(want) {
		return fmt.Errorf("the written list has %d item(s) and %d were intended", len(got),
			len(want))
	}
	for i := range want {
		if content.RefID(got[i]) != content.RefID(want[i]) {
			return fmt.Errorf("item %d is %s and %s was intended", i, content.RefID(got[i]),
				content.RefID(want[i]))
		}
	}
	return nil
}

// afterListEdit is the one sentence every list edit ends with.
//
// Editing the declared list changes INTENT and nothing else: the server is
// still running whatever the lock and its config say. Saying so is what stops
// an operator believing `gvm content add` restarted anything.
func afterListEdit(w io.Writer, v *contentView, refs []content.Ref) {
	fmt.Fprintf(w, "\n  %s now declares %d item(s).\n", v.Paths.Spec, len(refs))
	fmt.Fprintf(w, "  Nothing was applied: the server still loads what %s and its config say.\n",
		v.Paths.ContentLock)
	fmt.Fprintf(w, "    gvm content diff %s      what would change\n", v.Name)
	fmt.Fprintf(w, "  applying mod changes is not available in this build; "+
		"gvm content sync %s --dry-run shows what it would do.\n", v.Name)
}

// ---------------------------------------------------------------------------
// list

func runContentList(ctx context.Context, env *Env, args []string) error {
	fset, opt := contentFlags(env, "list", "<instance>")
	observed := fset.Bool("observed", false, "read the roots and the live config too")
	blocked := fset.Bool("blocked", false, "show the items awaiting a manual download")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return flagErr(err)
	}
	v, err := openContent(ctx, env, opt, pos)
	if err != nil {
		return err
	}
	if *blocked {
		return printBlocked(ctx, env, v, opt.JSON)
	}

	var obs *content.Observed
	if *observed {
		if obs, err = v.observe(ctx); err != nil {
			return err
		}
	}
	if opt.JSON {
		return writeJSON(env.Stdout, contentListJSON(v, obs))
	}
	printContentList(env.Stdout, v, obs)
	return nil
}

// contentListReport is the --json shape. The field names are the lockfile's,
// so a dashboard reading this output and one reading the lock see one
// vocabulary rather than two.
type contentListReport struct {
	Instance string                 `json:"instance"`
	Block    string                 `json:"block"`
	Source   content.SourceKind     `json:"source"`
	Declared int                    `json:"declared"`
	Items    []contentListItem      `json:"items"`
	Unlisted []content.ActivationID `json:"unlisted,omitempty"`
}

type contentListItem struct {
	Ordinal       int                    `json:"ordinal"`
	AcquireID     content.AcquireID      `json:"acquire_id"`
	Pin           string                 `json:"pin,omitempty"`
	Note          string                 `json:"note,omitempty"`
	Requires      []content.AcquireID    `json:"requires,omitempty"`
	Vendored      bool                   `json:"vendored,omitempty"`
	ActivationIDs []content.ActivationID `json:"activation_ids,omitempty"`
	Dropped       []content.ActivationID `json:"dropped,omitempty"`
	Ambiguous     bool                   `json:"ambiguous,omitempty"`
	Evidence      []string               `json:"evidence,omitempty"`
	Locked        bool                   `json:"locked"`
	OnDisk        string                 `json:"on_disk,omitempty"`
}

func contentListJSON(v *contentView, obs *content.Observed) contentListReport {
	sl, _ := v.sourceLock()
	rep := contentListReport{
		Instance: v.Name,
		Block:    v.Schema.Block,
		Source:   v.Schema.Source,
		Declared: len(v.Refs),
	}
	for i, ref := range v.Refs {
		id := content.RefID(ref)
		it := contentListItem{
			Ordinal:   i,
			AcquireID: id,
			Pin:       ref.Pin,
			Note:      ref.Note,
			Requires:  content.RefRequires(ref),
			Vendored:  ref.Vendored,
		}
		if sl != nil {
			if e, ok := sl.Entry(id); ok {
				it.Locked = true
				it.Ordinal = e.Ordinal
				it.ActivationIDs = e.ActivationIDs
				it.Dropped = e.Dropped
				it.Ambiguous = e.Ambiguous
				it.Evidence = e.Evidence
			}
		}
		if obs != nil {
			// An id the observed reading does not hold at all is ABSENT, not
			// unknown: Observe looks at the live config and the roots, and an
			// id in neither is one nothing on this box has. Leaving the column
			// blank would read as "not checked".
			it.OnDisk = "absent"
			if oi, ok := obs.Item(id); ok {
				it.OnDisk = observedState(oi)
				if !it.Locked {
					it.ActivationIDs = oi.Activation
					it.Dropped = oi.Dropped
					it.Ambiguous = oi.Ambiguous
					it.Evidence = oi.Evidence
				}
			}
		}
		rep.Items = append(rep.Items, it)
	}
	if obs != nil {
		rep.Unlisted = obs.Unlisted
	}
	return rep
}

// observedState names what the filesystem holds for one id.
//
// "empty" is its own answer and not a flavour of "absent": a directory the
// server created and then failed to fill is a download that started and did
// not finish, and reporting it as acquired is how a mod silently never loads.
func observedState(oi content.ObservedItem) string {
	switch {
	case oi.Empty:
		return "empty"
	case oi.Present:
		return "present"
	default:
		return "absent"
	}
}

func printContentList(w io.Writer, v *contentView, obs *content.Observed) {
	sl, why := v.sourceLock()
	fmt.Fprintf(w, "\n  %s   block %s (%s), %s acquisition\n", v.Name, v.Schema.Block,
		v.Schema.Source, v.Schema.Acquisition.Mechanism)
	fmt.Fprintf(w, "  declared %d, locked %s\n\n", len(v.Refs), lockedCount(sl))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  #\t%s\t%s\tstate\tnote\n", v.Schema.Acquisition.Key,
		v.Schema.Activation.Key)
	for i, ref := range v.Refs {
		id := content.RefID(ref)
		act, state := "—", "not locked"
		if sl != nil {
			if e, ok := sl.Entry(id); ok {
				act = joinActivationIDs(e.ActivationIDs)
				state = "locked"
				if e.Ambiguous {
					state = "check"
				}
			}
		}
		if obs != nil {
			state = "absent"
			if oi, ok := obs.Item(id); ok {
				state = observedState(oi)
				if oi.Ambiguous {
					state += ", check"
				}
				if act == "—" {
					act = joinActivationIDs(oi.Activation)
				}
			}
		}
		fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\n", i, id, act, state, pinNote(ref))
	}
	_ = tw.Flush()
	v.printRootNotes(w)

	if obs != nil && len(obs.Unlisted) > 0 {
		// The "WorkshopItems= has 32 entries and Mods= has 33" discovery
		// finding, as named ids. `gvm content why` explains each one.
		fmt.Fprintf(w, "\n  ! %d activation id(s) live in %s that no declared item accounts "+
			"for:\n      %s\n    gvm content why %s <id>   explains one\n", len(obs.Unlisted),
			v.Schema.Activation.Key, joinActivationIDs(obs.Unlisted), v.Name)
	}
	if why != "" {
		fmt.Fprintf(w, "\n  %s\n", why)
	}
}

func lockedCount(sl *content.SourceLock) string {
	if sl == nil {
		return "none"
	}
	return fmt.Sprintf("%d (%d activation id(s))", len(sl.Entries), len(sl.ActivationIDs()))
}

func pinNote(ref content.Ref) string {
	var parts []string
	if ref.Pin != "" {
		parts = append(parts, "pin "+ref.Pin)
	}
	if ref.Vendored {
		parts = append(parts, "vendored")
	}
	if len(ref.Requires) > 0 {
		parts = append(parts, "requires "+strings.Join(ref.Requires, ","))
	}
	if ref.Note != "" {
		parts = append(parts, ref.Note)
	}
	return strings.Join(parts, "; ")
}

func joinActivationIDs(ids []content.ActivationID) string {
	if len(ids) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id.String())
	}
	return strings.Join(parts, ", ")
}

func joinAcquireIDs(ids []content.AcquireID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id.String())
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// add / remove / move — the three verbs that edit the declared list

// placement is where a ref goes. Exactly one of the four may be set, and the
// check is explicit rather than last-flag-wins: `--first --after X` is somebody
// changing their mind mid-command, and guessing which half they meant is how a
// library ends up below the mod that needs it.
type placement struct {
	First  bool
	Last   bool
	After  string
	Before string
}

func (p placement) count() int {
	n := 0
	for _, set := range []bool{p.First, p.Last, p.After != "", p.Before != ""} {
		if set {
			n++
		}
	}
	return n
}

func (p placement) validate() error {
	if p.count() > 1 {
		return errors.New("name one position: --first, --last, --after ID or --before ID")
	}
	return nil
}

// index resolves the placement to an insertion point in refs.
func (p placement) index(refs []content.Ref) (int, error) {
	switch {
	case p.First:
		return 0, nil
	case p.After != "":
		i, ok := indexOfRef(refs, p.After)
		if !ok {
			return 0, fmt.Errorf("--after %s: that id is not declared, so there is no position "+
				"to go after", p.After)
		}
		return i + 1, nil
	case p.Before != "":
		i, ok := indexOfRef(refs, p.Before)
		if !ok {
			return 0, fmt.Errorf("--before %s: that id is not declared, so there is no "+
				"position to go before", p.Before)
		}
		return i, nil
	default:
		return len(refs), nil
	}
}

func indexOfRef(refs []content.Ref, id string) (int, bool) {
	want := content.AcquireID(strings.TrimSpace(id))
	for i, r := range refs {
		if content.RefID(r) == want {
			return i, true
		}
	}
	return 0, false
}

// addRefs inserts add at the placement, refusing an id that is already
// declared.
//
// Refusing rather than moving: an operator typing `gvm content add` for an id
// they already have meant to change its position or its note, and silently
// relocating it would reorder a hand-tuned list without saying so.
func addRefs(refs []content.Ref, add []content.Ref, p placement) ([]content.Ref, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	for _, a := range add {
		if i, ok := indexOfRef(refs, a.ID); ok {
			return nil, fmt.Errorf("%s is already declared at ordinal %d; "+
				"`gvm content move` changes its position", a.ID, i)
		}
	}
	seen := map[content.AcquireID]bool{}
	for _, a := range add {
		id := content.RefID(a)
		if id == "" {
			return nil, errors.New("an item needs an id")
		}
		if seen[id] {
			return nil, fmt.Errorf("%s was named twice in one command", id)
		}
		seen[id] = true
	}
	at, err := p.index(refs)
	if err != nil {
		return nil, err
	}
	out := make([]content.Ref, 0, len(refs)+len(add))
	out = append(out, refs[:at]...)
	out = append(out, add...)
	return append(out, refs[at:]...), nil
}

// removeRefs drops every named id, refusing one that is not declared.
func removeRefs(refs []content.Ref, ids []string) ([]content.Ref, error) {
	drop := make(map[content.AcquireID]bool, len(ids))
	for _, raw := range ids {
		id := content.AcquireID(strings.TrimSpace(raw))
		if id == "" {
			return nil, errors.New("name an id to remove")
		}
		if _, ok := indexOfRef(refs, string(id)); !ok {
			return nil, fmt.Errorf("%s is not declared, so there is nothing to remove; "+
				"`gvm content list` shows what is", id)
		}
		drop[id] = true
	}
	out := make([]content.Ref, 0, len(refs))
	for _, r := range refs {
		if drop[content.RefID(r)] {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// moveRef changes one item's position and nothing else.
//
// This is the command CheckOrder's violations name as their fix, and the
// reason it exists rather than an automatic reorder: load order is hand-tuned
// and the reasoning lives in the list's own notes, which gvm cannot read. So
// gvm reports the conflict and the operator makes the move.
func moveRef(refs []content.Ref, id string, p placement) ([]content.Ref, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if p.count() == 0 {
		return nil, errors.New("name a position: --first, --last, --after ID or --before ID")
	}
	from, ok := indexOfRef(refs, id)
	if !ok {
		return nil, fmt.Errorf("%s is not declared, so there is nothing to move", id)
	}
	if ref := strings.TrimSpace(p.After + p.Before); ref == strings.TrimSpace(id) {
		return nil, fmt.Errorf("%s cannot be moved relative to itself", id)
	}
	moving := refs[from]
	rest := make([]content.Ref, 0, len(refs)-1)
	rest = append(rest, refs[:from]...)
	rest = append(rest, refs[from+1:]...)

	at, err := p.index(rest)
	if err != nil {
		return nil, err
	}
	out := make([]content.Ref, 0, len(refs))
	out = append(out, rest[:at]...)
	out = append(out, moving)
	return append(out, rest[at:]...), nil
}

func placementFlags(fset *flag.FlagSet) *placement {
	p := &placement{}
	fset.BoolVar(&p.First, "first", false, "put it at the head of the load order")
	fset.BoolVar(&p.Last, "last", false, "put it at the tail of the load order (the default for add)")
	fset.StringVar(&p.After, "after", "", "put it immediately after this acquisition id")
	fset.StringVar(&p.Before, "before", "", "put it immediately before this acquisition id")
	return p
}

func runContentAdd(ctx context.Context, env *Env, args []string) error {
	fset, opt := contentFlags(env, "add", "<instance> <id>... [--first|--after ID|--before ID]")
	place := placementFlags(fset)
	pin := fset.String("pin", "", "pin the item to this version (Modrinth/CurseForge; a Steam Workshop item has no per-version id)")
	note := fset.String("note", "", "the documentation line this item carries in the list")
	requires := fset.String("requires", "", "comma-separated acquisition ids this item must load after")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) < 2 {
		fset.Usage()
		return errors.New("name an instance and at least one acquisition id")
	}
	// No `--apply`: editing the list changes intent only, and applying it is not
	// available in this build, so a flag that pretended to is gone rather than
	// accepted-and-refused (U3). afterListEdit says what the next step is.

	v, err := openContent(ctx, env, opt, pos[:1])
	if err != nil {
		return err
	}
	add := make([]content.Ref, 0, len(pos)-1)
	for _, id := range pos[1:] {
		ref := content.Ref{ID: strings.TrimSpace(id), Pin: *pin, Note: *note}
		for _, r := range strings.Split(*requires, ",") {
			if r = strings.TrimSpace(r); r != "" {
				ref.Requires = append(ref.Requires, r)
			}
		}
		add = append(add, ref)
	}
	refs, err := addRefs(v.Refs, add, *place)
	if err != nil {
		return err
	}
	if err := v.writeInstance(ctx, env, refs, nil, ""); err != nil {
		return err
	}

	w := env.Stdout
	fmt.Fprintln(w)
	for _, a := range add {
		i, _ := indexOfRef(refs, a.ID)
		fmt.Fprintf(w, "  + %s  ordinal %d  %s\n", a.ID, i, pinNote(a))
	}
	printOrderFindings(w, v.Name, content.CheckOrderOf(orderItemsOf(refs)))
	afterListEdit(w, v, refs)
	return nil
}

func runContentRemove(ctx context.Context, env *Env, args []string) error {
	fset, opt := contentFlags(env, "remove", "<instance> <id>...")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) < 2 {
		fset.Usage()
		return errors.New("name an instance and at least one acquisition id")
	}
	// No `--apply`: see runContentAdd.
	v, err := openContent(ctx, env, opt, pos[:1])
	if err != nil {
		return err
	}
	refs, err := removeRefs(v.Refs, pos[1:])
	if err != nil {
		return err
	}
	if err := v.writeInstance(ctx, env, refs, nil, ""); err != nil {
		return err
	}

	w := env.Stdout
	fmt.Fprintln(w)
	for _, id := range pos[1:] {
		fmt.Fprintf(w, "  - %s\n", strings.TrimSpace(id))
	}
	// Removing an item leaves its activation ids in the live config until a
	// sync rewrites the activation key, which is a different half of the
	// model. Saying so is the difference between "removed" and "stopped
	// loading".
	fmt.Fprintf(w, "\n  The %s key still enables whatever these items contributed until a sync\n"+
		"  rewrites it; `gvm content diff %s` shows the gap.\n", v.Schema.Activation.Key, v.Name)
	printOrderFindings(w, v.Name, content.CheckOrderOf(orderItemsOf(refs)))
	afterListEdit(w, v, refs)
	return nil
}

func runContentMove(ctx context.Context, env *Env, args []string) error {
	fset, opt := contentFlags(env, "move", "<instance> <id> (--first|--last|--after ID|--before ID)")
	place := placementFlags(fset)
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 2 {
		fset.Usage()
		return errors.New("name an instance and exactly one acquisition id")
	}
	v, err := openContent(ctx, env, opt, pos[:1])
	if err != nil {
		return err
	}
	before, _ := indexOfRef(v.Refs, pos[1])
	refs, err := moveRef(v.Refs, pos[1], *place)
	if err != nil {
		return err
	}
	after, _ := indexOfRef(refs, pos[1])
	if err := v.writeInstance(ctx, env, refs, nil, ""); err != nil {
		return err
	}

	w := env.Stdout
	fmt.Fprintf(w, "\n  ~ %s  ordinal %d -> %d\n", strings.TrimSpace(pos[1]), before, after)
	printOrderFindings(w, v.Name, content.CheckOrderOf(orderItemsOf(refs)))
	afterListEdit(w, v, refs)
	return nil
}

// orderItemsOf turns the declared list into CheckOrder's input, using the
// DECLARED position as the ordinal. That is the ordering an edit has just
// changed, and checking it here is what makes a requires: violation visible
// before anything stops.
func orderItemsOf(refs []content.Ref) []content.OrderItem {
	out := make([]content.OrderItem, 0, len(refs))
	for i, r := range refs {
		out = append(out, content.OrderItem{
			Ordinal:  i,
			ID:       content.RefID(r),
			Requires: content.RefRequires(r),
			Note:     r.Note,
		})
	}
	return out
}

// printOrderFindings renders the load-order violations.
//
// The instance name goes in as an ARGUMENT rather than being substituted into a
// placeholder afterwards. A placeholder is only ever as good as the least
// careful of its call sites, and there were two: this one replaced it and
// content_diff.go's requiresDetail printed it verbatim, into both the diff rows
// and the --json payload. A ReplaceAll over the whole report also rewrote an
// operator's own note if it happened to contain the placeholder text.
func printOrderFindings(w io.Writer, name string, vs []content.OrderViolation) {
	if len(vs) == 0 {
		return
	}
	fmt.Fprintf(w, "\n  %s\n", content.OrderReport(vs, name))
}

// errContentSyncIsAJob is the one refusal shared by every path that would have
// to stop and start the server.
//
// It is a scope statement, not a stub: the decision half of a sync is built and
// read-only, and the acting half is a job that holds the per-instance lock
// across the whole window under a defer that guarantees the instance ends up
// started. Doing that from here would be a second implementation of the one
// invariant section 13.4 exists for.
func errContentSyncIsAJob(name string) error {
	return fmt.Errorf("applying a content change stops the server, writes the acquisition key, "+
		"boots once for the download, stops again, writes the activation key and starts -- one "+
		"job holding this instance's lock the whole time (section 12.6). That job is not in "+
		"this build, and running the steps from the CLI would be a second thing that can leave "+
		"a server stopped.\n"+
		"  gvm content sync %s --dry-run   decides, read-only, safe with players online\n"+
		"  gvm content diff %s             declared vs locked vs on-disk", name, name)
}

// ---------------------------------------------------------------------------
// sync

func runContentSync(ctx context.Context, env *Env, args []string) error {
	fset, opt := contentFlags(env, "sync", "<instance> [--dry-run]")
	dry := fset.Bool("dry-run", false, "resolve and print the ladder; write nothing")
	noBoot := fset.Bool("no-boot", false,
		"refuse rather than boot for a download; the default for `gvm adopt`")
	yes := fset.Bool("yes", false, "do not prompt")
	allowAmbiguous := fset.Bool("allow-ambiguous", false,
		"proceed with items that carry no build marker (they are enabled with a warning either way)")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return flagErr(err)
	}
	v, err := openContent(ctx, env, opt, pos)
	if err != nil {
		return err
	}
	// The three flags the APPLYING half honours. They are declared because
	// section 12.9 documents them and an operator typing one should not get
	// "flag provided but not defined", and a --dry-run says plainly that it
	// did not act on them: a flag silently accepted and ignored is worse than
	// one that does not exist.
	var inert []string
	for _, f := range []struct {
		name string
		set  bool
	}{{"--no-boot", *noBoot}, {"--yes", *yes}, {"--allow-ambiguous", *allowAmbiguous}} {
		if f.set {
			inert = append(inert, f.name)
		}
	}

	p, err := v.plan(ctx)
	if err != nil {
		return err
	}
	if opt.JSON {
		if err := writeJSON(env.Stdout, contentPlanJSON(v, p)); err != nil {
			return err
		}
	} else {
		printContentPlan(env.Stdout, v, p)
		if len(inert) > 0 {
			fmt.Fprintf(env.Stdout, "  %s %s the job that applies this; a decision-only run "+
				"does not act on %s.\n", strings.Join(inert, " and "), wasWere(len(inert)),
				themIt(len(inert)))
		}
	}
	if !*dry {
		return errContentSyncIsAJob(v.Name)
	}
	return nil
}

func themIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

type contentPlanReport struct {
	Instance    string                `json:"instance"`
	Block       string                `json:"block"`
	Declared    int                   `json:"declared"`
	Impact      string                `json:"impact"`
	Boot        bool                  `json:"boot"`
	Changes     []content.Change      `json:"changes,omitempty"`
	Unaccounted []content.Unaccounted `json:"unaccounted,omitempty"`
	Ambiguous   []content.AcquireID   `json:"ambiguous,omitempty"`
	Missing     []content.AcquireID   `json:"unmaterialized,omitempty"`
	Warnings    []string              `json:"warnings,omitempty"`
	Steps       []string              `json:"steps,omitempty"`
}

func contentPlanJSON(v *contentView, p *content.Plan) contentPlanReport {
	return contentPlanReport{
		Instance:    v.Name,
		Block:       p.Block,
		Declared:    p.Declared,
		Impact:      p.Impact().String(),
		Boot:        p.Boot,
		Changes:     p.Changes,
		Unaccounted: p.Unaccounted,
		Ambiguous:   p.Ambiguous(),
		Missing:     p.Unmaterialized(),
		Warnings:    p.Warnings,
		Steps:       p.Steps,
	}
}

func printContentPlan(w io.Writer, v *contentView, p *content.Plan) {
	fmt.Fprintf(w, "\n  %s   %s\n\n", v.Name, p.Summary())
	for _, c := range p.Changes {
		fmt.Fprintf(w, "  %s %s\n", changeSigil(c.Kind), c.String())
	}
	if len(p.Changes) == 0 {
		// The normal nightly answer, stated as a first-class success: nothing
		// to do must not read as something gone wrong (D-43).
		fmt.Fprint(w, "  = the declared list, the lock and the live config agree\n")
	}
	printOrderFindings(w, v.Name, content.CheckOrderPlan(p))
	v.printRootNotes(w)

	if len(p.Unaccounted) > 0 {
		fmt.Fprintf(w, "\n  REFUSING: %d of %d declared item(s) could not be accounted for, "+
			"so neither list would be written\n", len(p.Unaccounted), p.Declared)
		for _, u := range p.Unaccounted {
			fmt.Fprintf(w, "    ! %s\n", u.String())
		}
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "\n  ! %s\n", warn)
	}
	if len(p.Steps) > 0 {
		fmt.Fprintf(w, "\n  ladder (impact %s):\n", p.Impact())
		for i, s := range p.Steps {
			fmt.Fprintf(w, "   %2d. %s\n", i+1, s)
		}
	}
	fmt.Fprint(w, "\n  nothing has been applied.\n")
}

// changeDetail renders one change without repeating the sigil.
//
// content.Change.String() leads with the kind, which a report that already
// prints a sigil says twice, and it renders an absent From as "( -> x)".
func changeDetail(c content.Change) string {
	var parts []string
	switch {
	case c.From != "" && c.To != "":
		parts = append(parts, c.From+" -> "+c.To)
	case c.To != "":
		parts = append(parts, c.To)
	case c.From != "":
		parts = append(parts, c.From)
	}
	if c.Why != "" {
		parts = append(parts, c.Why)
	}
	return strings.Join(parts, "; ")
}

// shortDuration renders a boot budget the way an operator writes it: 60m, not
// 1h0m0s.
func shortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "none"
	case d%time.Minute == 0 && d < 24*time.Hour:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return d.String()
	}
}

// needs agrees the verb with the count, because "1 item need an acquisition
// boot" is the sort of line that makes an operator doubt the number too.
func needs(n int) string {
	if n == 1 {
		return "needs"
	}
	return "need"
}

func changeSigil(k content.ChangeKind) string {
	switch k {
	case content.ChangeAdd:
		return "+"
	case content.ChangeRemove:
		return "-"
	case content.ChangeNote:
		return "="
	default:
		return "~"
	}
}

// ---------------------------------------------------------------------------
// update

func runContentUpdate(ctx context.Context, env *Env, args []string) error {
	fset, opt := contentFlags(env, "update", "<instance> [<id>...]")
	to := fset.String("to", "", "pin the named items to this version")
	dry := fset.Bool("dry-run", false, "report only (which is all this command does)")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) == 0 {
		fset.Usage()
		return errors.New("name an instance")
	}
	v, err := openContent(ctx, env, opt, pos[:1])
	if err != nil {
		return err
	}
	_ = *dry

	if strings.TrimSpace(*to) != "" && v.Schema.Source == content.KindSteamWorkshop {
		// Refused by name rather than accepted and ignored. Steam publishes no
		// per-version id for a workshop item: the server downloads whatever the
		// item is NOW, which is exactly why section 12.6 step 10 exists and why
		// `update` here is a report rather than a pin move.
		return fmt.Errorf("--to cannot pin a Steam Workshop item: the Workshop has no "+
			"per-version id, so the server downloads whatever the item is at boot time. "+
			"What changed since the lock is what `gvm content update %s` reports; a pin is a "+
			"Modrinth (v1.1) and CurseForge (v1.2) concept", v.Name)
	}

	p, err := v.plan(ctx)
	if err != nil {
		return err
	}
	sl, why := v.sourceLock()
	if sl == nil {
		return errors.New(why)
	}
	only := map[content.AcquireID]bool{}
	for _, id := range pos[1:] {
		only[content.AcquireID(strings.TrimSpace(id))] = true
	}

	w := env.Stdout
	fmt.Fprintf(w, "\n  %s   %s\n\n", v.Name, p.Summary())
	moved := 0
	for _, e := range p.Entries {
		id := e.AcquireID()
		if len(only) > 0 && !only[id] {
			continue
		}
		rec, ok := sl.Entry(id)
		if !ok {
			fmt.Fprintf(w, "  + %s  declared and not in the lock\n", id)
			moved++
			continue
		}
		was, now := joinActivationIDs(rec.ActivationIDs), joinActivationIDs(e.Activation)
		if was == now {
			continue
		}
		// Section 12.6 step 10's finding: the SERVER pulled a new version of
		// an already-listed item and the item's internal ids changed. That is
		// update.sh's blind second restart, replaced by its data-driven
		// descendant.
		fmt.Fprintf(w, "  ~ %s  %s: %s -> %s\n", id, v.Schema.Activation.Key, was, now)
		moved++
	}
	if moved == 0 {
		fmt.Fprintf(w, "  = every locked item still resolves to the same %s ids\n",
			v.Schema.Activation.Key)
	}
	v.printRootNotes(w)
	fmt.Fprintf(w, "\n  nothing has been applied; `gvm content sync %s` writes it.\n", v.Name)
	return nil
}

// ---------------------------------------------------------------------------
// lock

func runContentLock(ctx context.Context, env *Env, args []string) error {
	fset, opt := contentFlags(env, "lock", "<instance> [--check|--refresh]")
	check := fset.Bool("check", false,
		"fail if the lock does not match the pinned manifest or its own entries")
	refresh := fset.Bool("refresh", false,
		"re-stamp the lock against the pinned manifest, changing no list")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return flagErr(err)
	}
	if *check && *refresh {
		return errors.New("--check reports and --refresh writes; run one at a time")
	}
	v, err := openContent(ctx, env, opt, pos)
	if err != nil {
		return err
	}
	sl, why := v.sourceLock()
	if sl == nil {
		return errors.New(why)
	}

	w := env.Stdout
	pin := v.Instance.Spec.Manifest.SHA256
	structural := v.Lock.ValidateAgainst(v.Schema)
	manifest := v.Lock.CheckManifest(pin)
	order := content.CheckOrder(sl.Entries)

	if *refresh {
		if structural != nil {
			// Re-stamping a lock that does not validate would record a
			// manifest digest against entries gvm cannot trust, which makes
			// the next --check pass for the wrong reason.
			return fmt.Errorf("%s does not validate, so it will not be re-stamped: %w",
				v.Paths.ContentLock, structural)
		}
		v.Lock.ManifestSHA256 = pin
		v.Lock.GeneratedAt = v.Host.now().UTC()
		v.Lock.GVMVersion = version.Version
		body, err := content.Marshal(v.Lock)
		if err != nil {
			return err
		}
		t, err := Resolve(env, "content", "")
		if err != nil {
			return err
		}
		res, err := v.Host.Ops.Do(ctx, hostops.Request{
			Verb:     hostops.VerbFileWrite,
			Actor:    actorFor(t),
			Instance: v.Name,
			File:     hostops.FileContentLock,
			Body:     body,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\n  re-stamped %s against manifest %s\n", res.Path, short(pin))
		fmt.Fprint(w, "  the two lists are unchanged: --refresh records which manifest the "+
			"intent was computed against and nothing else.\n")
		return nil
	}

	fmt.Fprintf(w, "\n  %s   %s\n", v.Paths.ContentLock, v.Lock.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "  gvm %s, manifest %s, %d entries, %d activation id(s)\n", v.Lock.GVMVersion,
		short(v.Lock.ManifestSHA256), len(sl.Entries), len(sl.ActivationIDs()))
	fmt.Fprintf(w, "  %s\n", verbatimLine(sl, v.Schema))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", mark(structural == nil, "the recorded lines render from the "+
		"recorded entries"))
	fmt.Fprintf(w, "  %s\n", mark(manifest == nil, "the lock was computed against the pinned "+
		"manifest "+short(pin)))
	fmt.Fprintf(w, "  %s\n", mark(len(order) == 0, fmt.Sprintf(
		"%d declared requires: edge(s) hold against the recorded order", countRequires(sl))))
	for _, e := range []error{structural, manifest} {
		if e != nil {
			fmt.Fprintf(w, "\n  ✗ %v\n", e)
		}
	}
	printOrderFindings(w, v.Name, order)

	if *check {
		if err := errors.Join(structural, manifest); err != nil {
			return err
		}
		if len(order) > 0 {
			return fmt.Errorf("%d load-order violation(s)", len(order))
		}
	}
	return nil
}

func countRequires(sl *content.SourceLock) int {
	n := 0
	for _, e := range sl.Entries {
		n += len(e.Requires)
	}
	return n
}

func verbatimLine(sl *content.SourceLock, sc *content.Schema) string {
	if sl.VerbatimActivation {
		return fmt.Sprintf("verbatim_activation: the recorded %s line is the operator's and is "+
			"returned untouched (handing it back to the resolver is not available in this build)",
			sc.Activation.Key)
	}
	return fmt.Sprintf("verbatim_activation: false, so %s is derived from the entries",
		sc.Activation.Key)
}
