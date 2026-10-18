// Package authz defines the permission model for gvm.
//
// The roles form a strict, cumulative hierarchy (§15.2): viewer ⊂ operator ⊂
// admin ⊂ owner. Each tier ADDS to the one below, so a higher role holds every
// permission a lower one does. The permission matrix is the single
// authoritative definition of what each role may do, compiled into the binary
// and mirrored to authz_permissions at daemon start. Doctor fails if the table
// and the matrix disagree.
//
// The dividing line that matters most is operator | admin: an operator is the
// natural moderator role, so it may start, stop, restart and update a server
// and sync its content, but it may NOT do the things there is no undo for —
// restore over a world, delete or prune backups, rebuild, apply config, adopt,
// edit the mod list, or touch secrets. Those are admin, and the few truly
// host-shaping actions (managing users, rotating secrets, publishing manifests,
// host config) are owner.
package authz

// Role is a user's capability level, matching the role_t Postgres enum.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
	RoleOwner    Role = "owner"
)

// ParseRole returns the Role for s, or an error if s is not a valid role.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleViewer, RoleOperator, RoleAdmin, RoleOwner:
		return Role(s), nil
	default:
		return "", &ErrUnknownRole{Name: s}
	}
}

// Rank returns the numeric rank of the role (higher = more capable).
func (r Role) Rank() int {
	switch r {
	case RoleViewer:
		return 0
	case RoleOperator:
		return 1
	case RoleAdmin:
		return 2
	case RoleOwner:
		return 3
	default:
		return -1
	}
}

// CanGrant reports whether a granter may assign target to another user (§15.2):
// an owner may grant any role, and an admin may grant only operator or viewer.
// Anyone below admin may grant nothing. It never lets a granter assign a role
// at or above their own that admin could use to escalate: admin is capped at
// operator.
func CanGrant(granter, target Role) bool {
	if target.Rank() < 0 {
		return false
	}
	switch granter {
	case RoleOwner:
		return true
	case RoleAdmin:
		return target == RoleViewer || target == RoleOperator
	default:
		return false
	}
}

// AtLeast reports whether r is at least as capable as min.
func (r Role) AtLeast(min Role) bool { return r.Rank() >= min.Rank() }

// Greatest returns the higher of two roles.
func Greatest(a, b Role) Role {
	if a.Rank() >= b.Rank() {
		return a
	}
	return b
}

type ErrUnknownRole struct{ Name string }

func (e *ErrUnknownRole) Error() string { return "authz: unknown role " + e.Name }
