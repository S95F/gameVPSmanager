// `gvm create` — write down what a server IS, and nothing else.
//
// The whole of section 6.1 is that /etc/gvm/instances/<name>.yaml is the
// definition of one server, so this command's job is to produce a correct one
// and then stop. It creates no account, no directory, no unit and no game
// files: those are `gvm apply` and `gvm install`, in that order, and the split
// is what makes the file reviewable before anything on the box changes.
//
// The two decisions it does make are the ones that cannot be made later.
// metadata.uid is a UUIDv7 generated once and never again — grants hang
// off it, so a second identity for the same server is a silent loss of every
// permission row. And the port set is MATERIALISED rather than recomputed
//: a number written into a file is stable, greppable and immune
// to an allocator change moving a live server on the next apply.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/ports"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// createOptions is one `gvm create` invocation, fully parsed.
type createOptions struct {
	Name string
	Game string
	Vars map[string]string
	// VarOrder keeps the order they were typed, so an error names the first
	// one that was wrong rather than a random one.
	VarOrder []string

	User  string
	Paths spec.Paths

	Enabled bool
	Pins    []ports.Pin

	Unapproved bool
	Remote     string
	DryRun     bool
}

// keyValues collects a repeatable KEY=VALUE flag.
type keyValues struct {
	vals  map[string]string
	order *[]string
}

func (k keyValues) String() string { return "" }

func (k keyValues) Set(s string) error {
	key, val, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("want KEY=VALUE, got %q", s)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty key in %q", s)
	}
	if _, dup := k.vals[key]; dup {
		// Last-wins would be a silent typo: two --var public_name= flags in
		// one command line is somebody editing a long invocation and losing
		// track, and the value they meant is not knowable from here.
		return fmt.Errorf("%s given twice", key)
	}
	k.vals[key] = val
	*k.order = append(*k.order, key)
	return nil
}

// portPins collects --pin-port NAME=PORT.
type portPins struct{ pins *[]ports.Pin }

func (p portPins) String() string { return "" }

func (p portPins) Set(s string) error {
	name, num, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("want NAME=PORT, got %q", s)
	}
	var port int
	if _, err := fmt.Sscanf(strings.TrimSpace(num), "%d", &port); err != nil {
		return fmt.Errorf("%q is not a port number", num)
	}
	if port < 1 || port > ports.MaxPort {
		return fmt.Errorf("port %d is outside 1-%d", port, ports.MaxPort)
	}
	*p.pins = append(*p.pins, ports.Pin{Name: strings.TrimSpace(name), Port: port})
	return nil
}

func parseCreateArgs(env *Env, args []string) (createOptions, error) {
	opt := createOptions{Vars: map[string]string{}, Enabled: true}

	var dryAlias, noEnabled bool
	fs := flag.NewFlagSet("gvm create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm create <instance> --game <game-id> [--var KEY=VALUE]...\n\n"+
			"Writes /etc/gvm/instances/<instance>.yaml and nothing else.\n"+
			"Run `gvm apply <instance>` next to put it on the host.\n\n")
		// --user and --dry are hidden: --user only ever names the managed
		// account (any other value is redirected to `gvm adopt`), and --dry is
		// the alias of --dry-run kept for muscle memory.
		flagUsage(env.Stderr, fs, "user", "dry")
		printExamples(env.Stderr,
			`sudo gvm create valheim-1 --game valheim --var public_name="Friends"`,
			`sudo gvm create pz-main --game projectzomboid --pin-port game=16261`)
	}
	fs.StringVar(&opt.Game, "game", "", "game id from `gvm game list` (required)")
	fs.Var(keyValues{vals: opt.Vars, order: &opt.VarOrder}, "var", "set a manifest variable, KEY=VALUE (repeatable)")
	fs.Var(portPins{pins: &opt.Pins}, "pin-port", "fix one port at a number the allocator may not move, NAME=PORT (repeatable)")
	fs.StringVar(&opt.User, "user", "", "unix account to run as (default: gvm-<instance>)")
	fs.StringVar(&opt.Paths.Install, "install", "", "install directory (default: /srv/gvm/<instance>/install)")
	fs.StringVar(&opt.Paths.Data, "data", "", "world/save directory (default: /srv/gvm/<instance>/data)")
	fs.StringVar(&opt.Paths.Home, "home", "", "the GAME's HOME (default: /srv/gvm/<instance>)")
	fs.BoolVar(&opt.Enabled, "enabled", true, "start at boot")
	fs.BoolVar(&noEnabled, "no-enabled", false, "do not start at boot (the off switch for --enabled)")
	fs.BoolVar(&opt.Unapproved, "unapproved-override", false,
		"use an /etc/gvm/games override that has no recorded sha256 approval")
	fs.BoolVar(&opt.DryRun, "dry-run", false, "print the YAML that would be written and write nothing")
	fs.BoolVar(&dryAlias, "dry", false, "alias for --dry-run")
	fs.StringVar(&opt.Remote, "remote", "", "dashboard URL to run this against instead of the local host")

	pos, err := parseWithInstance(fs, args)
	if err != nil {
		return createOptions{}, flagErr(err)
	}
	// --dry is --dry-run; --no-enabled is the off switch for --enabled. Fold
	// the aliases into the one option each downstream code reads.
	opt.DryRun = opt.DryRun || dryAlias
	if noEnabled {
		opt.Enabled = false
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return createOptions{}, err
	}
	opt.Name = name
	if opt.Game == "" {
		fs.Usage()
		return createOptions{}, errors.New("--game is required; `gvm game list` shows what this box can run")
	}
	if !spec.ValidGameID(opt.Game) {
		return createOptions{}, fmt.Errorf("%q is not a game id", opt.Game)
	}
	// Every instance `gvm create` writes is MANAGED, and a managed instance
	// runs as gvm-<name>: reconcile derives the account from the name, and so
	// does hostops. Honouring --user here would write a spec.user that the
	// account step never creates, the directory chowns then aim at, and the
	// drop-in's User= then names -- an apply that dies part-way through with
	// half an instance on the host. A pre-existing account is the ADOPTED case,
	// which is a different command and a different flag on the YAML.
	if opt.User != "" && opt.User != instance.UserName(opt.Name) {
		return createOptions{}, fmt.Errorf("--user %s: every instance `gvm create` writes is "+
			"managed and runs as %s. An instance that must run as an account you already have is "+
			"an adoption (managed_install: false); use `gvm adopt`",
			opt.User, instance.UserName(opt.Name))
	}
	return opt, nil
}

// createInput is everything buildInstance needs, and it is all data.
//
// The function below touches nothing: it is handed the manifest, the occupancy
// and the clock, so the whole of "what does `gvm create valheim-1` write" is
// answerable in a test on any platform, which is where the interesting bugs in
// this command are.
type createInput struct {
	Opt      createOptions
	Manifest *spec.Manifest
	Occ      *ports.Occupancy
	Now      time.Time
	UID      spec.UID
}

// buildInstance assembles the instance document. Pure.
func buildInstance(in createInput) (*spec.Instance, ports.Allocation, error) {
	opt, m := in.Opt, in.Manifest

	vars, err := resolveVars(m, opt)
	if err != nil {
		return nil, nil, err
	}

	reqs, err := m.Spec.PortRequirements()
	if err != nil {
		return nil, nil, err
	}
	alloc, err := ports.Allocate(reqs, m.Spec.PortStride, in.Occ, opt.Name, opt.Pins)
	if err != nil {
		return nil, nil, err
	}

	res, err := instance.ComputeResources(m, vars)
	if err != nil {
		return nil, nil, err
	}

	paths := instance.DefaultSpecPaths(opt.Name)
	if opt.Paths.Install != "" {
		paths.Install = opt.Paths.Install
	}
	if opt.Paths.Data != "" {
		paths.Data = opt.Paths.Data
	}
	if opt.Paths.Home != "" {
		paths.Home = opt.Paths.Home
	}

	// parseCreateArgs has already refused any --user that is not this, so the
	// two spellings cannot diverge; the account is derived here rather than
	// read back so that buildInstance stays answerable from its inputs alone.
	account := instance.UserName(opt.Name)
	// A managed instance's primary group IS its own account. There is no
	// --group flag, and there was never a value for one to carry: an instance
	// whose group is somebody else's is an adoption.
	group := account

	inst := &spec.Instance{
		APIVersion: spec.APIVersionV1,
		Kind:       "Instance",
		Metadata: spec.InstanceMeta{
			Name:    opt.Name,
			UID:     in.UID,
			Created: in.Now.UTC().Truncate(time.Second),
		},
		Spec: spec.InstanceSpec{
			Game:     m.Metadata.ID,
			Manifest: spec.PinOf(m),
			Enabled:  opt.Enabled,
			// A created instance is always managed: managed_install:false is
			// the ADOPTION flag, and it means "these files predate gvm and
			// gvm must not chown them". Nothing `gvm create` writes predates
			// anything, so setting it here would tell the provider to refuse
			// the very install the operator is about to ask for.
			ManagedInstall: true,
			User:           account,
			Group:          group,
			Paths:          paths,
			Ports:          map[string]int(alloc),
			Vars:           vars,
			Resources:      res,
			Jobs:           recommendedJobs(m),
			Backup: spec.BackupPolicy{
				Keep: backupKeep(m),
				Dir:  instance.PathsFor(opt.Name).Backups,
			},
		},
	}
	return inst, alloc, nil
}

// resolveVars merges the manifest's declared defaults with what was typed.
//
// An UNDECLARED --var is refused rather than carried. The manifest's var block
// is the closed set the templates, the dashboard form and the validators all
// read; a value nothing declares would be written into the YAML, rendered by
// nothing, and quietly do nothing forever -- which is worse than an error,
// because the operator believes they configured something.
func resolveVars(m *spec.Manifest, opt createOptions) (map[string]string, error) {
	for _, k := range opt.VarOrder {
		if _, ok := m.Spec.Vars[k]; !ok {
			return nil, fmt.Errorf("%s declares no variable %q; `gvm game validate %s` lists them",
				m.Metadata.ID, k, m.Metadata.ID)
		}
	}
	out := make(map[string]string, len(m.Spec.Vars))
	var missing []string
	for name, vs := range m.Spec.Vars {
		if v, ok := opt.Vars[name]; ok {
			out[name] = v
			continue
		}
		if vs.Default != "" {
			out[name] = vs.Default
			continue
		}
		if vs.Required {
			missing = append(missing, name)
			continue
		}
		// A declared, unrequired var with no default is written empty rather
		// than omitted: the file is the form, and a field that is absent
		// looks like a field the manifest does not have.
		out[name] = ""
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		var b strings.Builder
		for _, name := range missing {
			fmt.Fprintf(&b, "\n  --var %s=…", name)
			if h := m.Spec.Vars[name].Help; h != "" {
				fmt.Fprintf(&b, "   %s", strings.TrimSpace(h))
			}
		}
		return nil, fmt.Errorf("%s requires %d variable(s) with no default:%s",
			m.Metadata.ID, len(missing), b.String())
	}
	return out, nil
}

// recommendedJobs turns the manifest's recommended_jobs into real schedules.
//
// The manifest gives an "HH:MM" and gvm renders a systemd OnCalendar from it.
// No timezone is appended: an OnCalendar with no zone is the host's, which is
// the one an operator setting `04:00` means, and inventing a zone here would
// make the same manifest fire at different local times on two boxes.
func recommendedJobs(m *spec.Manifest) map[string]spec.Job {
	if len(m.Spec.Jobs) == 0 {
		return nil
	}
	out := make(map[string]spec.Job, len(m.Spec.Jobs))
	for name, rj := range m.Spec.Jobs {
		action := rj.Action
		if action == "" {
			action = name
		}
		out[name] = spec.Job{
			Schedule:        "*-*-* " + rj.At + ":00",
			Action:          action,
			RandomizedDelay: rj.Jitter,
			Persistent:      rj.Persistent,
		}
	}
	return out
}

// backupKeep is the retention an instance is created with.
//
// A manifest may recommend more than one job that declares a keep -- a nightly
// and a weekly, with different depths -- but an instance carries ONE
// spec.backup.keep, so this has to pick. It picks in sorted NAME order rather
// than map order, because two `gvm create` runs against the same manifest must
// write the same YAML, and ranging a Go map is randomized. Preferring a job
// whose resolved action is actually "backup" is the sensible rule but not the
// load-bearing part: two backup jobs would still tie, and the sorted walk is
// what breaks the tie the same way every time.
func backupKeep(m *spec.Manifest) int {
	names := make([]string, 0, len(m.Spec.Jobs))
	for name := range m.Spec.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)

	fallback := 0
	for _, name := range names {
		rj := m.Spec.Jobs[name]
		if rj.Keep <= 0 {
			continue
		}
		// The action defaults to the job's own name, the same defaulting
		// recommendedJobs does.
		action := rj.Action
		if action == "" {
			action = name
		}
		if action == "backup" {
			return rj.Keep
		}
		if fallback == 0 {
			fallback = rj.Keep
		}
	}
	if fallback > 0 {
		return fallback
	}
	return 7
}

func runCreate(ctx context.Context, env *Env, args []string) error {
	opt, err := parseCreateArgs(env, args)
	if err != nil {
		return err
	}
	t, err := Resolve(env, "create", opt.Remote)
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}
	h, err := env.host(ctx)
	if err != nil {
		return err
	}
	return createExec(ctx, env, h, t, opt)
}

// createExec is the half that touches the host.
//
// The ordering is section 6.7's and every step of it earns its place: build
// occupancy from all five sources, allocate, RESERVE in the ledger, write the
// YAML, release. The reserve is what stops two concurrent creates both taking
// 16261, and it is useless if the YAML is written before it -- so the release
// is deferred and the write sits between them.
func createExec(ctx context.Context, env *Env, h *HostEnv, t Transport, opt createOptions) error {
	specPath := instance.PathsFor(opt.Name).Spec
	if _, err := h.files().Stat(specPath); err == nil {
		return fmt.Errorf("%s already exists; `gvm create` never overwrites a server definition. "+
			"Edit the file and run `gvm apply %s`, or pick another name", specPath, opt.Name)
	}

	m, diags, err := h.catalog().Load(opt.Game, overrideAuth(opt.Unapproved))
	if err != nil {
		return err
	}
	if diags.HasErrors() {
		return fmt.Errorf("%s does not validate:\n%s", opt.Game, diags.Errors())
	}

	occ, notes, err := buildOccupancy(ctx, h, opt.Name)
	if err != nil {
		return err
	}

	inst, alloc, err := buildInstance(createInput{
		Opt: opt, Manifest: m, Occ: occ, Now: h.now(), UID: spec.NewUID(),
	})
	if err != nil {
		return err
	}

	// Validation runs on the ASSEMBLED document rather than on the flags,
	// because a var that is legal on its own can be illegal next to another
	// one -- Valheim's join password may not be a substring of the server
	// name, and neither half of that rule is checkable from a flag.
	vd := spec.ValidateInstance(inst, m, spec.ValidatorInput{
		Instance: opt.Name,
		Vars:     spec.VarValues(inst.Spec.Vars),
		Ports:    spec.PortValues(inst.Spec.Ports),
	}, nil)
	if vd.HasErrors() {
		return fmt.Errorf("the instance this would write does not validate:\n%s", vd.Errors())
	}

	body, err := spec.MarshalYAML(inst)
	if err != nil {
		return err
	}

	if opt.DryRun {
		fmt.Fprintf(env.Stdout, "# %s — not written (--dry-run)\n%s", specPath, body)
		return nil
	}

	// The reservation window. Release is deferred rather than called on the
	// success path only: a create that fails after reserving must not leave
	// ports blocked for the ledger's whole TTL.
	if h.Ledger != nil {
		reqs, err := m.Spec.PortRequirements()
		if err != nil {
			return err
		}
		if _, err := h.Ledger.Reserve(opt.Name, alloc, reqs); err != nil {
			return fmt.Errorf("reserving ports: %w", err)
		}
		defer func() {
			if _, err := h.Ledger.Release(opt.Name); err != nil {
				fmt.Fprintf(env.Stderr, "gvm create: the port reservation for %s could not be "+
					"released and will expire on its own: %v\n", opt.Name, err)
			}
		}()
	}

	if _, err := h.Ops.Do(ctx, hostops.Request{
		Verb:     hostops.VerbFileWrite,
		Actor:    actorFor(t),
		Instance: opt.Name,
		File:     hostops.FileInstanceSpec,
		Body:     body,
	}); err != nil {
		return err
	}

	printCreated(env.Stdout, inst, m, occ, notes)
	return nil
}

// printCreated says what was written, what was NOT, and what to do next.
//
// The "not" half matters more than it looks: an operator who runs create and
// then `systemctl start gvm@valheim-1` gets a unit that does not exist, and
// the reason is that create deliberately did not make one. Saying so here is
// cheaper than the support thread.
func printCreated(w io.Writer, inst *spec.Instance, m *spec.Manifest, occ *ports.Occupancy, notes []string) {
	p := instance.PathsFor(inst.Metadata.Name)
	fmt.Fprintf(w, "\nwrote %s\n", p.Spec)
	fmt.Fprintf(w, "  game      %s rev %d (%s), pinned\n",
		m.Metadata.ID, m.Metadata.Revision, m.Origin)
	fmt.Fprintf(w, "  uid       %s\n", inst.Metadata.UID)
	fmt.Fprintf(w, "  user      %s\n", inst.Spec.User)

	names := make([]string, 0, len(inst.Spec.Ports))
	for k := range inst.Spec.Ports {
		names = append(names, k)
	}
	sort.Strings(names)
	for i, n := range names {
		label := "  ports    "
		if i > 0 {
			label = "           "
		}
		fmt.Fprintf(w, "%s %-8s %d\n", label, n, inst.Spec.Ports[n])
	}
	if occ != nil && occ.EphemeralAssumed() {
		fmt.Fprintf(w, "           the kernel's ip_local_port_range could not be read; "+
			"%d-%d was ASSUMED\n", occ.Ephemeral().Low, occ.Ephemeral().High)
	}
	for _, n := range notes {
		fmt.Fprintf(w, "           %s\n", n)
	}

	if refs, err := spec.ScanManifestRefs(m); err == nil && len(refs.Secrets) > 0 {
		fmt.Fprintf(w, "  secrets   %s — not generated yet; the unit starts without them "+
			"and the game gets an empty value\n", strings.Join(refs.Secrets, ", "))
	}

	fmt.Fprintf(w, "\nnothing else on this host changed: no account, no directories, no unit.\n")
	fmt.Fprintf(w, "next: gvm apply %s --diff\n", inst.Metadata.Name)
}
