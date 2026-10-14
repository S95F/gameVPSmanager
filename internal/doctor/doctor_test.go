package doctor

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/sshport"
)

func sshportDetection() sshport.Detection { return sshport.Detection{} }

// fakeCheckEnv is a test double for lifecycle.CheckEnv.
type fakeCheckEnv struct{ pgReachable bool }

func (f fakeCheckEnv) PostgresReachable(_ context.Context) bool { return f.pgReachable }

// cleanUnitFS returns a minimal in-memory FS with clean unit files (no postgresql refs).
func cleanUnitFS() fs.FS {
	return fstest.MapFS{
		"systemd/gvm@.service": &fstest.MapFile{
			Data: []byte("[Unit]\nDescription=GVM game server\n# Deliberately NOT After=postgresql.service\n[Service]\nExecStart=/usr/bin/gvm run %i\n"),
		},
		"systemd/gvm-job@.service": &fstest.MapFile{
			Data: []byte("[Unit]\nDescription=GVM job\n[Service]\nExecStart=/usr/bin/gvm job run %i\n"),
		},
		"systemd/gvm-job@_guard.service": &fstest.MapFile{
			Data: []byte("[Unit]\nDescription=GVM job guard\n[Service]\nExecStart=/usr/bin/gvm job guard %i\n"),
		},
		"systemd/gvm-job@_verify.service": &fstest.MapFile{
			Data: []byte("[Unit]\nDescription=GVM job verify\n[Service]\nExecStart=/usr/bin/gvm job verify %i\n"),
		},
	}
}

// --- Registry and severity tests ---

func TestRegistryRunAllNoPanic(t *testing.T) {
	reg := &lifecycle.CheckRegistry{}
	reg.Register(lifecycle.DoctorCheck{
		ID:      "TEST-001",
		Summary: "always ok",
		Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
			return lifecycle.CheckFinding{ID: "TEST-001", Severity: lifecycle.CheckOK, Message: "ok"}
		},
	})
	reg.Register(lifecycle.DoctorCheck{
		ID:      "TEST-002",
		Summary: "always panics",
		Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
			panic("deliberate test panic")
		},
	})
	findings := reg.RunAll(context.Background(), fakeCheckEnv{})
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}
	if findings[1].Severity != lifecycle.CheckProblem {
		t.Errorf("panicking check must produce CheckProblem, got %v", findings[1].Severity)
	}
	if !strings.Contains(findings[1].Message, "deliberate test panic") {
		t.Errorf("problem message should contain panic value: %q", findings[1].Message)
	}
}

func TestCountCheckProblems(t *testing.T) {
	findings := []lifecycle.CheckFinding{
		{Severity: lifecycle.CheckOK},
		{Severity: lifecycle.CheckProblem},
		{Severity: lifecycle.CheckWarning},
		{Severity: lifecycle.CheckProblem},
	}
	if n := lifecycle.CountCheckProblems(findings); n != 2 {
		t.Errorf("expected 2, got %d", n)
	}
}

func TestFormatFindingGlyphs(t *testing.T) {
	cases := []struct {
		sev   lifecycle.CheckSeverity
		glyph string
	}{
		{lifecycle.CheckOK, "✓"},
		{lifecycle.CheckProblem, "✗"},
		{lifecycle.CheckWarning, "!"},
		{lifecycle.CheckNote, "·"},
		{lifecycle.CheckSkip, "-"},
	}
	for _, tc := range cases {
		f := lifecycle.CheckFinding{ID: "X-001", Severity: tc.sev, Message: "test"}
		out := lifecycle.FormatFinding(f)
		if !strings.Contains(out, tc.glyph) {
			t.Errorf("severity %s: expected glyph %q in %q", tc.sev, tc.glyph, out)
		}
	}
}

func TestFormatFindingWithFix(t *testing.T) {
	f := lifecycle.CheckFinding{
		ID:       "FW-001",
		Severity: lifecycle.CheckProblem,
		Message:  "firewall not active",
		Fix:      "gvm firewall enable",
	}
	out := lifecycle.FormatFinding(f)
	if !strings.Contains(out, "Fix:") {
		t.Errorf("expected Fix: line, got: %q", out)
	}
	if !strings.Contains(out, "gvm firewall enable") {
		t.Errorf("expected fix command in output, got: %q", out)
	}
}

// --- HOST-010: no unit depends on postgres ---

func TestHostCheckNoUnitDependsOnPostgres(t *testing.T) {
	checks := HostChecks(cleanUnitFS())
	var c *lifecycle.DoctorCheck
	for i := range checks {
		if checks[i].ID == "HOST-010" {
			c = &checks[i]
			break
		}
	}
	if c == nil {
		t.Fatal("HOST-010 check not found")
	}
	f := c.Run(context.Background(), fakeCheckEnv{})
	if f.Severity == lifecycle.CheckProblem {
		t.Errorf("HOST-010 found postgresql reference in units: %s", f.Message)
	}
}

// --- EDGE checks ---

func TestEdgeCheckAdminSocketDetectsLocalhost(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Edge.Caddy.Admin = "localhost:2019"
	checks := EdgeChecks(&host)
	var c *lifecycle.DoctorCheck
	for i := range checks {
		if checks[i].ID == "EDGE-004" {
			c = &checks[i]
			break
		}
	}
	if c == nil {
		t.Fatal("EDGE-004 check not found")
	}
	f := c.Run(context.Background(), fakeCheckEnv{})
	if f.Severity != lifecycle.CheckProblem {
		t.Errorf("EDGE-004 should be Problem when admin is localhost:2019, got %v: %s", f.Severity, f.Message)
	}
	if !strings.Contains(f.Message, "D-36") {
		t.Errorf("EDGE-004 problem message should mention D-36: %q", f.Message)
	}
}

func TestEdgeCheckAdminSocketPassesForUnixSocket(t *testing.T) {
	host := spec.DefaultHostConfig()
	checks := EdgeChecks(&host)
	var c *lifecycle.DoctorCheck
	for i := range checks {
		if checks[i].ID == "EDGE-004" {
			c = &checks[i]
			break
		}
	}
	if c == nil {
		t.Fatal("EDGE-004 not found")
	}
	f := c.Run(context.Background(), fakeCheckEnv{})
	if f.Severity != lifecycle.CheckOK {
		t.Errorf("EDGE-004 should be OK for unix socket, got %v: %s", f.Severity, f.Message)
	}
}

func TestEdgeCheckMinVersion(t *testing.T) {
	cases := []struct {
		ver  string
		want bool
	}{
		{"v2.11.4 h1:abc", true},
		{"v2.11.1", true},
		{"v2.11.0", false},
		{"v2.10.9", false},
		{"v3.0.0", true},
	}
	for _, tc := range cases {
		got := meetsMinVersion(tc.ver, "2.11.1")
		if got != tc.want {
			t.Errorf("meetsMinVersion(%q, 2.11.1) = %v, want %v", tc.ver, got, tc.want)
		}
	}
}

// --- FW checks ---

func TestFWCheckActiveFailsWhenInactive(t *testing.T) {
	host := spec.DefaultHostConfig()
	host.Spec.Firewall.Backend = "ufw"
	checks := FirewallChecks(&host, nil, nil, nil, false, true, sshportDetection())
	var c *lifecycle.DoctorCheck
	for i := range checks {
		if checks[i].ID == "FW-001" {
			c = &checks[i]
			break
		}
	}
	if c == nil {
		t.Fatal("FW-001 not found")
	}
	f := c.Run(context.Background(), fakeCheckEnv{})
	if f.Severity != lifecycle.CheckProblem {
		t.Errorf("FW-001 should be Problem when ufw inactive, got %v", f.Severity)
	}
}

func TestFWCheckParseNotTrustedIsAProblem(t *testing.T) {
	host := spec.DefaultHostConfig()
	checks := FirewallChecks(&host, nil, nil, nil, true, false, sshportDetection())
	var c *lifecycle.DoctorCheck
	for i := range checks {
		if checks[i].ID == "FW-003" {
			c = &checks[i]
			break
		}
	}
	if c == nil {
		t.Fatal("FW-003 not found")
	}
	f := c.Run(context.Background(), fakeCheckEnv{})
	if f.Severity != lifecycle.CheckProblem {
		t.Errorf("FW-003 should be Problem when parse not trusted, got %v", f.Severity)
	}
}

// --- INST-002: OOM sentence ---

func TestInstCheckMemoryOOMSentenceVerbatim(t *testing.T) {
	inst := spec.Instance{
		Metadata: spec.InstanceMeta{Name: "pz1"},
		Spec: spec.InstanceSpec{
			Resources: spec.Resources{MemoryMax: "6G"},
			Vars: map[string]string{
				"jvm_xmx":         "5g",
				"native_headroom": "3G",
			},
		},
	}
	manifest := &spec.Manifest{}
	checks := InstanceChecks(inst, manifest)
	var c *lifecycle.DoctorCheck
	for i := range checks {
		if checks[i].ID == "INST-002" {
			c = &checks[i]
			break
		}
	}
	if c == nil {
		t.Fatal("INST-002 not found")
	}
	f := c.Run(context.Background(), fakeCheckEnv{})
	if f.Severity != lifecycle.CheckProblem {
		t.Errorf("INST-002 should be Problem (6G < 5g+3G=8G), got %v: %s", f.Severity, f.Message)
	}
	if !strings.Contains(f.Message, oomSentence) {
		t.Errorf("INST-002 must contain the OOM sentence verbatim\ngot: %q\nwant: %q", f.Message, oomSentence)
	}
}

func TestInstCheckMemoryOKWhenSufficient(t *testing.T) {
	inst := spec.Instance{
		Metadata: spec.InstanceMeta{Name: "pz1"},
		Spec: spec.InstanceSpec{
			Resources: spec.Resources{MemoryMax: "12G"},
			Vars: map[string]string{
				"jvm_xmx":         "5g",
				"native_headroom": "3G",
			},
		},
	}
	manifest := &spec.Manifest{}
	checks := InstanceChecks(inst, manifest)
	var c *lifecycle.DoctorCheck
	for i := range checks {
		if checks[i].ID == "INST-002" {
			c = &checks[i]
			break
		}
	}
	if c == nil {
		t.Fatal("INST-002 not found")
	}
	f := c.Run(context.Background(), fakeCheckEnv{})
	if f.Severity != lifecycle.CheckOK {
		t.Errorf("INST-002 should be OK (12G >= 8G), got %v: %s", f.Severity, f.Message)
	}
}

// --- INST-010: requires: violation ---

func TestInstCheckOrderViolation(t *testing.T) {
	inst := spec.Instance{
		Metadata: spec.InstanceMeta{Name: "pz1"},
		Spec: spec.InstanceSpec{
			Content: map[string]spec.ContentList{
				"mods": {
					Acquire: []spec.ContentRef{
						{ID: "mod-b", Requires: []string{"mod-a"}},
						{ID: "mod-a"},
					},
				},
			},
		},
	}
	manifest := &spec.Manifest{}
	checks := InstanceChecks(inst, manifest)
	var c *lifecycle.DoctorCheck
	for i := range checks {
		if checks[i].ID == "INST-010" {
			c = &checks[i]
			break
		}
	}
	if c == nil {
		t.Fatal("INST-010 not found")
	}
	f := c.Run(context.Background(), fakeCheckEnv{})
	if f.Severity != lifecycle.CheckProblem {
		t.Errorf("INST-010 should be Problem for ordering violation, got %v: %s", f.Severity, f.Message)
	}
}

func TestInstCheckOrderOK(t *testing.T) {
	inst := spec.Instance{
		Metadata: spec.InstanceMeta{Name: "pz1"},
		Spec: spec.InstanceSpec{
			Content: map[string]spec.ContentList{
				"mods": {
					Acquire: []spec.ContentRef{
						{ID: "mod-a"},
						{ID: "mod-b", Requires: []string{"mod-a"}},
					},
				},
			},
		},
	}
	manifest := &spec.Manifest{}
	checks := InstanceChecks(inst, manifest)
	var c *lifecycle.DoctorCheck
	for i := range checks {
		if checks[i].ID == "INST-010" {
			c = &checks[i]
			break
		}
	}
	if c == nil {
		t.Fatal("INST-010 not found")
	}
	f := c.Run(context.Background(), fakeCheckEnv{})
	if f.Severity != lifecycle.CheckOK {
		t.Errorf("INST-010 should be OK for correct ordering, got %v: %s", f.Severity, f.Message)
	}
}
