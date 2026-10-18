package authz

// matrix maps each role to the set of permissions it holds.
// A permission granted to a lower role is implicitly held by all higher roles
// (enforced by Check calling r.AtLeast).
//
// This is the single authoritative definition. At daemon start, gvmd upserts
// every row from AllGrants() into authz_permissions. Doctor fails when the
// table diverges.
// matrix lists the permissions each role ADDS over the tier below it (§15.2).
// Inheritance is left to Check, which calls r.AtLeast, so a permission appears
// at exactly the lowest tier that first holds it and no row repeats a lower
// tier's grant.
//
// The operator | admin boundary is the whole security point of this table: the
// moderator role (operator) can run a server but cannot do the irreversible
// things — restore, delete/prune backups, rebuild, apply, adopt, edit content,
// touch secrets — which sit at admin; a prior version of this matrix gave them
// to operator, which is exactly how a trusted moderator could restore over a
// world or prune the backups.
var matrix = map[Role][]Permission{
	RoleViewer: {
		PermInstanceView,
		PermInstancePorts,
		PermInstanceLogs, // console:read
		PermContentView,
		PermBackupView, // backup:list
		PermJobView,
		PermManifestView,
		PermTokenCreate, // token:create.self
		PermSecretView,  // secrets visible only as set/unset
	},
	RoleOperator: {
		PermInstanceStart,
		PermInstanceStop,
		PermInstanceRestart,
		PermInstanceUpdate,
		PermInstanceConsole, // console:write
		PermInstanceControl,
		PermContentSync,
		PermBackupCreate,
		PermJobRun,
	},
	RoleAdmin: {
		PermInstanceCreate,
		PermInstanceDestroy, // instance:delete
		PermInstanceApply,   // instance:config
		PermInstanceInstall,
		PermInstanceRebuild,
		PermContentEdit,
		PermBackupRestore,
		PermBackupPrune, // backup:delete
		PermAdoptDiscover,
		PermAdoptCutover,
		PermAdoptRollback,
		PermAdoptFinalize,
		PermFirewallView,
		PermFirewallEdit, // firewall:*
		PermEdgeView,
		PermEdgeEdit, // caddy:*
		PermUserView,
		PermGrantEdit, // grant:manage
		PermAuditView, // audit:read
		PermDoctorRun,
		PermTokenView,
	},
	RoleOwner: {
		PermUserCreate, // user:manage
		PermUserEdit,
		PermUserDelete,
		PermSecretRotate,
		PermManifestPublish,
		PermManifestApprove,
		PermTokenRevoke, // token:manage.any
		PermDBMigrate,   // host:config
		PermDBReplaySpool,
	},
}

// roleOrder is the tiers from lowest to highest, so Check and AllGrants walk
// them without hard-coding three names that a new role would silently escape.
var roleOrder = []Role{RoleViewer, RoleOperator, RoleAdmin, RoleOwner}

// Check returns nil if role r holds permission p, or ErrForbidden.
func Check(r Role, p Permission) error {
	for _, minRole := range roleOrder {
		for _, perm := range matrix[minRole] {
			if perm == p {
				if r.AtLeast(minRole) {
					return nil
				}
				return &ErrForbidden{Role: r, Permission: p}
			}
		}
	}
	// Unknown permission: treat as owner-only (fail closed).
	if !r.AtLeast(RoleOwner) {
		return &ErrForbidden{Role: r, Permission: p}
	}
	return nil
}

// AllGrants returns all (role, permission) pairs from the matrix, including
// inherited ones, de-duplicated. Used to populate authz_permissions at daemon
// start.
func AllGrants() []Grant {
	var out []Grant
	seen := map[string]bool{}
	for _, role := range roleOrder {
		for _, tier := range roleOrder {
			if !role.AtLeast(tier) {
				continue
			}
			for _, perm := range matrix[tier] {
				k := string(role) + ":" + string(perm)
				if seen[k] {
					continue
				}
				seen[k] = true
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
