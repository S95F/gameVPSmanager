package spec

import (
	"strings"
	"testing"
)

const diffBase = `apiVersion: gvm/v1
kind: Game
metadata:
  id: projectzomboid
  name: Project Zomboid
  revision: 7
  doc: "RCON does not bind until boot completes"
spec:
  driverABI: 1
  runtime:
    exec: ./start-server.sh
    args:
      - "-servername"
      - "{{ .Var.server_name }}"
    env:
      HOME: "{{ .Home }}"
  readiness:
    hard_deadline: 3600s
    probes:
      - {kind: log_marker, pattern: "SERVER STARTED"}
    fail_markers:
      - {pattern: "Address already in use", fatal: true}
  shutdown:
    budget: 300s
    steps:
      - {via: console, send: save, wait: 20s}
  ports:
    - {name: game, proto: udp, default: 16261, expose: public}
  vars:
    jvm_xmx: {type: memory, default: 8G, apply: restart, help: "leave 3GB free"}
  backup:
    destroy_protected: ["Zomboid/Saves"]
`

func mustParseManifest(t *testing.T, src string) *Manifest {
	t.Helper()
	m, diags, err := parseManifest("test.yaml", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v (%s)", err, diags)
	}
	return m
}

func TestDiffClassifiesEveryFieldItReports(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Manifest)
		wantPath  string
		wantKind  ChangeKind
		wantClass Class
		wantFrom  string
		wantTo    string
	}{
		{
			name:      "shutdown budget",
			mutate:    func(m *Manifest) { m.Spec.Shutdown.Budget = Duration(420e9) },
			wantPath:  "spec.shutdown.budget",
			wantKind:  ChangeModified,
			wantClass: ClassShutdown,
			wantFrom:  "300s",
			wantTo:    "420s",
		},
		{
			name:      "shutdown step wait",
			mutate:    func(m *Manifest) { m.Spec.Shutdown.Steps[0].Wait = Duration(45e9) },
			wantPath:  "spec.shutdown.steps[0].wait",
			wantKind:  ChangeModified,
			wantClass: ClassShutdown,
			wantFrom:  "20s",
			wantTo:    "45s",
		},
		{
			name:      "an argv element",
			mutate:    func(m *Manifest) { m.Spec.Runtime.Args[1].Value = "-XX:+UseZGC" },
			wantPath:  "spec.runtime.args[1].value",
			wantKind:  ChangeModified,
			wantClass: ClassArgv,
			wantFrom:  `"{{ .Var.server_name }}"`,
			wantTo:    `"-XX:+UseZGC"`,
		},
		{
			name:      "an appended argv element",
			mutate:    func(m *Manifest) { m.Spec.Runtime.Args = append(m.Spec.Runtime.Args, Arg{Value: "-XX:+UseZGC"}) },
			wantPath:  "spec.runtime.args[2]",
			wantKind:  ChangeAdded,
			wantClass: ClassArgv,
			wantTo:    `{value: "-XX:+UseZGC"}`,
		},
		{
			name:      "the exec itself",
			mutate:    func(m *Manifest) { m.Spec.Runtime.Exec = "./start-server-64.sh" },
			wantPath:  "spec.runtime.exec",
			wantKind:  ChangeModified,
			wantClass: ClassArgv,
			wantFrom:  `"./start-server.sh"`,
			wantTo:    `"./start-server-64.sh"`,
		},
		{
			name:      "an environment variable added",
			mutate:    func(m *Manifest) { m.Spec.Runtime.Env["JAVA_HOME"] = "/usr/lib/jvm/temurin-21" },
			wantPath:  "spec.runtime.env[JAVA_HOME]",
			wantKind:  ChangeAdded,
			wantClass: ClassArgv,
			wantTo:    `"/usr/lib/jvm/temurin-21"`,
		},
		{
			name:      "an environment variable removed",
			mutate:    func(m *Manifest) { delete(m.Spec.Runtime.Env, "HOME") },
			wantPath:  "spec.runtime.env[HOME]",
			wantKind:  ChangeRemoved,
			wantClass: ClassArgv,
			wantFrom:  `"{{ .Home }}"`,
		},
		{
			name:      "a port default",
			mutate:    func(m *Manifest) { m.Spec.Ports[0].Default = 16271 },
			wantPath:  "spec.ports[0].default",
			wantKind:  ChangeModified,
			wantClass: ClassPorts,
			wantFrom:  "16261",
			wantTo:    "16271",
		},
		{
			name:      "a port exposure",
			mutate:    func(m *Manifest) { m.Spec.Ports[0].Expose = ExposeAdmin },
			wantPath:  "spec.ports[0].expose",
			wantKind:  ChangeModified,
			wantClass: ClassPorts,
			wantFrom:  `"public"`,
			wantTo:    `"admin"`,
		},
		{
			name:      "a destroy glob",
			mutate:    func(m *Manifest) { m.Spec.Backup.DestroyProtected[0] = "Zomboid/" },
			wantPath:  "spec.backup.destroy_protected[0]",
			wantKind:  ChangeModified,
			wantClass: ClassDestroy,
			wantFrom:  `"Zomboid/Saves"`,
			wantTo:    `"Zomboid/"`,
		},
		{
			name: "a fail marker appended",
			mutate: func(m *Manifest) {
				m.Spec.Readiness.Fail = append(m.Spec.Readiness.Fail, FailMarker{Pattern: "OutOfMemoryError", Fatal: true})
			},
			wantPath:  "spec.readiness.fail_markers[1]",
			wantKind:  ChangeAdded,
			wantClass: ClassOther,
			wantTo:    `{pattern: "OutOfMemoryError", fatal: true}`,
		},
		{
			name:      "prose",
			mutate:    func(m *Manifest) { m.Metadata.Doc = "now with more detail" },
			wantPath:  "metadata.doc",
			wantKind:  ChangeModified,
			wantClass: ClassCosmetic,
			wantFrom:  `"RCON does not bind until boot completes"`,
			wantTo:    `"now with more detail"`,
		},
		{
			name:      "a var's tooltip",
			mutate:    func(m *Manifest) { v := m.Spec.Vars["jvm_xmx"]; v.Help = "leave 4GB free"; m.Spec.Vars["jvm_xmx"] = v },
			wantPath:  "spec.vars[jvm_xmx].help",
			wantKind:  ChangeModified,
			wantClass: ClassCosmetic,
			wantFrom:  `"leave 3GB free"`,
			wantTo:    `"leave 4GB free"`,
		},
		{
			// NOT cosmetic. A readiness deadline is not one of the four risk
			// classes, and calling it cosmetic in the summary is how a change to
			// how long a boot may take gets skimmed past.
			name:      "a readiness deadline",
			mutate:    func(m *Manifest) { m.Spec.Readiness.HardDeadline = Duration(7200e9) },
			wantPath:  "spec.readiness.hard_deadline",
			wantKind:  ChangeModified,
			wantClass: ClassOther,
			wantFrom:  "3600s",
			wantTo:    "7200s",
		},
		{
			name:      "a whole block appearing",
			mutate:    func(m *Manifest) { m.Spec.Control = &ControlSpec{Kind: "rcon", Port: "rcon", Dialect: DialectSimple} },
			wantPath:  "spec.control",
			wantKind:  ChangeAdded,
			wantClass: ClassOther,
			wantTo:    `{kind: "rcon", port: "rcon", dialect: "simple"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			from := mustParseManifest(t, diffBase)
			to := mustParseManifest(t, diffBase)
			tc.mutate(to)

			d := Diff(from, to)
			if len(d.Changes) != 1 {
				t.Fatalf("got %d changes, want exactly 1:\n%s", len(d.Changes), d)
			}
			c := d.Changes[0]
			if c.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", c.Path, tc.wantPath)
			}
			if c.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", c.Kind, tc.wantKind)
			}
			if c.Class != tc.wantClass {
				t.Errorf("class = %q, want %q", c.Class, tc.wantClass)
			}
			if c.From != tc.wantFrom || c.To != tc.wantTo {
				t.Errorf("%q -> %q, want %q -> %q", c.From, c.To, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestDiffOfIdenticalManifestsIsEmptyAndSaysHowMuchItLookedAt(t *testing.T) {
	from := mustParseManifest(t, diffBase)
	to := mustParseManifest(t, diffBase)
	d := Diff(from, to)
	if !d.Empty() {
		t.Fatalf("changes reported for identical manifests:\n%s", d)
	}
	// The "= N other fields unchanged" line is the evidence that the diff is
	// field-level rather than a comparison of the two files' bytes.
	if d.Unchanged < 40 {
		t.Errorf("Unchanged = %d, want the whole schema walked", d.Unchanged)
	}
	if d.RequiresRestart() {
		t.Error("an empty diff requires a restart")
	}
}

func TestDiffIgnoresProvenance(t *testing.T) {
	// Origin, SourceRef, SHA256 and Raw describe where a manifest came from, not
	// what it says. A diff that reported them would flag every `manifest
	// upgrade` as changing everything.
	from := mustParseManifest(t, diffBase)
	to := mustParseManifest(t, diffBase)
	from.Origin, to.Origin = OriginPackage, OriginOverride
	from.SourceRef, to.SourceRef = "/usr/share/gvm/games/pz.yaml", "/etc/gvm/games/pz.yaml"
	from.SHA256, to.SHA256 = "aaaa", "bbbb"

	if d := Diff(from, to); !d.Empty() {
		t.Errorf("provenance was diffed:\n%s", d)
	}
}

func TestDiffAgainstNothingDescribesTheManifest(t *testing.T) {
	// `gvm manifest diff` on a first apply has no "from". It must describe, not
	// panic.
	d := Diff(nil, mustParseManifest(t, diffBase))
	if d.Empty() {
		t.Fatal("no changes reported against an empty manifest")
	}
	if d.Count(ClassArgv) == 0 {
		t.Errorf("no argv changes reported:\n%s", d)
	}
}

func TestRequiresRestart(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   bool
	}{
		{"argv", func(m *Manifest) { m.Spec.Runtime.Exec = "./other.sh" }, true},
		{"port default", func(m *Manifest) { m.Spec.Ports[0].Default = 16271 }, true},
		{"shutdown budget", func(m *Manifest) { m.Spec.Shutdown.Budget = Duration(420e9) }, true},
		{"readiness deadline", func(m *Manifest) { m.Spec.Readiness.HardDeadline = Duration(7200e9) }, true},
		{"prose", func(m *Manifest) { m.Metadata.Doc = "reworded" }, false},
		{"destroy glob", func(m *Manifest) { m.Spec.Backup.DestroyProtected[0] = "Zomboid/" }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			from := mustParseManifest(t, diffBase)
			to := mustParseManifest(t, diffBase)
			tc.mutate(to)
			if got := Diff(from, to).RequiresRestart(); got != tc.want {
				t.Errorf("RequiresRestart() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiffRenderingCannotBeSkimmedPast(t *testing.T) {
	from := mustParseManifest(t, diffBase)
	to := mustParseManifest(t, strings.Replace(diffBase, "revision: 7", "revision: 9", 1))
	to.Spec.Runtime.Args[1].Value = "-XX:+UseZGC"
	to.Spec.Shutdown.Budget = Duration(420e9)
	to.SourceRef = "/etc/gvm/games/projectzomboid.yaml"

	out := Diff(from, to).String()
	for _, want := range []string{
		"projectzomboid",
		"revision 7 -> 9",
		"/etc/gvm/games/projectzomboid.yaml",
		"spec.runtime.args[1].value",
		"CHANGES THE LAUNCH COMMAND OF A LIVE SERVER",
		"other fields unchanged",
		"1 argv change, 0 destroy-glob changes, 0 port changes, 1 shutdown change.",
		"Requires a restart",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendering does not contain %q:\n%s", want, out)
		}
	}
}

func TestDiffHeaderNamesAnInPlaceEdit(t *testing.T) {
	// Same revision, different bytes: every other line of the diff looks like an
	// ordinary upgrade, so the header has to say so.
	from := mustParseManifest(t, diffBase)
	to := mustParseManifest(t, diffBase+"# trailing comment\n")
	to.Spec.Runtime.Exec = "./other.sh"

	out := Diff(from, to).String()
	if !strings.Contains(out, "edited in place") {
		t.Errorf("header does not flag the in-place edit:\n%s", out)
	}
}

func TestPathPrefixMatchesWholeSegments(t *testing.T) {
	tests := []struct {
		path, prefix string
		want         bool
	}{
		{"spec.ports", "spec.ports", true},
		{"spec.ports[0].default", "spec.ports", true},
		{"spec.ports.anything", "spec.ports", true},
		{"spec.port_stride[game]", "spec.ports", false},
		{"spec.runtime.argsomething", "spec.runtime.args", false},
	}
	for _, tc := range tests {
		if got := pathHasPrefix(tc.path, tc.prefix); got != tc.want {
			t.Errorf("pathHasPrefix(%q, %q) = %v, want %v", tc.path, tc.prefix, got, tc.want)
		}
	}
	if got := classify("spec.port_stride[game]"); got != ClassPorts {
		t.Errorf("classify(port_stride) = %q, want %q", got, ClassPorts)
	}
}
