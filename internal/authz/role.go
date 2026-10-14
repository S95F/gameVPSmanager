// Package authz defines the permission model for gvm.
//
// The three roles form a strict hierarchy: viewer < operator < admin. The
// permission matrix is the single authoritative definition of what each role
// may do, compiled into the binary and mirrored to authz_permissions at
// daemon start. Doctor fails if the table and the matrix disagree.
package authz

// Role is a user's capability level, matching the role_t Postgres enum.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// ParseRole returns the Role for s, or an error if s is not a valid role.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleViewer, RoleOperator, RoleAdmin:
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
	default:
		return -1
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
