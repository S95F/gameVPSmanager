package lifecycle

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
)

// TestTheVerbSetIsClosed reads the Verb constants out of verbs.go's own SOURCE
// and compares them against the declaration table.
//
// The AST leg is the one that cannot be satisfied by a mistake agreeing with
// itself. A declaration keyed by Verb("kill") would pass any map-to-map
// comparison, because both sides would have it; only reading the constants
// catches a lifecycle action that exists and was never declared.
func TestTheVerbSetIsClosed(t *testing.T) {
	declared := constNamesFrom(t, "verbs.go", "Verb")
	if len(declared) == 0 {
		t.Fatal("no Verb constants were found; the scan is broken")
	}
	for _, v := range declared {
		if _, ok := decls[Verb(v)]; !ok {
			t.Errorf("Verb %q is a constant with no declaration", v)
		}
	}
	for v := range decls {
		if !slices.Contains(declared, string(v)) {
			t.Errorf("verb %q has a declaration and is not one of the constants in verbs.go: "+
				"an operator action nobody declared", v)
		}
	}
}

// TestEveryVerbBecomesExactlyOneDeclaredPrivilegedVerb.
//
// The hostops verb a lifecycle verb becomes must be one hostops declares, and
// it must be a mutating one -- a lifecycle action that mapped onto an
// observing verb would report success having changed nothing.
func TestEveryVerbBecomesExactlyOneDeclaredPrivilegedVerb(t *testing.T) {
	for _, v := range Verbs() {
		d, _ := Lookup(v)
		hd, ok := hostops.Lookup(d.Host)
		if !ok {
			t.Errorf("%s maps to %q, which hostops does not declare", v, d.Host)
			continue
		}
		if hd.Effect != hostops.Mutates {
			t.Errorf("%s maps to %s, which observes; a lifecycle verb that changes nothing "+
				"would report success having done nothing", v, d.Host)
		}
		if hd.Audit != d.Audit {
			t.Errorf("%s declares audit %q and hostops writes %q; the params assembled at the "+
				"call site would be filtered against the wrong allowlist", v, d.Audit, hd.Audit)
		}
		if !audit.Known(d.Audit) {
			t.Errorf("%s declares audit action %q, which internal/audit does not know", v, d.Audit)
		}
	}
}

// TestEnableAndDisableAreNotLifecycleVerbs guards a deliberate omission.
//
// Section 13.4 defines a durable stop as `gvm disable`, which sets
// `enabled: false` and applies. That is a change to INTENT, and intent has one
// write path (internal/reconcile). A verb here that could enable or disable
// would be a second way to change what an instance is, and it would do it
// without re-rendering the drop-in the new value implies.
//
// `hold` is on the same list for a different reason: it is not an operation,
// it is what a stop leaves behind, and a verb that set it without stopping
// anything would disarm the guard timer for a running instance with no record
// of why the instance later stayed down.
func TestEnableAndDisableAreNotLifecycleVerbs(t *testing.T) {
	for _, name := range []string{
		"enable", "disable", "hold", "unhold", "destroy", "rebuild",
		"apply", "install", "update", "backup", "restore",
	} {
		if _, ok := decls[Verb(name)]; ok {
			t.Errorf("%q is declared as a lifecycle verb. Every name on this list either "+
				"changes intent (which is internal/reconcile's single write path) or is a job "+
				"(which needs a job_runs row and a RunningGuard, not a bare hostops call)", name)
		}
	}
}

func TestAllowedRefusesWhatWouldMakeThingsWorse(t *testing.T) {
	tests := []struct {
		name   string
		verb   Verb
		setup  func(*Observation)
		refuse bool
		force  bool
		hint   string
	}{
		{
			name: "start a healthy instance", verb: VerbStart,
			setup: func(o *Observation) { o.Unit.ActiveState = "inactive"; o.Status = nil },
		},
		{
			name: "start something with no spec", verb: VerbStart,
			setup:  func(o *Observation) { o.SpecPresent = false },
			refuse: true, hint: "gvm list",
		},
		{
			name: "stop something with no spec", verb: VerbStop,
			setup:  func(o *Observation) { o.SpecPresent = false },
			refuse: true,
		},
		{
			name: "start while a job holds it", verb: VerbStart,
			setup: func(o *Observation) {
				o.Lease = &Lease{Kind: JobUpdate, PID: 900, StartedAt: at("2026-09-04T18:55:00Z")}
				o.LeaseState = LeaseLive
			},
			refuse: true, force: true, hint: "gvm job clear pz-main",
		},
		{
			// Overridable, because the operator may be stopping it precisely
			// BECAUSE the job is wedged.
			name: "stop while a job holds it", verb: VerbStop,
			setup: func(o *Observation) {
				o.Lease = &Lease{Kind: JobUpdate, PID: 900, StartedAt: at("2026-09-04T18:55:00Z")}
				o.LeaseState = LeaseLive
			},
			refuse: true, force: true,
		},
		{
			name: "start after a job died holding it", verb: VerbStart,
			setup: func(o *Observation) {
				o.Lease = &Lease{Kind: JobUpdate, PID: 900, StartedAt: at("2026-09-04T18:00:00Z")}
				o.LeaseState = LeaseStale
			},
			refuse: true, force: true, hint: "gvm job clear pz-main",
		},
		{
			// A stale lease is a reason not to bring an instance UP. It is
			// never a reason to leave one running.
			name: "stop after a job died holding it", verb: VerbStop,
			setup: func(o *Observation) {
				o.Lease = &Lease{Kind: JobUpdate, PID: 900, StartedAt: at("2026-09-04T18:00:00Z")}
				o.LeaseState = LeaseStale
			},
		},
		{
			name: "start on a manifest pin mismatch", verb: VerbStart,
			setup: func(o *Observation) { o.Pin.OK = false; o.Pin.Reason = "revision 8" },
			// Not forceable: the runner compares the pin itself and exits 64,
			// so --force would swap a clear message for a unit stuck in
			// "restart prevented".
			refuse: true, force: false, hint: "gvm manifest upgrade pz-main --dry",
		},
		{
			name: "start something never applied", verb: VerbStart,
			setup: func(o *Observation) {
				o.DropInPresent = false
				o.DropIn = DropInHeader{}
				o.Pin = Pin{}
			},
			refuse: true, hint: "gvm apply pz-main",
		},
		{
			name: "start a masked unit", verb: VerbStart,
			setup:  func(o *Observation) { o.Unit.LoadState = "masked" },
			refuse: true, hint: "systemctl unmask",
		},
		{
			name: "start with no install sentinel", verb: VerbStart,
			setup: func(o *Observation) {
				o.Unit.ActiveState = "inactive"
				o.Status = nil
				o.Installed = No
			},
			refuse: true, force: true, hint: "gvm install pz-main",
		},
		{
			// The sentinel is missing because the install is missing, but
			// stopping is still allowed: refusing to stop buys nothing.
			name: "stop with no install sentinel", verb: VerbStop,
			setup: func(o *Observation) {
				o.Unit.ActiveState = "inactive"
				o.Status = nil
				o.Installed = No
			},
		},
		{
			// Content that needs a human is a problem with the content, not
			// with running the server.
			name: "start with blocked content", verb: VerbStart,
			setup: func(o *Observation) {
				o.Unit.ActiveState = "inactive"
				o.Status = nil
				o.BlockedMods = true
			},
		},
		{
			name: "restart a healthy instance", verb: VerbRestart,
			setup: func(o *Observation) {},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := base()
			tc.setup(&o)
			err := Allowed(tc.verb, o, Derive(o))
			if !tc.refuse {
				if err != nil {
					t.Fatalf("Allowed refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Allowed permitted it")
			}
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("%v does not wrap ErrRefused", err)
			}
			var ref *Refusal
			if !errors.As(err, &ref) {
				t.Fatalf("%v is not a *Refusal", err)
			}
			if ref.Force != tc.force {
				t.Errorf("Force = %v, want %v (%s)", ref.Force, tc.force, ref.Reason)
			}
			if tc.hint != "" && !strings.Contains(ref.Hint, tc.hint) {
				t.Errorf("hint %q does not contain %q", ref.Hint, tc.hint)
			}
		})
	}
}

// TestAnUndeclaredVerbIsRefused. Allowed is reached from the CLI and, in
// phase 15, from a socket; a verb string it does not recognise must be a
// refusal and not a permit-by-default.
func TestAnUndeclaredVerbIsRefused(t *testing.T) {
	o := base()
	if err := Allowed(Verb("kill"), o, Derive(o)); !errors.Is(err, ErrUnknownVerb) {
		t.Fatalf("Allowed(kill) = %v, want ErrUnknownVerb", err)
	}
}

// TestEveryRefusalNamesANextMove. A refusal an operator cannot act on is one
// they will work around with --force, and then --force is how they always run
// the command.
func TestEveryRefusalNamesANextMove(t *testing.T) {
	for _, tc := range refusalShapes() {
		o := base()
		tc(&o)
		for _, v := range Verbs() {
			err := Allowed(v, o, Derive(o))
			var ref *Refusal
			if !errors.As(err, &ref) {
				continue
			}
			if ref.Hint == "" {
				t.Errorf("%s refused with no hint: %s", v, ref.Reason)
			}
			if ref.Reason == "" {
				t.Errorf("%s refused with no reason", v)
			}
		}
	}
}

func refusalShapes() []func(*Observation) {
	return []func(*Observation){
		func(o *Observation) { o.SpecPresent = false },
		func(o *Observation) { o.Game = "" },
		func(o *Observation) { o.Pin.OK = false },
		func(o *Observation) { o.Pin = Pin{} },
		func(o *Observation) { o.DropIn = DropInHeader{}; o.DropInPresent = false; o.Pin = Pin{} },
		func(o *Observation) { o.Unit.LoadState = "masked" },
		func(o *Observation) { o.Unit.LoadState = "not-found" },
		func(o *Observation) { o.Installed = No; o.Unit.ActiveState = "inactive"; o.Status = nil },
		func(o *Observation) { o.LeaseUnreadable = true },
		func(o *Observation) {
			o.Lease = &Lease{Kind: JobUpdate, PID: 9, StartedAt: at("2026-09-04T18:00:00Z")}
			o.LeaseState = LeaseStale
		},
		func(o *Observation) {
			o.Lease = &Lease{Kind: JobUpdate, PID: 9, StartedAt: at("2026-09-04T18:55:00Z")}
			o.LeaseState = LeaseLive
		},
	}
}

// constNamesFrom reads the string values of every `Name Type = "value"`
// constant declared in one of this package's source files.
func constNamesFrom(t *testing.T, file, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		id, ok := vs.Type.(*ast.Ident)
		if !ok || id.Name != typeName {
			return true
		}
		for _, v := range vs.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return true
	})
	return out
}

// TestThePhaseConstantsMatchTheSchemaEnum reads the SQL out of
// docs/ARCHITECTURE.md.
//
// instance_phase_t is a Postgres enum and Phase is a Go one, and neither
// compiler can see the other. A value added to one and not the other becomes a
// mirror write that fails at 3am on a value nothing ever tested.
func TestThePhaseConstantsMatchTheSchemaEnum(t *testing.T) {
	raw, err := os.ReadFile("../../docs/ARCHITECTURE.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	i := strings.Index(doc, "CREATE TYPE instance_phase_t")
	if i < 0 {
		t.Fatal("instance_phase_t is not in ARCHITECTURE.md; this test can no longer check anything")
	}
	j := strings.Index(doc[i:], ");")
	if j < 0 {
		t.Fatal("the instance_phase_t declaration is not terminated")
	}
	body := doc[i : i+j]
	var want []string
	for _, part := range strings.Split(body, "'") {
		if part != "" && !strings.ContainsAny(part, "(),\n ") {
			want = append(want, part)
		}
	}
	got := make([]string, 0, len(phaseOrder))
	for _, p := range phaseOrder {
		got = append(got, string(p))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Go phases     %v\nSQL enum      %v\nThe ORDER matters too: the schema calls the "+
			"enum's ordinal order load-bearing", got, want)
	}
}
