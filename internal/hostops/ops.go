package hostops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// UnitKind says which of an instance's units a supervision verb acts on.
//
// It is an enum and not a unit name for the reason the package comment gives:
// hostops derives `gvm@pz-main.service` from the instance name through
// internal/instance, so there is no request shape in which gvm-web asks gvmd
// to start sshd.
type UnitKind string

const (
	// UnitInstance is gvm@<name>.service, the game itself. It is the zero
	// value's meaning, because it is what almost every request wants.
	UnitInstance UnitKind = ""
	// UnitJob is gvm-job@<instance>_<job>.service.
	UnitJob UnitKind = "job"
	// UnitJobTimer is gvm-job@<instance>_<job>.timer. Separate from UnitJob
	// because enabling a job's timer and starting the job itself are
	// different acts (section 7.5).
	UnitJobTimer UnitKind = "job_timer"
)

// UnitScope is what unit.list asks about.
type UnitScope string

const (
	ScopeInstances UnitScope = "instances"
	ScopeJobs      UnitScope = "jobs"
	ScopeJobTimers UnitScope = "job_timers"
)

// patterns is the glob each scope resolves to. A scope enum rather than a
// caller-supplied pattern keeps `*` -- the whole host's unit table -- out of
// the request shape.
var scopePatterns = map[UnitScope]string{
	ScopeInstances: "gvm@*.service",
	ScopeJobs:      "gvm-job@*.service",
	ScopeJobTimers: "gvm-job@*.timer",
}

// Request is one verb invocation, and it is the SAME struct on both paths: the
// root CLI fills it in and calls Do, and phase 15's socket server decodes
// NDJSON into it and calls the same Do. Nothing about a request means
// something different depending on where it arrived from.
//
// Every field is addressing information. There are no paths and no unit names,
// because a request that carried either would let the caller choose what
// "start" starts.
type Request struct {
	Verb  Verb        `json:"verb"`
	Actor audit.Actor `json:"actor"`

	// Instance is the instance the verb acts on, validated by
	// instance.ValidateName before it is used to derive anything.
	Instance string `json:"instance,omitempty"`
	// Job names one of the instance's jobs, for the job unit, its timer and
	// its descriptor (D-13).
	Job string `json:"job,omitempty"`
	// LegacyUnit is the pre-gvm unit an adoption guards against. It is the
	// only operator-typed string in this struct that reaches a path, and
	// validateLegacyUnit is where it is refused.
	LegacyUnit string `json:"legacy_unit,omitempty"`

	Unit  UnitKind  `json:"unit,omitempty"`
	Scope UnitScope `json:"scope,omitempty"`
	File  FileKind  `json:"file,omitempty"`
	Dir   DirKind   `json:"dir,omitempty"`

	// Body is the rendered content of a file.write. It is rendered by
	// internal/reconcile, which is the same build in both processes; hostops
	// deliberately does not render, because rendering here would mean
	// hostops importing the template layer and the unit golden tests moving
	// into the root daemon's dependency graph.
	Body []byte `json:"body,omitempty"`

	// Params is what the CALLER knows and hostops cannot: the reason for a
	// stop, the rung the shutdown ladder ended on, how long a start took to
	// become ready. Accepting a free-form map is safe here precisely because
	// internal/audit filters params through a per-action ALLOWLIST -- an
	// unknown key is dropped and recorded, never written.
	Params audit.Params `json:"params,omitempty"`

	RequestID string `json:"request_id,omitempty"`
}

// Result is what a verb answers with. Only the fields the verb's declaration
// implies are set; the rest are zero.
type Result struct {
	Verb Verb `json:"verb"`

	// Changed says the host is not what it was. It is false for a file.write
	// whose bytes and mode already matched, which is what lets `gvm apply`
	// print "= unchanged" honestly instead of counting every rewrite as work.
	Changed bool `json:"changed"`

	// Path and Unit are what the verb DERIVED. They are returned rather than
	// assumed, so a caller (and an audit row) records the object that was
	// actually touched rather than the one it meant.
	Path string `json:"path,omitempty"`
	Unit string `json:"unit,omitempty"`

	State platform.UnitState   `json:"state,omitzero"`
	Units []platform.UnitState `json:"units,omitempty"`
	Disk  platform.DiskUsage   `json:"disk,omitzero"`
	User  platform.User        `json:"user,omitzero"`
}

// Errors Do returns before any handler runs.
var (
	// ErrBadRequest is a request that cannot be addressed: no instance name
	// where the declaration requires one, no file kind on a file.write. It is
	// separate from a handler's failure because it means the caller is wrong,
	// not the host.
	ErrBadRequest = errors.New("hostops: request does not address anything")

	// ErrNoAudit refuses to construct an Ops with nowhere to record.
	// Section 3.3 promises that a root CLI verb works with Postgres down and
	// "logs a warning if the audit insert fails rather than refusing to act";
	// that promise is about the SINK failing, not about there being no sink.
	ErrNoAudit = errors.New("hostops: no audit logger; every mutating verb must be recordable")
)

// Config is what Ops needs. Each field is a seam, and each has exactly one
// production implementation.
type Config struct {
	Systemd platform.Systemd
	Users   platform.Users
	FS      FS
	Audit   *audit.Logger

	// LoadInstance reads an instance's YAML. It is here because two directory
	// kinds belong to the account the game runs as, and for an ADOPTED
	// instance that account is pzserver rather than gvm-pz-main -- a fact
	// that exists only in the file. Deriving it from the name instead is how
	// an adoption chowns a live world to an account it does not run as.
	LoadInstance func(path string) (*spec.Instance, spec.Diags, error)

	// Log receives this package's own complaints. It is not where audit
	// records go.
	Log *slog.Logger

	// close releases whatever New acquired, and is set only by NewLocal.
	close func() error
}

// Ops is THE implementation of the privileged surface.
//
// It is a struct with one dispatch entry point rather than a set of exported
// methods, because two entry points are two places for a check to be missed:
// the in-process root CLI and the socket server must not be able to reach a
// verb by different routes.
type Ops struct {
	cfg Config
}

// New builds an Ops from explicit seams. Tests use it; NewLocal is production.
func New(cfg Config) (*Ops, error) {
	if cfg.Audit == nil {
		return nil, ErrNoAudit
	}
	if cfg.FS == nil {
		cfg.FS = osFS{}
	}
	if cfg.LoadInstance == nil {
		cfg.LoadInstance = spec.LoadInstance
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Ops{cfg: cfg}, nil
}

// NewLocal builds the production Ops: systemd over its own socket, real unix
// accounts, the real filesystem.
//
// It is what `gvm gvmd` and `sudo gvm` both call. Off Linux every seam refuses
// with platform.ErrNotSupported rather than pretending, so the failure a
// developer sees names the platform instead of arriving as a wrong answer.
func NewLocal(ctx context.Context, lg *audit.Logger) (*Ops, error) {
	if lg == nil {
		return nil, ErrNoAudit
	}
	sd, closeSD, err := newSystemd(ctx)
	if err != nil {
		return nil, err
	}
	us, err := newUsers()
	if err != nil {
		if closeSD != nil {
			_ = closeSD()
		}
		return nil, err
	}
	return New(Config{
		Systemd: sd,
		Users:   us,
		FS:      osFS{},
		Audit:   lg,
		close:   closeSD,
	})
}

// Close releases the systemd connection NewLocal opened.
func (o *Ops) Close() error {
	if o.cfg.close == nil {
		return nil
	}
	return o.cfg.close()
}

// handler runs one verb. It is called only after Do has looked the verb up and
// validated the request against its declaration.
//
// The table below is built from METHOD EXPRESSIONS rather than closures, so
// every entry is a named method on Ops and a typo produces a compile error
// instead of a handler that quietly does the wrong verb's work.
type handler func(*Ops, context.Context, Request) (Result, error)

// handlers is the dispatch table. Its keys must be exactly the keys of the
// declaration table in verbs.go -- verbs_test.go fails the build otherwise,
// and that assertion is the whole security value of the two-daemon split.
var handlers = map[Verb]handler{
	VerbUnitStart:   (*Ops).unitStart,
	VerbUnitStop:    (*Ops).unitStop,
	VerbUnitRestart: (*Ops).unitRestart,
	VerbUnitEnable:  (*Ops).unitEnable,
	VerbUnitDisable: (*Ops).unitDisable,

	VerbDaemonReload: (*Ops).daemonReload,

	VerbUnitState: (*Ops).unitState,
	VerbUnitList:  (*Ops).unitList,
	VerbDiskUsage: (*Ops).diskUsage,

	VerbFileWrite:  (*Ops).fileWrite,
	VerbFileRemove: (*Ops).fileRemove,
	VerbDirEnsure:  (*Ops).dirEnsure,

	VerbUserEnsure: (*Ops).userEnsure,
}

// Do executes one verb. This is the only way into this package.
func (o *Ops) Do(ctx context.Context, req Request) (Result, error) {
	decl, ok := Lookup(req.Verb)
	if !ok {
		// An undeclared verb arriving from gvm-web is a security event and
		// not an operating condition, so it is recorded as a denial rather
		// than returned quietly. The alternative -- a bare error -- leaves
		// the single most interesting thing that can happen to this socket
		// with no trace anywhere.
		//
		// The denial goes to audit AND to journald, because the two copies
		// answer different questions: the audit row is what an investigation
		// reads afterwards, and the log line is what an operator watching a
		// live box sees. journald is the copy that survives /var/lib/gvm
		// being wiped.
		o.cfg.Audit.Denied(ctx, req.Actor, req.Instance, string(req.Verb),
			"hostops.verb", "undeclared")
		o.cfg.Log.LogAttrs(ctx, slog.LevelWarn, "hostops: refused an undeclared verb",
			slog.String("verb", string(req.Verb)),
			slog.String("actor_via", string(req.Actor.Via)))
		return Result{Verb: req.Verb}, fmt.Errorf("%w: %q", ErrUnknownVerb, req.Verb)
	}
	if err := req.validate(decl); err != nil {
		return Result{Verb: req.Verb}, err
	}
	h := handlers[req.Verb]
	if h == nil {
		// Unreachable while verbs_test.go passes. It is here because the
		// consequence of reaching it is a nil dereference inside a root
		// daemon, and "the test covers it" is not the same as "the process
		// survives it". It is logged at error level as well as returned,
		// because a build that reaches this line has a broken dispatch table
		// and the operator's copy of that fact belongs in journald.
		o.cfg.Log.LogAttrs(ctx, slog.LevelError, "hostops: declared verb has no handler",
			slog.String("verb", string(req.Verb)))
		return Result{Verb: req.Verb}, fmt.Errorf("%w: %q is declared with no handler", ErrUnknownVerb, req.Verb)
	}

	if decl.Effect == Observes {
		return h(o, ctx, req)
	}

	var res Result
	err := o.cfg.Audit.Do(ctx, audit.Event{
		Actor:     req.Actor,
		Action:    decl.Audit,
		Instance:  req.Instance,
		RequestID: req.RequestID,
		Params:    req.Params,
	}, func(ctx context.Context) (audit.Params, error) {
		var err error
		res, err = h(o, ctx, req)
		return res.auditParams(decl), err
	})
	return res, err
}

// auditParams is what the verb itself proved, as opposed to what the caller
// asserted. It is merged OVER the caller's params, because these are the
// facts hostops can stand behind.
//
// Only actions.go's allowlist for the declared action survives into the row;
// everything else is dropped and reported. That is the mechanism, not a
// shortcoming: the `actions` key is what audit.InstanceApply accepts, so an
// apply's rows say which step ran, while an instance.stop's row keeps the
// caller's rung and drops a path it has no column for.
func (r Result) auditParams(decl Decl) audit.Params {
	if decl.Audit != audit.InstanceApply {
		return nil
	}
	what := string(r.Verb)
	switch {
	case r.Path != "":
		what += " " + r.Path
	case r.Unit != "":
		what += " " + r.Unit
	}
	if !r.Changed {
		what += " (unchanged)"
	}
	return audit.Params{"actions": []string{what}}
}

// validate checks a request against its declaration before any host call.
//
// It is strict about fields the verb does not read, and that strictness is the
// point rather than pedantry: a dir.ensure carrying a Body would otherwise
// succeed having written nothing, and a daemon.reload carrying a file kind
// would succeed having reloaded a manager that had not read the file the
// caller thought it had just written. Both are silent.
func (r Request) validate(decl Decl) error {
	if decl.Instance {
		if r.Instance == "" {
			return fmt.Errorf("%w: %s needs an instance name", ErrBadRequest, r.Verb)
		}
		if err := instance.ValidateName(r.Instance); err != nil {
			return fmt.Errorf("%w: %w", ErrBadRequest, err)
		}
	} else if r.Instance != "" {
		return fmt.Errorf("%w: %s is host-wide and takes no instance", ErrBadRequest, r.Verb)
	}
	if r.Job != "" {
		if err := instance.ValidateJobName(r.Job); err != nil {
			return fmt.Errorf("%w: %w", ErrBadRequest, err)
		}
	}
	for _, set := range r.argsSet() {
		if !decl.Args.Has(set) {
			return fmt.Errorf("%w: %s does not read %q, so a request that sets it means "+
				"something the verb will not do", ErrBadRequest, r.Verb, argNames[set])
		}
	}
	return nil
}

// argsSet is which addressing fields the request actually carries.
//
// UnitKind's zero value is UnitInstance and is therefore not "set" -- the
// common case is the instance's own unit, and requiring it to be spelled out
// would make every call site noisier for no safety.
func (r Request) argsSet() []Args {
	var out []Args
	if r.Unit != UnitInstance {
		out = append(out, ArgUnit)
	}
	if r.Scope != "" {
		out = append(out, ArgScope)
	}
	if r.File != "" {
		out = append(out, ArgFile)
	}
	if r.Dir != "" {
		out = append(out, ArgDir)
	}
	if len(r.Body) > 0 {
		out = append(out, ArgBody)
	}
	if r.Job != "" {
		out = append(out, ArgJob)
	}
	if r.LegacyUnit != "" {
		out = append(out, ArgLegacy)
	}
	return out
}

// unitName derives the unit a supervision verb acts on.
func (r Request) unitName() (string, error) {
	switch r.Unit {
	case UnitInstance:
		if r.Job != "" {
			return "", fmt.Errorf("%w: %s names job %q but targets the instance unit; set unit=job",
				ErrBadRequest, r.Verb, r.Job)
		}
		return instance.UnitName(r.Instance), nil
	case UnitJob:
		if r.Job == "" {
			return "", fmt.Errorf("%w: %s on a job unit needs a job name", ErrBadRequest, r.Verb)
		}
		return instance.JobUnit(r.Instance, r.Job), nil
	case UnitJobTimer:
		if r.Job == "" {
			return "", fmt.Errorf("%w: %s on a job timer needs a job name", ErrBadRequest, r.Verb)
		}
		return instance.JobTimer(r.Instance, r.Job), nil
	}
	return "", fmt.Errorf("%w: unit kind %q is not one of %q, %q, %q",
		ErrBadRequest, r.Unit, UnitInstance, UnitJob, UnitJobTimer)
}

func (o *Ops) systemd() (platform.Systemd, error) {
	if o.cfg.Systemd == nil {
		return nil, platform.Unsupported("systemd")
	}
	return o.cfg.Systemd, nil
}

// unitAction is the shape every supervision verb shares: derive the unit, then
// call one method on the manager. Writing it once is what stops one of the
// five from deriving its unit differently.
func (o *Ops) unitAction(ctx context.Context, req Request, fn func(context.Context, platform.Systemd, string) error) (Result, error) {
	sd, err := o.systemd()
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	unit, err := req.unitName()
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	res := Result{Verb: req.Verb, Unit: unit}
	if err := fn(ctx, sd, unit); err != nil {
		return res, fmt.Errorf("hostops: %s %s: %w", req.Verb, unit, err)
	}
	res.Changed = true
	return res, nil
}

func (o *Ops) unitStart(ctx context.Context, req Request) (Result, error) {
	return o.unitAction(ctx, req, func(ctx context.Context, sd platform.Systemd, u string) error {
		return sd.Start(ctx, u)
	})
}

func (o *Ops) unitStop(ctx context.Context, req Request) (Result, error) {
	return o.unitAction(ctx, req, func(ctx context.Context, sd platform.Systemd, u string) error {
		return sd.Stop(ctx, u)
	})
}

func (o *Ops) unitRestart(ctx context.Context, req Request) (Result, error) {
	return o.unitAction(ctx, req, func(ctx context.Context, sd platform.Systemd, u string) error {
		return sd.Restart(ctx, u)
	})
}

func (o *Ops) unitEnable(ctx context.Context, req Request) (Result, error) {
	return o.unitAction(ctx, req, func(ctx context.Context, sd platform.Systemd, u string) error {
		return sd.Enable(ctx, u)
	})
}

func (o *Ops) unitDisable(ctx context.Context, req Request) (Result, error) {
	return o.unitAction(ctx, req, func(ctx context.Context, sd platform.Systemd, u string) error {
		return sd.Disable(ctx, u)
	})
}

// daemonReload makes systemd re-read unit files.
//
// It is instance-scoped in the declaration table even though the manager call
// is host-wide, because every reload gvm performs is the tail of one
// instance's apply and the audit row has to say whose.
func (o *Ops) daemonReload(ctx context.Context, req Request) (Result, error) {
	sd, err := o.systemd()
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	if err := sd.DaemonReload(ctx); err != nil {
		return Result{Verb: req.Verb}, fmt.Errorf("hostops: daemon-reload: %w", err)
	}
	return Result{Verb: req.Verb, Changed: true}, nil
}

func (o *Ops) unitState(ctx context.Context, req Request) (Result, error) {
	sd, err := o.systemd()
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	unit, err := req.unitName()
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	st, err := sd.State(ctx, unit)
	if err != nil {
		return Result{Verb: req.Verb, Unit: unit}, fmt.Errorf("hostops: state %s: %w", unit, err)
	}
	return Result{Verb: req.Verb, Unit: unit, State: st}, nil
}

func (o *Ops) unitList(ctx context.Context, req Request) (Result, error) {
	sd, err := o.systemd()
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	pattern, ok := scopePatterns[req.Scope]
	if !ok {
		return Result{Verb: req.Verb}, fmt.Errorf("%w: scope %q is not one of %q, %q, %q",
			ErrBadRequest, req.Scope, ScopeInstances, ScopeJobs, ScopeJobTimers)
	}
	units, err := sd.ListUnits(ctx, pattern)
	if err != nil {
		return Result{Verb: req.Verb}, fmt.Errorf("hostops: list %s: %w", pattern, err)
	}
	return Result{Verb: req.Verb, Units: units}, nil
}

// diskUsage statfs's the filesystem holding one of an instance's directories.
//
// It takes a DirKind rather than a path for the same reason every other verb
// does, and because the answer is about a FILESYSTEM: the caller wants to know
// whether the backup will fit, and "the volume /var/backups/gvm/pz-main is on"
// is the honest way to ask that.
func (o *Ops) diskUsage(_ context.Context, req Request) (Result, error) {
	d, ok := dirKinds[req.Dir]
	if !ok {
		return Result{Verb: req.Verb}, fmt.Errorf("%w: %q", ErrUnknownKind, req.Dir)
	}
	p := d.Path(req.Instance)
	du, err := o.cfg.FS.DiskUsage(p)
	if err != nil {
		return Result{Verb: req.Verb, Path: p}, err
	}
	return Result{Verb: req.Verb, Path: p, Disk: du}, nil
}

// fileTarget builds the addressing struct a file kind's Path uses, refusing a
// request that names a job for a kind that has none and vice versa.
func (r Request) fileTarget(d fileDecl) (fileTarget, error) {
	if d.Job && r.Job == "" {
		return fileTarget{}, fmt.Errorf("%w: file kind %q needs a job name", ErrBadRequest, d.Kind)
	}
	if !d.Job && r.Job != "" {
		return fileTarget{}, fmt.Errorf("%w: file kind %q is not per-job but the request names job %q",
			ErrBadRequest, d.Kind, r.Job)
	}
	if d.Legacy && r.LegacyUnit == "" {
		return fileTarget{}, fmt.Errorf("%w: file kind %q needs the legacy unit it guards against",
			ErrBadRequest, d.Kind)
	}
	if !d.Legacy && r.LegacyUnit != "" {
		return fileTarget{}, fmt.Errorf("%w: file kind %q takes no legacy unit", ErrBadRequest, d.Kind)
	}
	return fileTarget{Instance: r.Instance, Job: r.Job, LegacyUnit: r.LegacyUnit}, nil
}

func (o *Ops) fileWrite(_ context.Context, req Request) (Result, error) {
	d, ok := fileKinds[req.File]
	if !ok {
		return Result{Verb: req.Verb}, fmt.Errorf("%w: %q", ErrUnknownKind, req.File)
	}
	t, err := req.fileTarget(d)
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	p, err := d.Path(t)
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	changed, err := writeFile(o.cfg.FS, d, p, req.Body)
	return Result{Verb: req.Verb, Path: p, Changed: changed}, err
}

// fileRemove deletes one of the files gvm owns.
//
// A file that is already absent is not an error: `gvm apply` removing a timer
// for a job the operator deleted has to be re-runnable, and a reconciler that
// fails on its own successful previous run is a reconciler nobody runs twice.
func (o *Ops) fileRemove(_ context.Context, req Request) (Result, error) {
	d, ok := fileKinds[req.File]
	if !ok {
		return Result{Verb: req.Verb}, fmt.Errorf("%w: %q", ErrUnknownKind, req.File)
	}
	t, err := req.fileTarget(d)
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	p, err := d.Path(t)
	if err != nil {
		return Result{Verb: req.Verb}, err
	}
	if _, err := o.cfg.FS.Stat(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{Verb: req.Verb, Path: p}, nil
		}
		return Result{Verb: req.Verb, Path: p}, err
	}
	if err := o.cfg.FS.Remove(p); err != nil {
		return Result{Verb: req.Verb, Path: p}, err
	}
	return Result{Verb: req.Verb, Path: p, Changed: true}, nil
}

func (o *Ops) dirEnsure(_ context.Context, req Request) (Result, error) {
	d, ok := dirKinds[req.Dir]
	if !ok {
		return Result{Verb: req.Verb}, fmt.Errorf("%w: %q", ErrUnknownKind, req.Dir)
	}
	// The two game directories are located from the instance's own YAML, and
	// the account is read from the same document for the same reason: an
	// adopted pz-main installs in /opt/pzserver and runs as pzserver, and
	// deriving either from the name would act on the wrong thing.
	p := ""
	var in *spec.Instance
	if d.SpecPath != nil || d.OwnedByInstance {
		var err error
		if in, err = o.loadInstanceSpec(req.Instance); err != nil {
			return Result{Verb: req.Verb}, err
		}
	}
	if d.SpecPath != nil {
		if p = d.SpecPath(in); p == "" {
			return Result{Verb: req.Verb}, fmt.Errorf("%w: %s names no %s directory",
				ErrBadRequest, instance.PathsFor(req.Instance).Spec, d.Kind)
		}
	} else {
		p = d.Path(req.Instance)
	}

	owner := ""
	if d.OwnedByInstance {
		if owner = in.Spec.User; owner == "" {
			// Guessing here produces a directory owned by an account the runner
			// is not, which fails later as "cannot write status.json" with
			// nothing anywhere pointing at this decision.
			return Result{Verb: req.Verb, Path: p}, fmt.Errorf("%w: %s names no user",
				ErrBadRequest, instance.PathsFor(req.Instance).Spec)
		}
	}

	if d.CreateOnly {
		// An adopted instance keeps its existing paths verbatim (section 5), so
		// a directory that is already there is left exactly as it is: chowning
		// /opt/pzserver to gvm-pz-main would take a live world's tree away from
		// the process using it. For a managed instance this is invisible,
		// because gvm created the directory in the first place.
		if _, err := o.cfg.FS.Stat(p); err == nil {
			return Result{Verb: req.Verb, Path: p, Changed: false}, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Result{Verb: req.Verb, Path: p}, err
		}
	}

	changed, err := mkdirLeaf(o.cfg.FS, p, d.Mode, owner, d.Group)
	return Result{Verb: req.Verb, Path: p, Changed: changed}, err
}

// loadInstanceSpec reads an instance's own YAML.
//
// Reading it rather than deriving from the name is the whole point: an adopted
// instance runs as pzserver out of /opt/pzserver, and a derived name would
// either fail to resolve or -- worse -- resolve to a managed account and take a
// live world away from the process using it.
func (o *Ops) loadInstanceSpec(name string) (*spec.Instance, error) {
	p := instance.PathsFor(name).Spec
	in, _, err := o.cfg.LoadInstance(p)
	if err != nil {
		return nil, fmt.Errorf("hostops: %s: %w", p, err)
	}
	return in, nil
}

// instanceAccount reads the account the game runs as out of the instance's own
// YAML.
//
// Reading it rather than deriving `gvm-<name>` is the whole point: an adopted
// instance runs as pzserver, a pre-existing account that may own other things
// on the box, and section 6.4 says gvm creates no user and chowns nothing for
// it. A derived name would either fail to resolve or -- worse -- resolve to a
// managed account and take a live world's directories away from the process
// using them.
func (o *Ops) instanceAccount(name string) (string, error) {
	in, err := o.loadInstanceSpec(name)
	if err != nil {
		return "", err
	}
	if in.Spec.User == "" {
		return "", fmt.Errorf("%w: %s names no user", ErrBadRequest, instance.PathsFor(name).Spec)
	}
	return in.Spec.User, nil
}

// userEnsure creates the instance's system account and its own group.
//
// It never modifies an account that exists. That is enforced in user.go rather
// than here, because it has to hold for every caller of the seam and not only
// for the verb.
func (o *Ops) userEnsure(ctx context.Context, req Request) (Result, error) {
	if o.cfg.Users == nil {
		return Result{Verb: req.Verb}, platform.Unsupported("unix accounts")
	}
	name := instance.UserName(req.Instance)
	home := instance.PathsFor(req.Instance).Srv

	before, lookupErr := o.cfg.Users.Lookup(ctx, name)
	u, err := o.cfg.Users.EnsureSystemUser(ctx, name, home)
	if err != nil {
		return Result{Verb: req.Verb}, fmt.Errorf("hostops: ensure account %s: %w", name, err)
	}
	// Changed is derived from whether the account existed BEFORE, not from
	// anything the seam reports, so a future implementation cannot quietly
	// start claiming it modified something.
	return Result{
		Verb:    req.Verb,
		Changed: lookupErr != nil || before.Name == "",
		User:    u,
		Path:    home,
	}, nil
}

// Summary renders the declared surface for `gvm doctor` and for the socket's
// capability listing. An enumerable privileged surface is only useful if
// somebody can read it.
func Summary() string {
	var b strings.Builder
	for _, d := range Declarations() {
		fmt.Fprintf(&b, "%-14s %-9s %-16s %s\n", d.Verb, d.Effect, d.Audit, d.Summary)
	}
	return b.String()
}
