package authz

// matrix maps each role to the set of permissions it holds.
// A permission granted to a lower role is implicitly held by all higher roles
// (enforced by Check calling r.AtLeast).
//
// This is the single authoritative definition. At daemon start, gvmd upserts
// every row from AllGrants() into authz_permissions. Doctor fails when the
// table diverges.
var matrix = map[Role][]Permission{
	RoleViewer: {
		PermInstanceView,
		PermInstancePorts,
		PermInstanceLogs,
		PermContentView,
		PermBackupView,
		PermJobView,
		PermManifestView,
		PermDoctorRun,
		PermFirewallView,
		PermEdgeView,
		PermAuditView,
		PermTokenView,
		PermSecretView,
	},
	RoleOperator: {
		PermInstanceStart,
		PermInstanceStop,
		PermInstanceRestart,
		PermInstanceConsole,
		PermInstanceControl,
		PermInstanceApply,
		PermInstanceInstall,
		PermInstanceUpdate,
		PermInstanceRebuild,
		PermContentEdit,
		PermContentSync,
		PermBackupCreate,
		PermBackupRestore,
		PermBackupPrune,
		PermJobRun,
		PermAdoptDiscover,
		PermAdoptCutover,
		PermAdoptRollback,
		PermAdoptFinalize,
		PermSecretRotate,
		PermTokenCreate,
		PermTokenRevoke,
	},
	RoleAdmin: {
		PermInstanceCreate,
		PermInstanceDestroy,
		PermManifestPublish,
		PermManifestApprove,
		PermUserView,
		PermUserCreate,
		PermUserEdit,
		PermUserDelete,
		PermGrantEdit,
		PermFirewallEdit,
		PermEdgeEdit,
		PermDBMigrate,
		PermDBReplaySpool,
	},
}

// Check returns nil if role r holds permission p, or ErrForbidden.
func Check(r Role, p Permission) error {
	for _, minRole := range []Role{RoleViewer, RoleOperator, RoleAdmin} {
		for _, perm := range matrix[minRole] {
			if perm == p {
				if r.AtLeast(minRole) {
					return nil
				}
				return &ErrForbidden{Role: r, Permission: p}
			}
		}
	}
	// Unknown permission: treat as admin-only (fail closed).
	if !r.AtLeast(RoleAdmin) {
		return &ErrForbidden{Role: r, Permission: p}
	}
	return nil
}

// AllGrants returns all (role, permission) pairs from the matrix, including
// inherited ones. Used to populate authz_permissions at daemon start.
func AllGrants() []Grant {
	var out []Grant
	for _, role := range []Role{RoleViewer, RoleOperator, RoleAdmin} {
		for _, r := range []Role{RoleViewer, RoleOperator, RoleAdmin} {
			if !role.AtLeast(r) {
				continue
			}
			for _, perm := range matrix[r] {
				out = append(out, Grant{Role: role, Permission: perm})
			}
		}
	}
	return out
}

// Grant is a (role, permission) pair.
type Grant struct {
	Role       Role
	Permission Permission
}

// ErrForbidden is returned when a role lacks a permission.
type ErrForbidden struct {
	Role       Role
	Permission Permission
}

func (e *ErrForbidden) Error() string {
	return "authz: role " + string(e.Role) + " lacks " + string(e.Permission)
}
