package authz

import (
	"testing"
)

func TestRoleHierarchy(t *testing.T) {
	if !RoleAdmin.AtLeast(RoleOperator) {
		t.Error("admin should be at least operator")
	}
	if !RoleOperator.AtLeast(RoleViewer) {
		t.Error("operator should be at least viewer")
	}
	if RoleViewer.AtLeast(RoleOperator) {
		t.Error("viewer should not be at least operator")
	}
}

func TestGreatestRole(t *testing.T) {
	if got := Greatest(RoleViewer, RoleAdmin); got != RoleAdmin {
		t.Errorf("got %v, want %v", got, RoleAdmin)
	}
	if got := Greatest(RoleAdmin, RoleViewer); got != RoleAdmin {
		t.Errorf("got %v, want %v", got, RoleAdmin)
	}
}

func TestViewerCanView(t *testing.T) {
	if err := Check(RoleViewer, PermInstanceView); err != nil {
		t.Errorf("viewer should be able to view instances: %v", err)
	}
}

func TestViewerCannotCreate(t *testing.T) {
	if err := Check(RoleViewer, PermInstanceCreate); err == nil {
		t.Error("viewer should not be able to create instances")
	}
}

func TestOperatorCanStart(t *testing.T) {
	if err := Check(RoleOperator, PermInstanceStart); err != nil {
		t.Errorf("operator should be able to start instances: %v", err)
	}
}

func TestOperatorCannotCreate(t *testing.T) {
	if err := Check(RoleOperator, PermInstanceCreate); err == nil {
		t.Error("operator should not be able to create instances")
	}
}

func TestAdminCanDoEverything(t *testing.T) {
	for _, perms := range matrix {
		for _, p := range perms {
			if err := Check(RoleAdmin, p); err != nil {
				t.Errorf("admin denied %v: %v", p, err)
			}
		}
	}
}

func TestAdminInheritsViewerAndOperatorPerms(t *testing.T) {
	for _, p := range matrix[RoleViewer] {
		if err := Check(RoleAdmin, p); err != nil {
			t.Errorf("admin should inherit viewer perm %v: %v", p, err)
		}
	}
	for _, p := range matrix[RoleOperator] {
		if err := Check(RoleAdmin, p); err != nil {
			t.Errorf("admin should inherit operator perm %v: %v", p, err)
		}
	}
}

func TestAllGrantsNoDuplicates(t *testing.T) {
	grants := AllGrants()
	seen := map[string]bool{}
	for _, g := range grants {
		k := string(g.Role) + ":" + string(g.Permission)
		if seen[k] {
			t.Errorf("duplicate grant: %s", k)
		}
		seen[k] = true
	}
}

func TestParseRole(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Role
		ok   bool
	}{
		{"viewer", RoleViewer, true},
		{"operator", RoleOperator, true},
		{"admin", RoleAdmin, true},
		{"superuser", "", false},
		{"", "", false},
	} {
		got, err := ParseRole(tc.in)
		if tc.ok && err != nil {
			t.Errorf("ParseRole(%q): unexpected error: %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ParseRole(%q): expected error", tc.in)
		}
		if tc.ok && got != tc.want {
			t.Errorf("ParseRole(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
