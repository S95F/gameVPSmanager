// The closed template surface (ARCHITECTURE.md 8.3, D-21).
//
// A game manifest is DATA. What it embeds is not a scripting language and must
// never become one: everything below is enumerated, and anything not listed is
// refused when the manifest LOADS, with the manifest's own file:line, rather
// than at 3am when a restart renders the field for the first time. The refusal
// itself lives in render_guard.go.
//
// VARIABLES — the complete list:
//
//	.Name .Game                     instance name, game id
//	.Install .Data .Home .Work      absolute paths; .Work is the per-instance
//	                                SteamCMD HOME (D-30)
//	.User .Group                    the unix identity the game runs as
//	.Port.<name>                    int; <name> must be declared in ports[]
//	.Var.<name>                     string; <name> must be declared in vars
//	.Secret.<name>                  string; SETS THE TAINT FLAG
//	.Content.<name>.Acquire         []string, joined with the block's separator
//	.Content.<name>.Activate        []string
//	.Host.RamMB .Host.CPUs          int
//	.Host.IP4                       string
//	.Phase                          install | content | normal
//	.Msg .Player .Reason .Duration  VERB TEMPLATES ONLY (spec.verbs.*)
//
// The verb operands are surface-scoped rather than merely conventional: outside
// a verb they are always empty, and `-servername {{ .Player }}` would not fail —
// it would launch with an empty argument and generate a fresh world.
//
// FUNCTIONS — the complete list, seven:
//
//	join sep list       joins a .Content list with a block's separator
//	lower s
//	upper s
//	quote s             Go-style double-quoted and escaped. This is what makes
//	                    Palworld's ServerName="…" correct.
//	default fallback s  fallback when s is empty
//	mib s               "5g" -> 5120, whole MiB; a size that is not a whole
//	                    number of MiB is an error, not a rounded guess
//	plusBytes a b       "8G" + "3G" -> "11G", in ParseBytes/FormatBytes units
//
// There is no `env`. One candidate design listed it; it reads the daemon's
// environment into a game's argv and is a data-exfiltration primitive with no
// legitimate use here.
//
// There is no control flow — no if, range, with, template, call, printf or
// index, and no pipeline into anything off the list. Conditionality lives on the
// arg as a `when:` guard (see when.go), where it is checked at load time and
// shows up in `gvm apply --diff` instead of hiding inside a string.
//
// TAINT. Rendering anything that names .Secret.* returns TaintSecret, and
// RenderedFile.TaintedKeys names the keys that carry it, so the differ, the
// console echo, the dashboard and the audit row mask without re-rendering and
// without having to recognise a secret by looking at its value.

package spec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// Taint marks a rendered value that must not be printed, logged, echoed or
// diffed in the clear.
type Taint uint8

const (
	TaintNone   Taint = 0
	TaintSecret Taint = 1 << 0
)

// Has reports whether every bit of o is set in t.
func (t Taint) Has(o Taint) bool { return t&o == o }

func (t Taint) String() string {
	if t.Has(TaintSecret) {
		return "secret"
	}
	return "none"
}

// Redacted is what a tainted value renders as everywhere a human might see it.
// It is a constant so the diff, the dashboard and the audit row cannot disagree
// about what masking looks like.
const Redacted = "••••••"

// RenderPhase is the phase a template renders in. It is NOT hook.Phase: this is
// the three-valued `.Phase` variable of 8.3, which says whether the process
// being built is the install run, the content-acquisition boot, or a normal
// start — the distinction PZ's install-only `-adminpassword` argument turns on.
type RenderPhase string

const (
	PhaseInstall RenderPhase = "install"
	PhaseContent RenderPhase = "content"
	PhaseNormal  RenderPhase = "normal"
)

// Valid reports whether p is one of the three phases.
func (p RenderPhase) Valid() bool {
	switch p {
	case PhaseInstall, PhaseContent, PhaseNormal:
		return true
	}
	return false
}

// RenderPhases returns the three phases in the order the docs list them.
func RenderPhases() []RenderPhase { return []RenderPhase{PhaseInstall, PhaseContent, PhaseNormal} }

// VarValues, PortValues and SecretValues are the resolved values a manifest's
// templates and `when:` guards see. They are named types because they are passed
// through provider.Ctx and hook.Ctx, where a bare map would invite a caller to
// hand a hook the secrets map by mistake.
type (
	VarValues    map[string]string
	PortValues   map[string]int
	SecretValues map[string]string
)

// ContentValues is one content block's two namespaces. They are separate lists
// because an acquisition id (a workshop id) and an activation id (a mod.info
// id) are different identifiers for the same mod, and conflating them is the
// bug internal/pz exists to prevent.
type ContentValues struct {
	Acquire  []string
	Activate []string
}

// Empty reports whether the block has nothing in either namespace. It is the
// `content_nonempty:` predicate.
func (c ContentValues) Empty() bool { return len(c.Acquire) == 0 && len(c.Activate) == 0 }

// HostValues are the host facts a manifest may use for defaults and sizing.
// Deliberately three fields: a manifest that could read the host's hostname,
// kernel or environment would be a fingerprinting surface with no use case.
type HostValues struct {
	RamMB int
	CPUs  int
	IP4   string
}

// VerbArgs are the operator-supplied operands of one verb invocation. They are
// separate from RenderData because they change per call while everything else
// is bound once per instance.
//
// These values are NOT escaped here: quoting is per-game and belongs to
// internal/control, which refuses any argument containing a character that could
// terminate or extend a console command (§11.10).
type VerbArgs struct {
	Msg      string
	Player   string
	Reason   string
	Duration string
}

// RenderData is the complete variable surface, and the only value a template is
// ever executed against. Field names are the template's spelling exactly.
type RenderData struct {
	Name string
	Game string

	Install string
	Data    string
	Home    string
	Work    string

	User  string
	Group string

	Port    PortValues
	Var     VarValues
	Secret  SecretValues
	Content map[string]ContentValues
	Host    HostValues

	Phase RenderPhase

	Msg      string
	Player   string
	Reason   string
	Duration string
}

// Renderer binds one instance's values and renders the manifest's templated
// fields against them.
//
// A Renderer is immutable: WithPhase and WithVerbArgs return copies. The same
// Renderer is handed to providers and hooks (provider.Ctx.Render, hook.Ctx.Render)
// and a hook that could re-point it at another instance's data would be a
// cross-instance read.
type Renderer struct {
	data RenderData
}

// NewRenderer binds d. The maps are used as given rather than copied: they are
// the resolved values for this instance and the callers that build them
// (instance apply, the runner) do not mutate them afterwards.
func NewRenderer(d RenderData) *Renderer {
	if d.Phase == "" {
		d.Phase = PhaseNormal
	}
	return &Renderer{data: d}
}

// Data returns a copy of the bound values, for the `when:` evaluator and for
// tests. The maps inside are shared; nothing in this package writes to them.
func (r *Renderer) Data() RenderData { return r.data }

// WithPhase returns a copy of r bound to phase.
func (r *Renderer) WithPhase(p RenderPhase) *Renderer {
	c := *r
	c.data.Phase = p
	return &c
}

// WithVerbArgs returns a copy of r carrying one verb invocation's operands.
func (r *Renderer) WithVerbArgs(a VerbArgs) *Renderer {
	c := *r
	c.data.Msg, c.data.Player = a.Msg, a.Player
	c.data.Reason, c.data.Duration = a.Reason, a.Duration
	return &c
}

// Render expands one templated field. field is the document path
// ("spec.runtime.args[3]", "spec.verbs.kick.control"), which selects the surface
// and names the field in any error.
//
// The returned Taint is the caller's obligation, not advice: a TaintSecret value
// must not reach a log line, a diff, the console echo or an audit row in the
// clear.
//
// The guard runs on every call rather than trusting that the manifest was
// checked at load. Parsing a short string is cheap, and a Renderer that would
// execute an unchecked string is a way around the load-time refusal.
func (r *Renderer) Render(field, text string) (string, Taint, error) {
	if !strings.Contains(text, "{{") {
		return text, TaintNone, nil
	}
	t, te := parseGuarded(field, text, SurfaceFor(field))
	if te != nil {
		return "", TaintNone, te
	}
	taint := treeTaint(t.Tree.Root)

	var b strings.Builder
	if err := t.Execute(&b, r.data); err != nil {
		// The partial output is discarded: half of an argv element or half of a
		// config value is worse than none, and text/template's error names the
		// expression rather than the value, so it is safe to surface.
		return "", taint, fmt.Errorf("%s: %w", field, err)
	}
	return b.String(), taint, nil
}

// RenderedFile is one config file's owned keys, expanded.
//
// TaintedKeys is carried rather than recomputed so `gvm apply --diff` can print
// `Password=••••••` without rendering the secret a second time to find out that
// it should not print it.
type RenderedFile struct {
	Name   string // config_files[].name
	Path   string
	Format string
	Owner  string

	Keys        map[string]string
	TaintedKeys []string
	Taint       Taint
}

// IsTainted reports whether key expanded a secret.
func (f RenderedFile) IsTainted(key string) bool {
	for _, k := range f.TaintedKeys {
		if k == key {
			return true
		}
	}
	return false
}

// Display returns the value as a human may see it: the value itself, or Redacted
// when it carries a secret. Every rendered-config view goes through this, so
// "did this one remember to mask?" is not a question anyone has to ask per call
// site.
func (f RenderedFile) Display(key string) string {
	if f.IsTainted(key) {
		return Redacted
	}
	return f.Keys[key]
}

// RenderFile expands a config file's path and every owned key.
//
// It renders every key even after one fails, so `gvm apply` reports all the
// broken keys in one pass instead of one per run.
func (r *Renderer) RenderFile(cf ConfigFile) (RenderedFile, error) {
	base := "spec.config_files." + cf.Name
	out := RenderedFile{
		Name:   cf.Name,
		Format: cf.Format,
		Owner:  cf.Owner,
		Keys:   make(map[string]string, len(cf.Keys)),
	}

	var errs []error
	// Validation refuses `.Secret.*` here (8.6 check 4), but checkTemplates is
	// skipped when the NodeIndex is nil — which is the apply-from-pin case — so
	// the refusal is repeated where the path actually becomes a filename. A
	// masked path is not enough: the password would still be on disk as a name.
	path, ptaint, err := r.Render(base+".path", cf.Path)
	switch {
	case err != nil:
		errs = append(errs, err)
	case ptaint.Has(TaintSecret):
		errs = append(errs, fmt.Errorf("%s.path: a secret may not be rendered into a file path", base))
		path = "" // same rule as Render: no partial output that looks like a value
	}
	out.Path = path

	keys := make([]string, 0, len(cf.Keys))
	for k := range cf.Keys {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v, taint, err := r.Render(base+".keys."+k, cf.Keys[k])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out.Keys[k] = v
		if taint.Has(TaintSecret) {
			out.TaintedKeys = append(out.TaintedKeys, k)
			out.Taint |= TaintSecret
		}
	}
	if len(errs) > 0 {
		return out, joinErrs(errs)
	}
	return out, nil
}

func joinErrs(errs []error) error {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return fmt.Errorf("%s", strings.Join(msgs, "\n"))
}

// templateFuncs is the FuncMap the parser sees. Its key set must equal
// allowedFuncs in render_guard.go — the guard reads its own list, so adding a
// function here without adding it there means the function parses and then is
// refused; adding it there without adding it here means a load-time pass and an
// execute-time failure. TestFuncMapMatchesAllowlist fails on either.
var templateFuncs = template.FuncMap{
	"join":      tmplJoin,
	"lower":     strings.ToLower,
	"upper":     strings.ToUpper,
	"quote":     tmplQuote,
	"default":   tmplDefault,
	"mib":       tmplMiB,
	"plusBytes": tmplPlusBytes,
}

// tmplJoin exists so `.Content.mods.Acquire` can be written into the game's own
// config with the separator that game uses. PZ's is ";" and a comma breaks the
// list with no error at all, which is why the separator is manifest data.
func tmplJoin(sep string, list []string) string { return strings.Join(list, sep) }

// tmplQuote is Go's own quoting, which agrees with what the UE and Java configs
// actually parse: double quotes, backslash escapes, and non-printables escaped
// rather than emitted raw.
func tmplQuote(s string) string { return strconv.Quote(s) }

func tmplDefault(fallback, s string) string {
	if s == "" {
		return fallback
	}
	return s
}

// tmplMiB converts a size to whole MiB, for the flags that take a bare number
// (-Xmx5120m). A size that is not a whole number of MiB is an ERROR rather than
// a rounded guess: rounding a heap size down is invisible in a diff and shows up
// as an OOM kill weeks later.
func tmplMiB(s string) (int64, error) {
	n, err := ParseBytes(s)
	if err != nil {
		return 0, err
	}
	const mib = 1 << 20
	if n%mib != 0 {
		return 0, fmt.Errorf("%w: %q is not a whole number of MiB", ErrBytes, s)
	}
	return n / mib, nil
}

// tmplPlusBytes adds two sizes and re-emits the canonical spelling, so a
// manifest can say "the heap plus this game's native headroom" without the
// result turning into 11811160064 in the operator's diff.
func tmplPlusBytes(a, b string) (string, error) {
	x, err := ParseBytes(a)
	if err != nil {
		return "", err
	}
	y, err := ParseBytes(b)
	if err != nil {
		return "", err
	}
	sum := x + y
	if sum < x {
		return "", fmt.Errorf("%w: %q + %q overflows", ErrBytes, a, b)
	}
	return FormatBytes(sum), nil
}
