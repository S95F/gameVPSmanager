package modrinth

import (
	"context"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/content"
)

func TestModrinthSourceKind(t *testing.T) {
	s := New(nil)
	if s.Kind() != content.KindModrinth {
		t.Errorf("Kind() = %q, want %q", s.Kind(), content.KindModrinth)
	}
}

func TestModrinthResolveEmpty(t *testing.T) {
	s := New(nil)
	plan, err := s.Resolve(context.Background(), &content.Input{}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Declared != 0 {
		t.Errorf("Declared = %d, want 0", plan.Declared)
	}
	if len(plan.Entries) != 0 {
		t.Errorf("Entries = %d, want 0", len(plan.Entries))
	}
	if len(plan.Unaccounted) != 0 {
		t.Errorf("Unaccounted = %d, want 0", len(plan.Unaccounted))
	}
}

func TestModrinthDepsCheckIncompatible(t *testing.T) {
	deps := []modrinthDep{{
		ProjectID:      "other-mod",
		DependencyType: "incompatible",
	}}
	if err := checkDeps(deps); err == nil {
		t.Error("checkDeps should fail for incompatible dependency")
	}
}

func TestModrinthDepsCheckEmbeddedSkipped(t *testing.T) {
	deps := []modrinthDep{{
		ProjectID:      "embedded-lib",
		DependencyType: "embedded",
	}}
	if err := checkDeps(deps); err != nil {
		t.Errorf("checkDeps should skip embedded dep, got %v", err)
	}
}

func TestModrinthDepsCheckRequired(t *testing.T) {
	deps := []modrinthDep{{
		ProjectID:      "required-dep",
		DependencyType: "required",
	}}
	if err := checkDeps(deps); err != nil {
		t.Errorf("checkDeps should allow required dep, got %v", err)
	}
}

func TestParseModrinthRefWithVersion(t *testing.T) {
	proj, ver := parseModrinthRef("fabric-api:version1")
	if proj != "fabric-api" || ver != "version1" {
		t.Errorf("got %q:%q, want fabric-api:version1", proj, ver)
	}
}

func TestParseModrinthRefProjectOnly(t *testing.T) {
	proj, ver := parseModrinthRef("fabric-api")
	if proj != "fabric-api" || ver != "" {
		t.Errorf("got %q:%q, want fabric-api:", proj, ver)
	}
}

func TestModrinthMCVersion(t *testing.T) {
	in := &content.Input{Schema: &content.Schema{GameVersion: "1.21.4"}}
	if got := modrinthMCVersion(in); got != "1.21.4" {
		t.Errorf("modrinthMCVersion = %q, want 1.21.4", got)
	}
}

func TestModrinthLoader(t *testing.T) {
	in := &content.Input{Schema: &content.Schema{Loader: "fabric"}}
	if got := modrinthLoader(in); got != "fabric" {
		t.Errorf("modrinthLoader = %q, want fabric", got)
	}
}

func TestModrinthMCVersionNilSchema(t *testing.T) {
	if got := modrinthMCVersion(&content.Input{}); got != "" {
		t.Errorf("modrinthMCVersion with nil schema = %q, want empty", got)
	}
}

func TestModrinthContainsString(t *testing.T) {
	ss := []string{"a", "b", "c"}
	if !containsString(ss, "b") {
		t.Error("containsString: should find b")
	}
	if containsString(ss, "d") {
		t.Error("containsString: should not find d")
	}
}
