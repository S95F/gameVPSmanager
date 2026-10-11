// Package provider is the install seam: Plan, Apply, Observe, plus a mandatory
// read-only Inspect (section 10).
//
// Three properties are structural here rather than reviewed:
//
// Apply may only write bytes that trace to a Digest returned by Plan. The
// blob store in blobs.go is the chokepoint -- it hands back a path only on a
// digest match -- and Artifact is the declaration that makes "was this byte
// planned" a question with an answer.
//
// A plan reaches Apply only through Authorize. Authorize stamps the plan, and
// the stamp is an unexported field no other package can write, so Apply's
// first line (CheckAuthorized) refuses a plan that skipped the gate. That is
// what makes managed_install:false a property of the system rather than a
// check each provider author has to remember: an adopted install is a real
// world that predates gvm, and a provider that materialised over it would
// destroy an install nobody asked it to touch.
//
// Plan is network-reads-only. It is safe with players online, it is the half
// that decides whether anything needs to stop at all (section 13.5), and it is
// unit-testable against recorded fixtures. Apply is the only half that writes.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Intent is what the caller wants, not how to get it.
//
// It is an alias, not a second type: spec declares it because the manifest
// schema has to validate `validate_on:` against it (D-32), and a provider.Intent
// that had to be converted at the seam would be two enums one `switch` away
// from disagreeing.
type Intent = spec.Intent

const (
	IntentInstall = spec.IntentInstall
	IntentUpdate  = spec.IntentUpdate
	IntentRepair  = spec.IntentRepair
	IntentVerify  = spec.IntentVerify
)

// Intents returns the four intents in pipeline order.
func Intents() []Intent {
	return []Intent{IntentInstall, IntentUpdate, IntentRepair, IntentVerify}
}

func validIntent(in Intent) bool {
	switch in {
	case IntentInstall, IntentUpdate, IntentRepair, IntentVerify:
		return true
	}
	return false
}

// The seam's refusals. Each is a sentinel because the correct response differs:
// an adopted instance needs `gvm instance promote`, an unauthorized plan is a
// gvm bug to report, and an invalid plan is a provider bug.
var (
	// ErrAdopted is the managed_install:false refusal.
	ErrAdopted = errors.New("provider: instance is adopted (managed_install:false)")
	// ErrUnauthorized is a plan that reached Apply without passing Authorize,
	// or one authorized against a different instance's managed_install value.
	ErrUnauthorized = errors.New("provider: plan is not authorized for this instance")
	// ErrInvalidPlan is a plan a provider should never have produced.
	ErrInvalidPlan = errors.New("provider: invalid plan")
	// ErrRefused is a provider declining work it structurally cannot do --
	// `adopted` asked to materialise, `none` handed artifacts to write.
	ErrRefused = errors.New("provider: refused")
)

// Provider is one game's install driver.
//
// Implementations are registered by Kind and are stateless: everything one is
// allowed to see arrives in Ctx, so two instances of the same game cannot
// influence each other through a driver, and a driver cannot be handed a
// handle it then keeps.
type Provider interface {
	Kind() string

	// Inspect is read-only and MUST be safe against a live, running server.
	//
	// Mandatory on every provider: adoption, drift detection and "has someone
	// hand-edited this install" become one call graph rather than three
	// per-game special cases, and every future provider author has to answer
	// the question up front.
	Inspect(ctx context.Context, c *Ctx) (*Inspection, error)

	// Plan performs network READS only. No writes, no side effects, no
	// process spawning outside the sandboxed job slice (D-16). Safe with
	// players online.
	Plan(ctx context.Context, c *Ctx, in Intent) (*Plan, error)

	// Apply executes a plan. Its first statement is
	// p.CheckAuthorized(c.ManagedInstall), and every byte it writes must come
	// from c.Blobs.Get(digest) for a digest present in p.Artifacts.
	Apply(ctx context.Context, c *Ctx, p *Plan, pr Progress) (*Applied, error)

	// Observe reads local disk only. Never the network, never the database.
	// This is what makes "what is installed" answerable with Postgres down.
	Observe(ctx context.Context, c *Ctx) (*Observation, error)
}

// Ctx is everything a provider is allowed to see.
//
// Deliberately not the instance struct: a provider cannot reach RBAC, the
// database, systemd or another instance. Everything in it is either a value or
// a narrow seam, so the blast radius of a driver bug is this instance's install
// tree.
type Ctx struct {
	Name     string
	Manifest *spec.Manifest // the PINNED manifest, never the catalog's (D-17)
	Vars     spec.VarValues
	Ports    spec.PortValues
	Paths    Paths     // all Jailed
	RunAs    UnixIdent // the uid the provider must drop to (D-16)
	Blobs    BlobStore // nil for a provider that materialises nothing
	Log      *slog.Logger
	Render   *spec.Renderer // the closed template surface, pre-bound

	// ManagedInstall is the instance's own flag, not the manifest's.
	//
	// Section 10 does not list it, and it has to be here anyway: an adopted
	// Project Zomboid instance runs the `steamcmd` provider, because the
	// manifest describes the game and the flag describes the install. The
	// nightly cycle still asks that provider to CHECK the remote buildid and
	// then needs it to report "update available, this instance is adopted"
	// instead of planning the download (section 13.5). A provider that cannot
	// see the flag cannot answer that honestly, and Apply could not verify
	// that the plan it was handed was authorized for the instance it is
	// standing in.
	ManagedInstall bool

	// Clock is injectable so Observation.ReadAt is assertable rather than
	// "roughly now". Nil means time.Now.
	Clock func() time.Time
}

// Now is the provider's clock.
func (c *Ctx) Now() time.Time {
	if c == nil || c.Clock == nil {
		return time.Now()
	}
	return c.Clock()
}

// Logger is Ctx.Log, never nil. A provider logs findings on a path where
// returning them would mean inventing a field; a nil check at each of those
// call sites is how one gets forgotten.
func (c *Ctx) Logger() *slog.Logger {
	if c == nil || c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}

// Progress is how a long Apply reports to the job log and the dashboard.
//
// A struct of optional funcs rather than an interface, so the zero value is
// usable and silent: a provider calls pr.Step unconditionally and a caller
// that wants no progress passes nothing. An interface here would be nil at
// every call site that does not care, and the nil check is the thing that gets
// forgotten in the error path.
type Progress struct {
	OnStep  func(step string)
	OnBytes func(done, total int64)
}

// Step reports that a named phase has begun.
func (p Progress) Step(step string) {
	if p.OnStep != nil {
		p.OnStep(step)
	}
}

// Bytes reports download or write progress. total may be 0 when unknown.
func (p Progress) Bytes(done, total int64) {
	if p.OnBytes != nil {
		p.OnBytes(done, total)
	}
}

// Plan is what Apply is permitted to do, and nothing more.
type Plan struct {
	Intent Intent
	// Reason is the human sentence for the diff and the audit row. A
	// destructive plan without one is refused: "destroyed 14.2 GB" with no
	// sentence beside it is an audit row nobody can act on.
	Reason string
	// Artifacts is everything Apply may write. An empty Artifacts is not an
	// error -- it is the normal nightly answer (D-43) and the only answer
	// `adopted` and `none` ever give.
	Artifacts []Artifact
	// Bytes is the pre-flight figure checked against unix.Statfs before
	// anything stops. It may exceed the artifact sizes: steamcmd needs
	// headroom for its own staging.
	Bytes       int64
	Destructive bool
	// Steps are the Describe() lines `gvm apply --diff` prints.
	Steps []string
	// Opaque is provider-private: never rendered, never logged, never
	// persisted. It carries the parsed remote state Plan already paid for so
	// Apply does not fetch it twice.
	Opaque any

	// authorized is written only by Authorize. The field is unexported, so no
	// composite literal in any other package can forge a stamp, which is what
	// makes CheckAuthorized in Apply a guarantee instead of a convention.
	authorized bool
	// authorizedFor is the managed_install value the stamp was granted
	// against. Apply compares it to the instance it is actually standing in,
	// because a plan authorized for a managed instance must not be applied to
	// an adopted one.
	authorizedFor bool
}

// EmptyPlan is a plan that writes nothing and destroys nothing.
func EmptyPlan(in Intent, reason string) *Plan {
	return &Plan{Intent: in, Reason: reason}
}

// Writes reports whether this plan would put bytes on disk.
func (p *Plan) Writes() bool { return p != nil && len(p.Artifacts) > 0 }

// Authorize is the single choke point for managed_install:false.
//
// It lives on the PLAN, not inside each provider, so a future provider cannot
// forget it, and it stamps the plan so that a caller cannot forget it either:
// Apply refuses an unstamped plan. Validate runs here too, which means the
// stamp says both "allowed" and "well-formed".
//
// What it cannot defend against is a caller that passes the wrong flag. That
// is one call site -- the apply pipeline reading inst.Spec.ManagedInstall --
// and Apply's own check that the stamp matches Ctx.ManagedInstall closes the
// case where a plan is authorized for one instance and applied to another.
func (p *Plan) Authorize(managedInstall bool) error {
	if p == nil {
		return fmt.Errorf("%w: there is no plan to authorize", ErrInvalidPlan)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if !managedInstall {
		if p.Destructive {
			return fmt.Errorf("%w: the %s plan is destructive; run `gvm instance promote` "+
				"first if gvm really should own these files", ErrAdopted, p.Intent)
		}
		// Writing bytes into an adopted tree is the destructive act whether or
		// not the provider labelled it one: those files predate gvm, they may
		// be hand-patched, and `gvm install` already refuses outright for the
		// same reason. A provider that mislabels an overwrite as harmless
		// cannot get past this.
		if p.Writes() {
			return fmt.Errorf("%w: the %s plan would write %d artifact(s) into an install "+
				"gvm does not own; run `gvm instance promote` first", ErrAdopted, p.Intent,
				len(p.Artifacts))
		}
	}
	p.authorized = true
	p.authorizedFor = managedInstall
	return nil
}

// Authorized reports whether this plan passed Authorize.
func (p *Plan) Authorized() bool { return p != nil && p.authorized }

// CheckAuthorized is the first statement of every Apply.
//
// managedInstall is Ctx.ManagedInstall -- the instance Apply is standing in,
// not the one the plan was built for. They are the same in every correct
// pipeline and the comparison costs nothing, which is the right price for
// "this plan was cleared against some other world".
func (p *Plan) CheckAuthorized(managedInstall bool) error {
	switch {
	case p == nil:
		return fmt.Errorf("%w: there is no plan", ErrUnauthorized)
	case !p.authorized:
		return fmt.Errorf("%w: the %s plan never passed Plan.Authorize, so nothing has "+
			"checked it against managed_install; this is a gvm bug, not an operator error",
			ErrUnauthorized, p.Intent)
	case p.authorizedFor != managedInstall:
		return fmt.Errorf("%w: the %s plan was authorized for managed_install:%t and Apply is "+
			"running against managed_install:%t", ErrUnauthorized, p.Intent,
			p.authorizedFor, managedInstall)
	}
	return nil
}

// Validate is what a provider must be able to pass before its plan is allowed
// to exist. It is called by Authorize; a provider may call it earlier.
func (p *Plan) Validate() error {
	if p == nil {
		return fmt.Errorf("%w: nil plan", ErrInvalidPlan)
	}
	var errs []error
	if !validIntent(p.Intent) {
		errs = append(errs, fmt.Errorf("%w: %q is not an intent", ErrInvalidPlan, string(p.Intent)))
	}
	if p.Bytes < 0 {
		errs = append(errs, fmt.Errorf("%w: Bytes is %d", ErrInvalidPlan, p.Bytes))
	}
	if p.Destructive && strings.TrimSpace(p.Reason) == "" {
		errs = append(errs, fmt.Errorf("%w: a destructive plan must carry a Reason; it is the "+
			"sentence the audit row and the confirmation prompt are made of", ErrInvalidPlan))
	}
	seen := make(map[string]int, len(p.Artifacts))
	for i, a := range p.Artifacts {
		if err := a.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("artifact[%d]: %w", i, err))
			continue
		}
		key := filepath.ToSlash(filepath.Clean(a.DestRel))
		if first, dup := seen[key]; dup {
			// Two artifacts writing one path makes the installed bytes depend
			// on apply order, which means the digest that was approved is not
			// necessarily the digest that ends up on disk.
			errs = append(errs, fmt.Errorf("%w: artifact[%d] and artifact[%d] both write %s",
				ErrInvalidPlan, first, i, key))
			continue
		}
		seen[key] = i
	}
	return errors.Join(errs...)
}

// Artifact is one thing Apply is permitted to write.
type Artifact struct {
	// Digest identifies the bytes: "sha256:…" | "sha1:…" for anything gvm
	// downloads itself and hands to the blob store, or an identity the store
	// cannot verify ("buildid:380870:18452901") for bytes a third-party
	// installer writes directly. ParseDigest tells the two apart and the blob
	// store refuses the second kind outright, so an unverifiable digest can
	// never be mistaken for a verified one.
	Digest Digest
	Size   int64
	Source string // https URL, or "steamcmd:app_update 380870"
	// DestRel is relative to Paths.Install. "." names the install tree itself,
	// which is what a provider whose installer writes its own bytes declares.
	DestRel string
}

// Validate refuses an artifact that could write outside the install tree.
//
// This is zip-slip's front door and it is closed twice: here, lexically, at
// plan time, where the operator can still see the refusal; and again in
// Paths.Dest, which resolves symlinks and re-checks containment against the
// real filesystem.
func (a Artifact) Validate() error {
	if _, err := ParseDigest(a.Digest); err != nil {
		// Wrapped twice so the caller can ask either question: the plan is
		// invalid, AND the specific reason is the digest. A pipeline deciding
		// whether to retry asks Terminal; an operator reads the sentence.
		return fmt.Errorf("%w: %w", ErrInvalidPlan, err)
	}
	if a.Size < 0 {
		return fmt.Errorf("%w: size %d", ErrInvalidPlan, a.Size)
	}
	if strings.TrimSpace(a.Source) == "" {
		return fmt.Errorf("%w: no source; an artifact nobody can say the origin of is not "+
			"reviewable", ErrInvalidPlan)
	}
	return validDestRel(a.DestRel)
}

// validDestRel judges a destination path lexically.
//
// It splits on both separators on every platform for the reason
// instance.hasDotDot does: a rule that behaves differently on the development
// machine than on the host is not a rule that has been tested.
func validDestRel(rel string) error {
	if rel == "" {
		return fmt.Errorf("%w: empty destination", ErrInvalidPlan)
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) ||
		filepath.VolumeName(rel) != "" {
		return fmt.Errorf("%w: %q is absolute, and a destination is relative to the install "+
			"tree", ErrInvalidPlan, rel)
	}
	for _, el := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if el == ".." {
			return fmt.Errorf("%w: %q contains a \"..\" element", ErrInvalidPlan, rel)
		}
	}
	return nil
}

// Applied is what Apply actually did, for the job record and the audit row.
type Applied struct {
	Intent  Intent
	Version Version
	// Changed false is the honest "nothing on disk moved": state `skipped`,
	// which is a first-class success and must not page anyone (D-43).
	Changed bool
	// Wrote names the artifacts that landed, by DestRel.
	Wrote []string
	Bytes int64
	// Steps is what ran, in order. Separate from Plan.Steps, which is what was
	// going to run: an update that retried twice should say so.
	Steps              []string
	StartedAt, EndedAt time.Time
	// Note is the sentence the CLI prints. For a no-op it is the whole point:
	// "pz-main is adopted; the binary update was not performed".
	Note string
}

// ApplyEmptyPlan is Apply for a provider that materialises nothing.
//
// Shared rather than copied into `adopted` and `none` so there is one place
// where the authorization check happens before anything else, and so a third
// such provider inherits it instead of re-deriving it. refusal is the sentence
// explaining why this provider cannot write, used when a caller hands it a
// plan it could not have produced.
func ApplyEmptyPlan(c *Ctx, p *Plan, refusal string) (*Applied, error) {
	managed := c != nil && c.ManagedInstall
	if err := p.CheckAuthorized(managed); err != nil {
		return nil, err
	}
	if p.Writes() || p.Destructive {
		return nil, fmt.Errorf("%w: %s", ErrRefused, refusal)
	}
	now := c.Now()
	return &Applied{
		Intent:    p.Intent,
		Changed:   false,
		StartedAt: now,
		EndedAt:   now,
		Note:      p.Reason,
	}, nil
}

// Inspection is what Inspect found on disk, without judgement.
type Inspection struct {
	Present  bool
	Version  Version
	Sentinel SentinelResult
	// Integrity is D-32's compensating check: the thing that pays for removing
	// the nightly `validate`.
	Integrity IntegrityResult
	// HandEdits are files differing from the provider's own record of what it
	// wrote. A provider with no such record reports none and says so in
	// Integrity rather than guessing.
	HandEdits []string
	// Unmanaged are files the provider did not put there.
	Unmanaged []string
	// Fingerprint is the world's shape (D-34), nil when it was not taken.
	//
	// A pointer, against section 10's spelling, because a zero Fingerprint
	// value reads as "zero files, zero bytes" -- which is exactly the
	// catastrophic finding the fingerprint exists to detect. "Not measured"
	// and "the world is empty" must not be the same value.
	Fingerprint *archive.Fingerprint
}

// SentinelResult is the manifest-declared "is this installed" check (D-32
// step 5): not "is the install directory non-empty", because a directory
// holding 40 GB of a 65 GB download is neither empty nor installed, and the
// difference is a server that starts and immediately dies.
//
// A record, not a checker: the provider fills it from its own filesystem seam
// and OK turns it into a verdict. internal/cli has a pre-provider stopgap of
// the same shape (cli.sentinelState); it should route through Inspect once a
// driver exists for every kind, rather than become a third copy of the rule.
type SentinelResult struct {
	// Declared false means the manifest names no sentinel, so the answer is
	// "cannot tell" -- never "installed".
	Declared bool
	Rel      string // the declared path, relative to Install
	Present  bool
	IsDir    bool
	Size     int64
	// Executable is whether the file carries any execute bit, and
	// WantExecutable whether the manifest requires one. Both are recorded so
	// the message can say which half failed.
	Executable     bool
	WantExecutable bool
	MinBytes       int64
}

// OK reports the verdict, or why there is not one.
func (s SentinelResult) OK() error {
	switch {
	case !s.Declared:
		return errors.New("this manifest declares no install sentinel, so gvm cannot tell " +
			"whether the files are there")
	case !s.Present:
		return fmt.Errorf("%s is absent", s.Rel)
	case s.IsDir:
		return fmt.Errorf("%s is a directory, and the sentinel names a file", s.Rel)
	case s.MinBytes > 0 && s.Size < s.MinBytes:
		return fmt.Errorf("%s is %s, under the manifest's %s floor: a partial download",
			s.Rel, spec.FormatBytes(s.Size), spec.FormatBytes(s.MinBytes))
	case s.WantExecutable && !s.Executable:
		return fmt.Errorf("%s is present but not executable", s.Rel)
	}
	return nil
}

// IntegrityResult is one provider's integrity finding (D-32).
//
// Problems are sentences rather than codes because this is read by an operator
// at 2am, and Facts is a named map rather than a union struct because the three
// providers measure three different things -- steamcmd's StateFlags and byte
// pairs, minecraft's jar manifest -- and a struct holding all of them would
// carry every provider's fields into every row.
type IntegrityResult struct {
	// Checked false means this provider performs no integrity check. Reason
	// says why, so "no problems found" and "nobody looked" are never the same
	// value.
	Checked  bool
	Reason   string
	Problems []string
	Facts    map[string]string
}

// Failed reports whether the check ran and found something.
func (r IntegrityResult) Failed() bool { return r.Checked && len(r.Problems) > 0 }

// Observation is the cheap, disk-only, every-30s version of Inspection.
type Observation struct {
	Version Version
	Healthy bool
	Detail  string
	ReadAt  time.Time
}

// Version is what is installed, as the provider understands versions.
type Version struct {
	ID    string // "18452901" | "26.2.0.76"
	Label string
	At    time.Time
}

// Known reports whether the provider could name a version at all.
func (v Version) Known() bool { return v.ID != "" }

func (v Version) String() string {
	switch {
	case v.ID == "":
		return "unknown"
	case v.Label == "":
		return v.ID
	}
	return v.Label + " (" + v.ID + ")"
}

var (
	registryMu sync.RWMutex
	providers  = map[string]Provider{}
)

// Register adds a driver to the registry.
//
// It panics on a duplicate, because two drivers answering to one
// `provider.kind:` would make the winner depend on package initialisation
// order. It also panics on a kind internal/spec does not call
// SupportImplemented: a driver registered under a name no manifest can name is
// unreachable code that looks like a feature, and a driver registered under a
// name the schema calls planned is a promise the loader will not keep.
func Register(p Provider) {
	kind := p.Kind()
	if spec.ProviderKinds()[kind] != spec.SupportImplemented {
		panic(fmt.Sprintf("provider: %q is not a provider kind internal/spec implements; "+
			"add it to providerKinds first, or a manifest could never name this driver", kind))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := providers[kind]; dup {
		panic("provider: duplicate registration: " + kind)
	}
	providers[kind] = p
}

// Lookup returns the driver registered for a manifest's provider.kind.
func Lookup(kind string) (Provider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := providers[kind]
	return p, ok
}

// Kinds lists every registered kind, sorted.
//
// plan_authorize_test.go walks this list and asserts the adopted-install
// refusal against each entry, so adding a driver adds a test subject rather
// than an exemption.
func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(providers))
	for k := range providers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
