// Package hook is the ~10% of a game that cannot honestly be data (D-21).
//
// A manifest names hooks; it does not define them. `hooks: {post_content:
// [pz.resolve_mod_ids]}` resolves against a CLOSED registry in this package,
// and an unknown name is a load-time error naming file:line. That one property
// is what keeps the manifest schema from becoming a plugin system, and it is
// the reason internal/spec's closed template grammar exists at all: a template
// with no control flow and a hook list that can name arbitrary code would be
// the same hole with an extra step.
//
// Two rules are enforced by tests rather than by review:
//
//	noexec_test.go    nothing here spawns a process. If a hook needs to run a
//	                  program, it is a Provider -- which runs in a transient
//	                  systemd service as the instance user (D-16), not in gvm's
//	                  root process. A hook that could shell out would make the
//	                  manifest a scripting language through the back door. The
//	                  authoritative form of this is the import-graph assertion
//	                  in internal/platform/deps_test.go, which also sees an
//	                  indirect route through internal/jobs.
//	registry_test.go  every hook named in every shipped manifest resolves and
//	                  declares the phase it runs in. A manifest naming a hook
//	                  that does not exist fails the build, not the first boot
//	                  at 3am.
//
// The other three contract rules are structural. A hook cannot start, stop or
// signal the game, because nothing in Ctx can: the console handle is
// read-only and sending a command is a VERB, which is RBAC-gated and audited. A
// hook writes only through Ctx.Fs, which addresses a (root, relative path) pair
// and resolves it through a jail. And a hook cannot read the database, another
// instance or the network, because none of those is reachable from Ctx.
package hook

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/gamecfg"
	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/provider/steamcmd"
	"github.com/S95F/gameVPSmanager/internal/secrets"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Phase is when a hook runs (section 8.8).
type Phase string

const (
	// PhaseValidate runs during `gvm apply` and `gvm game validate`, and is
	// READ-ONLY. It is where a hook says "this manifest cannot work on this
	// instance" before anything is changed.
	PhaseValidate Phase = "validate"
	// PhasePostInstall runs in the job process, after Provider.Apply.
	PhasePostInstall Phase = "post_install"
	// PhasePostContent runs in the job process, after content is materialised
	// and before it is activated. It is the one phase that may mutate the
	// two-namespace content lock.
	PhasePostContent Phase = "post_content"
	// PhasePreStart runs in the runner, before exec, as the instance user.
	PhasePreStart Phase = "pre_start"
	// PhasePostStart runs in the runner after READY=1 -- with players already
	// connecting, which is why a failure here is degraded rather than fatal.
	PhasePostStart  Phase = "post_start"
	PhasePreBackup  Phase = "pre_backup"
	PhasePostBackup Phase = "post_backup"
)

// Phases returns every phase, in the order a job runs them.
func Phases() []Phase {
	return []Phase{
		PhaseValidate, PhasePostInstall, PhasePostContent,
		PhasePreStart, PhasePostStart, PhasePreBackup, PhasePostBackup,
	}
}

// Valid reports whether p is a declared phase.
func (p Phase) Valid() bool {
	for _, q := range Phases() {
		if p == q {
			return true
		}
	}
	return false
}

// The refusals. Separate sentinels because the next move differs: an unknown
// name is a manifest to fix, a wrong phase is a manifest to move a line in, and
// a missing seam is a gvm bug in the layer that built the Ctx.
var (
	// ErrUnknownHook is a name the registry does not hold.
	ErrUnknownHook = errors.New("hook: unknown hook")
	// ErrWrongPhase is a hook named in a phase it does not declare.
	ErrWrongPhase = errors.New("hook: hook is not declared for this phase")
	// ErrNoSeam is a Ctx missing something the hook needs -- no filesystem, no
	// config accessor, no content lock in post_content.
	ErrNoSeam = errors.New("hook: the hook context is incomplete")
	// ErrHook is a hook refusing to act on what it found.
	ErrHook = errors.New("hook: refused")
)

// Hook is one named piece of per-game Go logic.
//
// CONTRACT -- all five enforced by tests, not convention:
//
//  1. A hook MUST NOT exec a process. If it needs to, it is a Provider.
//     Enforced on the IMPORT GRAPH, by
//     internal/platform/deps_test.go: os/exec and internal/jobs must be
//     unreachable from this package for linux and for windows. The graph and
//     not a grep, because internal/jobs exports RunSandboxed and nothing here
//     prevents importing it -- a source scan walks straight past that.
//     noexec_test.go is the second line of defence.
//  2. A hook MUST be idempotent. Every phase can re-run after a crash, and
//     `gvm apply` re-runs validate on every invocation.
//  3. A hook MUST NOT start, stop or signal the game.
//  4. A hook writes ONLY through Ctx.Fs, which is jailed to the instance's own
//     four roots.
//  5. A hook MUST NOT read the database, another instance, or the network
//     except via Ctx.HTTP (rate-limited, User-Agent pinned).
type Hook interface {
	Name() string
	// Phases is the closed list of phases this hook may be named in. It is
	// what makes "pz.first_boot in pre_start" a validation error rather than a
	// hook that boots the server on every restart.
	Phases() []Phase
	// Doc is rendered by `gvm game hooks` and by the manifest reference.
	Doc() string
	Run(ctx context.Context, h *Ctx) error
}

// Commit writes a config document back: atomic replace, a .bak beside it, and
// an fsync of the directory.
type Commit func() error

// ConsoleView is a hook's read-only window on the console ring.
//
// Narrower than console.Ring deliberately: a hook gets the last n lines and
// nothing else. No cursor, no subscription, and above all no writer -- sending
// a command to a running game is a verb, verbs are RBAC-gated and audited, and
// a hook that could inject one would be an audit hole with a manifest for a
// front end. The job layer adapts the ring's cursor API to this shape.
type ConsoleView interface {
	Snapshot(n int) []console.Line
}

// Ctx is the complete surface. Anything not here is unreachable from a hook.
type Ctx struct {
	Phase    Phase
	Name     string
	Manifest *spec.Manifest  // read-only
	Vars     spec.VarValues  // read-only
	Ports    spec.PortValues // read-only
	Paths    provider.Paths  // Jailed
	Render   *spec.Renderer

	// Config opens one of the manifest's config_files by NAME.
	//
	// By name and not by path, so a hook cannot address a file the manifest
	// does not declare. The accessor rejects deny_keys and refuses any
	// owner:server file while the unit is active -- the game rewrites those on
	// shutdown, so editing one under a live server is a write that vanishes.
	// NewConfigAccess builds it.
	Config func(name string) (gamecfg.Doc, Commit, error)

	// Content is non-nil only in PhasePostContent -- the one place a hook may
	// mutate the two-namespace lock.
	Content *ContentState

	Console ConsoleView

	// Secret returns a credential as a secrets.Value rather than as a string.
	//
	// Section 8.8 sketches this as returning "a tainted string"; the type that
	// actually carries the taint is secrets.Value, whose String() and Format()
	// refuse to print, so a hook that interpolated a password into an error
	// message produces the redaction rather than the password.
	Secret func(name string) (secrets.Value, error)

	Fs JailedFS

	// Links is the symlink half of a hook's filesystem, and it exists only
	// because the Steam shims ARE symlinks: ~/.steam/root points at
	// ~/.local/share/Steam and ~/.steam/sdk64/steamclient.so points into the
	// steamcmd installation, both by Valve's design. JailedFS cannot express
	// one, and provider.Paths.Dest -- the strict install jail -- refuses a
	// path that reaches its target through one, correctly, since that is
	// zip-slip's second act. These paths are under Home and Work, which the
	// install jail does not own at all.
	//
	// It is steamcmd.ShimFS because the plan and the apply both already live
	// in internal/provider/steamcmd, and a second implementation here would be
	// a second set of rules about which link points where. nil means the real
	// filesystem: a nil seam that silently skipped a shim would produce a
	// server that starts, is called active by systemd, and is invisible to
	// every player.
	Links steamcmd.ShimFS

	// SteamCMDDir is where the steamcmd tarball was extracted, which is the
	// sdk symlinks' target directory. It is host configuration (gvm.yaml) and
	// not a manifest field, so it arrives here rather than being looked up.
	// Empty means steamcmd.DefaultSteamCMDDir.
	SteamCMDDir string

	HTTP *http.Client
	Log  *slog.Logger

	// What the run produced, collected rather than returned, because Run's
	// signature is error and a hook has findings that are neither a failure
	// nor a value.
	notes    []string
	problems []string
	boot     *BootRequest
}

// Logger is Ctx.Log, never nil.
func (h *Ctx) Logger() *slog.Logger {
	if h == nil || h.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Log
}

// Note records something an operator should see that is not a problem: a shim
// already in place, a preset that was copied, a declared value that had no
// effect.
func (h *Ctx) Note(format string, args ...any) {
	h.notes = append(h.notes, fmt.Sprintf(format, args...))
}

// Problem records a refusal the install survives.
//
// Not an error: a steamclient.so an operator placed in ~/.steam/sdk64 by hand
// works perfectly, and replacing a file gvm did not create is not a hook's call
// to make. Returning an error for it would abort a post_install phase over
// something that is already correct. `gvm doctor` shows these.
func (h *Ctx) Problem(format string, args ...any) {
	h.problems = append(h.problems, fmt.Sprintf(format, args...))
}

// Notes and Problems are what the last run collected.
func (h *Ctx) Notes() []string    { return h.notes }
func (h *Ctx) Problems() []string { return h.problems }

// BootRequest is a hook asking the job pipeline to run the game once.
//
// Contract rule 3 says a hook must not start the game, and pz.first_boot
// genuinely needs a boot to have happened: Project Zomboid generates
// <servername>.ini itself on its first run, and gvm cannot edit a file the
// server has not written yet. Those are reconciled by making the hook ask
// rather than act -- it has no handle that could start anything, and the
// pipeline that owns the runner, the readiness deadline and the shutdown ladder
// decides whether and how to honour the request.
type BootRequest struct {
	// Why is the operator-facing sentence, printed before a boot that will
	// take minutes.
	Why string
	// WaitFor is the log marker that means the boot has gone far enough. It
	// comes from the manifest's readiness probes rather than from the hook, so
	// a game whose marker changed is a manifest revision.
	WaitFor string
	// Then is the verb that ends the boot: a console command, because an
	// install-time boot is stopped the way a stop is stopped.
	Then string
}

// RequestBoot records the request. The last one wins; a phase whose hooks each
// wanted a boot wanted one boot.
func (h *Ctx) RequestBoot(r BootRequest) { h.boot = &r }

// BootRequested is what the pipeline reads after the phase.
func (h *Ctx) BootRequested() *BootRequest { return h.boot }

// Outcome is one hook's result.
type Outcome struct {
	Hook string
	Err  error
	// Degraded is a post_start failure: the server is up and serving players,
	// so the job is not failed. It is recorded and surfaced in doctor.
	Degraded bool
	Notes    []string
	Problems []string
}

// PhaseResult is everything one phase produced.
type PhaseResult struct {
	Phase Phase
	Ran   []Outcome
	Boot  *BootRequest
}

// Degraded reports whether any hook failed without failing the phase.
func (r *PhaseResult) Degraded() bool {
	for _, o := range r.Ran {
		if o.Degraded {
			return true
		}
	}
	return false
}

// Problems is every problem every hook reported, in order.
func (r *PhaseResult) Problems() []string {
	var out []string
	for _, o := range r.Ran {
		out = append(out, o.Problems...)
	}
	return out
}

// Run executes one phase's hooks, in list order.
//
// Ordering and failure, from section 8.8: hooks in a phase run sequentially in
// the order the manifest lists them, and the first error aborts the phase and
// fails the job -- except post_start, where the server is already up and
// serving players. There, an error is logged, recorded as a degraded hook and
// surfaced in `gvm doctor`, because failing a job whose server is live would
// either stop a running game or produce an alert nobody can act on.
//
// Every hook is resolved AND phase-checked before any of them runs. A phase
// that aborted halfway because its third entry was misspelled would have
// applied the first two, which is the opposite of what a closed registry is
// for.
func Run(ctx context.Context, h *Ctx, names []string) (*PhaseResult, error) {
	if h == nil {
		return nil, fmt.Errorf("%w: no context", ErrNoSeam)
	}
	if !h.Phase.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrWrongPhase, h.Phase)
	}
	hooks, err := Resolve(h.Phase, names)
	if err != nil {
		return nil, err
	}

	res := &PhaseResult{Phase: h.Phase}
	for _, hk := range hooks {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// Notes and problems are collected per hook, so a doctor line can name
		// which hook reported what.
		h.notes, h.problems = nil, nil
		runErr := hk.Run(ctx, h)
		out := Outcome{Hook: hk.Name(), Err: runErr, Notes: h.notes, Problems: h.problems}

		if runErr != nil && h.Phase == PhasePostStart {
			out.Degraded = true
			h.Logger().Error("hook degraded", "hook", hk.Name(), "phase", string(h.Phase),
				"err", runErr)
			res.Ran = append(res.Ran, out)
			continue
		}
		res.Ran = append(res.Ran, out)
		if runErr != nil {
			res.Boot = h.boot
			return res, fmt.Errorf("hook %s (%s): %w", hk.Name(), h.Phase, runErr)
		}
	}
	res.Boot = h.boot
	return res, nil
}

// Resolve looks up every name and checks each declares this phase.
//
// Both checks together, because the two failures an operator can make here are
// a typo and a hook in the wrong list, and only the second one looks like it
// works: pz.first_boot named in pre_start would ask for a boot before every
// single restart.
func Resolve(p Phase, names []string) ([]Hook, error) {
	out := make([]Hook, 0, len(names))
	var errs []error
	for i, n := range names {
		hk, err := Get(n)
		if err != nil {
			errs = append(errs, fmt.Errorf("hooks.%s[%d]: %w", p, i, err))
			continue
		}
		if !declares(hk, p) {
			errs = append(errs, fmt.Errorf("hooks.%s[%d]: %w: %s runs in %s",
				p, i, ErrWrongPhase, n, phaseList(hk.Phases())))
			continue
		}
		out = append(out, hk)
	}
	if len(errs) > 0 {
		// Every error, not the first: an operator fixing a manifest wants the
		// whole list, and a validation that reveals its problems one run at a
		// time turns a review into a guessing game.
		return nil, errors.Join(errs...)
	}
	return out, nil
}

func declares(hk Hook, p Phase) bool {
	for _, q := range hk.Phases() {
		if q == p {
			return true
		}
	}
	return false
}

func phaseList(ps []Phase) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}
	return strings.Join(out, ", ")
}

// HooksFor reads one phase's hook names out of a manifest.
//
// It exists so that nothing has to re-derive the mapping between a Phase and
// the HookSpec field it lives in. registry_test.go walks every shipped manifest
// through here, which is what makes "a manifest naming a hook that does not
// exist fails the build" true for every phase rather than for the ones somebody
// remembered.
func HooksFor(m *spec.Manifest, p Phase) []string {
	if m == nil {
		return nil
	}
	hs := m.Spec.Hooks
	switch p {
	case PhaseValidate:
		return hs.Validate
	case PhasePostInstall:
		return hs.PostInstall
	case PhasePostContent:
		return hs.PostContent
	case PhasePreStart:
		return hs.PreStart
	case PhasePostStart:
		return hs.PostStart
	case PhasePreBackup:
		return hs.PreBackup
	case PhasePostBackup:
		return hs.PostBackup
	}
	return nil
}

// ValidateManifest resolves every hook a manifest names, in every phase.
//
// This is the runtime half of section 8's registry closure. internal/spec has
// the load-time half (spec.HookNames), and registry_test.go asserts the two
// name the same set -- two registries that could disagree would mean a manifest
// that validates and then fails when something tries to run it.
func ValidateManifest(m *spec.Manifest) error {
	var errs []error
	for _, p := range Phases() {
		if _, err := Resolve(p, HooksFor(m, p)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Root names one of the four trees a hook may touch.
//
// A hook addresses a file as (root, relative path) rather than as a path,
// which is what makes contract rule 4 structural instead of reviewed: there is
// no spelling of an absolute path, a `..` or a sibling instance's name that
// this API accepts. Locate exists for the one case that needs the other
// direction -- a manifest template renders "{{ .Data }}/Server/x.ini" to an
// absolute path, and the hook that was handed it has to get back to a pair.
type Root string

const (
	RootInstall Root = "install"
	RootData    Root = "data"
	RootHome    Root = "home"
	RootWork    Root = "work"
)

// Roots returns the four roots, install first.
func Roots() []Root { return []Root{RootInstall, RootData, RootHome, RootWork} }

// JailedFS is every filesystem effect a hook has.
//
// A seam and not a convenience. The interesting half of a hook's write is which
// path and which mode, and neither can be exercised against a real /srv on the
// Windows development machine; every hook in this package is therefore tested
// against a real directory tree through this interface, and the one operation
// that cannot be (a symlink) belongs to the provider's ShimFS instead.
type JailedFS interface {
	// Path reports a root's absolute, symlink-resolved location. For
	// diagnostics and for a shim whose subject is a path rather than content.
	Path(r Root) (string, error)
	Stat(r Root, rel string) (fs.FileInfo, error)
	ReadFile(r Root, rel string) ([]byte, error)
	ReadDir(r Root, rel string) ([]fs.DirEntry, error)
	// Glob matches a pattern relative to a root, returning relative paths in
	// lexical order. Used for the server's OWN preset files, so the list is
	// whatever this build ships rather than a hardcoded table that drifts.
	Glob(r Root, pattern string) ([]string, error)
	MkdirAll(r Root, rel string, perm fs.FileMode) error
	// WriteFile replaces a file atomically, keeping the previous contents
	// beside it. A game server rewrites its own config files, and a hook that
	// truncated one and then failed would leave a server that cannot boot.
	WriteFile(r Root, rel string, data []byte, perm fs.FileMode) error
	// Sub is a read-only fs.FS over one subtree, which is the shape
	// internal/pz's resolver takes.
	Sub(r Root, rel string) (fs.FS, error)
	// Locate maps an absolute path back to the pair that addresses it.
	Locate(abs string) (Root, string, error)
}

// fsOf returns h.Fs or an error naming what is missing.
//
// Every hook starts with this rather than with a nil check of its own, because
// a nil check per hook is a nil check that gets forgotten in one of them, and
// the symptom is a panic inside a job that was halfway through an install.
func (h *Ctx) fsOf() (JailedFS, error) {
	if h == nil || h.Fs == nil {
		return nil, fmt.Errorf("%w: no filesystem; a hook writes only through Ctx.Fs", ErrNoSeam)
	}
	return h.Fs, nil
}

// links returns h.Links or the real filesystem.
func (h *Ctx) links() steamcmd.ShimFS {
	if h != nil && h.Links != nil {
		return h.Links
	}
	return steamcmd.OSShimFS
}

// openConfig opens a declared config file, or explains what is missing.
func (h *Ctx) openConfig(name string) (gamecfg.Doc, Commit, error) {
	if h == nil || h.Config == nil {
		return nil, nil, fmt.Errorf("%w: no config accessor, so %q cannot be opened; the job "+
			"layer builds it with NewConfigAccess", ErrNoSeam, name)
	}
	return h.Config(name)
}

// ContentState is the post_content hook's view of the two-namespace lock.
//
// Two lists per block, not one, because an acquisition id (a Steam workshop id)
// and an activation id (a mod.info `id=`) are different identifiers for the
// same mod, and conflating them is the bug internal/pz exists to prevent. It is
// a concrete type over spec.ContentValues rather than an interface the content
// lock must satisfy, so that this package does not have to wait for Phase 9 and
// so that a hook sees two string lists and nothing else of the lock.
type ContentState struct {
	blocks  map[string]*spec.ContentValues
	changed map[string]bool
	// policy is each block's activation_policy, which is an INSTANCE fact the
	// job layer reads out of the instance YAML. The zero value is `derive`,
	// because that is what every freshly created instance does; `preserve` is
	// the adoption switch and the one case where a resolver must not rewrite
	// the activation list at all.
	policy map[string]string
}

// NewContentState binds one instance's content blocks. The values are copied:
// a hook that could write through to the caller's slices would be mutating the
// lock outside the one call that records the change.
func NewContentState(in map[string]spec.ContentValues) *ContentState {
	c := &ContentState{
		blocks:  make(map[string]*spec.ContentValues, len(in)),
		changed: map[string]bool{},
		policy:  map[string]string{},
	}
	for name, v := range in {
		cv := spec.ContentValues{
			Acquire:  append([]string(nil), v.Acquire...),
			Activate: append([]string(nil), v.Activate...),
		}
		c.blocks[name] = &cv
	}
	return c
}

// SetActivationPolicy records one block's activation_policy.
//
// It is separate from NewContentState because the policy is not a value a hook
// may invent and not one the two lists imply: `preserve` means the live
// activation line is the operator's own -- an adopted server's Mods=, or a
// hand-pruned list -- and must be returned untouched rather than re-derived.
// A resolver that could not see it would rewrite exactly the list adoption
// exists to keep.
//
// An unknown value is refused rather than defaulted. `derive` is the zero
// value, so a typo'd "presrve" that fell through would silently hand the list
// back to the resolver, which is the one direction that loses work.
func (c *ContentState) SetActivationPolicy(block, policy string) error {
	if c == nil {
		return fmt.Errorf("%w: no content lock; the activation policy belongs to %s",
			ErrNoSeam, PhasePostContent)
	}
	if c.blocks[block] == nil {
		return fmt.Errorf("%w: no content block named %q in this instance's lock", ErrHook, block)
	}
	switch p := strings.TrimSpace(policy); p {
	case "", spec.ActivationDerive, spec.ActivationPreserve:
		if c.policy == nil {
			c.policy = map[string]string{}
		}
		c.policy[block] = p
		return nil
	default:
		return fmt.Errorf("%w: %q is not an activation policy for block %q (want %s|%s)",
			ErrHook, policy, block, spec.ActivationPreserve, spec.ActivationDerive)
	}
}

// ActivationPolicy is one block's policy. "" means derive, which is what every
// instance that was created rather than adopted does.
func (c *ContentState) ActivationPolicy(block string) string {
	if c == nil {
		return ""
	}
	return c.policy[block]
}

// Blocks lists the content block names, sorted.
func (c *ContentState) Blocks() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.blocks))
	for n := range c.blocks {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Declares reports whether this instance has a content block of that name at
// all.
//
// It is the discriminator a resolver needs and the two id lists cannot give it:
// a block declared with no items and a block that was never declared both
// answer nil from Acquire, and the first is an operator who removed every mod
// while the second is a list nobody read. Writing the activation key is correct
// for one and catastrophic for the other.
//
// Deliberately not keyed on len(Acquire()) == 0 for exactly that reason.
func (c *ContentState) Declares(block string) bool {
	if c == nil {
		return false
	}
	return c.blocks[block] != nil
}

// Acquire is one block's acquisition ids, in order.
func (c *ContentState) Acquire(block string) []string {
	if c == nil || c.blocks[block] == nil {
		return nil
	}
	return append([]string(nil), c.blocks[block].Acquire...)
}

// Activation is one block's activation ids, in load order.
func (c *ContentState) Activation(block string) []string {
	if c == nil || c.blocks[block] == nil {
		return nil
	}
	return append([]string(nil), c.blocks[block].Activate...)
}

// SetActivation records the resolved activation ids for a block.
//
// A block the manifest does not declare is refused rather than created: the
// lock is pinned intent (D-03), and a resolver that invented a namespace would
// write a lock no manifest can explain.
func (c *ContentState) SetActivation(block string, ids []string) error {
	if c == nil {
		return fmt.Errorf("%w: no content lock; activation ids are only writable in %s",
			ErrNoSeam, PhasePostContent)
	}
	cv := c.blocks[block]
	if cv == nil {
		return fmt.Errorf("%w: no content block named %q in this instance's lock", ErrHook, block)
	}
	next := append([]string(nil), ids...)
	if !equalStrings(cv.Activate, next) {
		c.changed[block] = true
	}
	cv.Activate = next
	return nil
}

// Changed reports whether any block's activation list was rewritten, which is
// what tells the pipeline whether the lock has to be written back.
func (c *ContentState) Changed() bool {
	if c == nil {
		return false
	}
	for _, v := range c.changed {
		if v {
			return true
		}
	}
	return false
}

// Values returns the blocks, for writing the lock back.
func (c *ContentState) Values() map[string]spec.ContentValues {
	if c == nil {
		return nil
	}
	out := make(map[string]spec.ContentValues, len(c.blocks))
	for n, v := range c.blocks {
		out[n] = spec.ContentValues{
			Acquire:  append([]string(nil), v.Acquire...),
			Activate: append([]string(nil), v.Activate...),
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
