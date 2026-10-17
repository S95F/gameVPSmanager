package authz

import (
	"testing"
)

func TestRoleHierarchy(t *testing.T) {
	if !RoleOwner.AtLeast(RoleAdmin) {
		t.Error("owner should be at least admin")
	}
	if !RoleAdmin.AtLeast(RoleOperator) {
		t.Error("admin should be at least operator")
	}
	if !RoleOperator.AtLeast(RoleViewer) {
		t.Error("operator should be at least viewer")
	}
	if RoleViewer.AtLeast(RoleOperator) {
		t.Error("viewer should not be at least operator")
	}
	if RoleAdmin.AtLeast(RoleOwner) {
		t.Error("admin should not be at least owner")
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

func TestOwnerCanDoEverything(t *testing.T) {
	for _, perms := range matrix {
		for _, p := range perms {
			if err := Check(RoleOwner, p); err != nil {
				t.Errorf("owner denied %v: %v", p, err)
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

// TestOperatorCeiling is §15.2's security line: the moderator role may run a
// server, but every irreversible or host-shaping action is above it. A prior
// matrix handed these to operator, which is how a trusted moderator could
// restore over a world or prune the backups.
func TestOperatorCeiling(t *testing.T) {
	denied := []Permission{
		PermInstanceApply,
		PermInstanceRebuild,
		PermInstanceCreate,
		PermInstanceDestroy,
		PermBackupRestore,
		PermBackupPrune,
		PermContentEdit,
		PermAdoptCutover,
		PermSecretRotate,
		PermFirewallEdit,
		PermUserCreate,
		PermManifestPublish,
	}
	for _, p := range denied {
		if err := Check(RoleOperator, p); err == nil {
			t.Errorf("operator must NOT hold %v, but Check allowed it", p)
		}
	}
	// What it SHOULD be able to do, to prove the ceiling did not take the floor.
	for _, p := range []Permission{
		PermInstanceStart, PermInstanceStop, PermInstanceRestart,
		PermInstanceUpdate, PermContentSync, PermBackupCreate, PermJobRun,
	} {
		if err := Check(RoleOperator, p); err != nil {
			t.Errorf("operator should hold %v: %v", p, err)
		}
	}
}

// TestAdminCeiling: the few truly host-shaping actions are owner-only, and an
// admin is refused them.
func TestAdminCeiling(t *testing.T) {
	for _, p := range []Permission{
		PermUserCreate, PermUserDelete, PermSecretRotate,
		PermManifestPublish, PermTokenRevoke, PermDBMigrate,
	} {
		if err := Check(RoleAdmin, p); err == nil {
			t.Errorf("admin must NOT hold owner permission %v, but Check allowed it", p)
		}
	}
}

// TestCanGrant is §15.2's delegation rule.
func TestCanGrant(t *testing.T) {
	cases := []struct {
		granter, target Role
		want            bool
	}{
		{RoleOwner, RoleAdmin, true},
		{RoleOwner, RoleOwner, true},
		{RoleOwner, RoleViewer, true},
		{RoleAdmin, RoleOperator, true},
		{RoleAdmin, RoleViewer, true},
		{RoleAdmin, RoleAdmin, false},
		{RoleAdmin, RoleOwner, false},
		{RoleOperator, RoleViewer, false},
		{RoleViewer, RoleViewer, false},
	}
	for _, c := range cases {
		if got := CanGrant(c.granter, c.target); got != c.want {
			t.Errorf("CanGrant(%s, %s) = %v, want %v", c.granter, c.target, got, c.want)
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
		{"owner", RoleOwner, true},
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
