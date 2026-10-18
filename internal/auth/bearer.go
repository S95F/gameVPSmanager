package auth

// Bearer-token authentication for the /api/v1 mux (D-40).
//
// This file deliberately contains no cookie read and no reference to Session
// or SessionQuerier: the API mux is built from this path alone, so a future
// handler cannot honour a cookie by accident. TestAPIIgnoresCookies is the
// belt-and-braces check on top of that structure.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/authz"
	"github.com/S95F/gameVPSmanager/internal/identity"
	"github.com/S95F/gameVPSmanager/internal/webapi"
)

// BearerPrefix is the required Authorization scheme for /api/v1.
const BearerPrefix = "Bearer "

// ErrTokenNotFound is returned by TokenQuerier implementations when no row
// matches the presented token's hash.
var ErrTokenNotFound = errors.New("auth: api token not found")

// APIToken is one api_tokens row joined with its user, resolved by hash.
type APIToken struct {
	ID     string
	UserID string
	Name   string

	// RoleCeiling caps what this token may do regardless of how capable its
	// user is (api_tokens.role_ceiling).
	RoleCeiling authz.Role
	// UserRole is the user's global_role at lookup time.
	UserRole authz.Role

	UserDisabled bool
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
}

// TokenQuerier is the minimal store interface bearer auth needs. The lookup
// key is the SHA-256 of the presented plaintext (identity.HashToken), so the
// plaintext never reaches the store layer.
type TokenQuerier interface {
	LookupAPIToken(ctx context.Context, tokenSHA256 []byte) (*APIToken, error)
	// RecordTokenUse stamps api_tokens.last_used_at / last_used_ip. It is
	// best-effort: a failed stamp must not fail the request.
	RecordTokenUse(ctx context.Context, tokenID string, at time.Time, remoteAddr string) error
}

// TokenActor is what a verified bearer token puts in the request context.
// Its Type is the audit actor_t value: an action taken with a token is
// audited as "api_token", never as the user typing in a browser.
type TokenActor struct {
	Type      string // always "api_token"
	TokenID   string
	TokenName string
	UserID    string
	// Role is the effective role: the LOWER of the user's global role and the
	// token's role_ceiling. A viewer-ceiling token held by an admin acts as a
	// viewer.
	Role authz.Role
}

// tokenActorKey is the unexported context key for the token actor.
type tokenActorKey struct{}

// TokenActorFromContext retrieves the TokenActor stored by the bearer
// middleware. The second return value is false when no actor is present.
func TokenActorFromContext(ctx context.Context) (*TokenActor, bool) {
	v, ok := ctx.Value(tokenActorKey{}).(*TokenActor)
	return v, ok && v != nil
}

// BearerMiddleware authenticates /api/v1 requests from the Authorization
// header alone.
type BearerMiddleware struct {
	querier TokenQuerier
	now     func() time.Time
}

// NewBearer returns a BearerMiddleware backed by querier. When querier is nil
// every authenticated route answers 503 degraded: tokens live in Postgres,
// and "the datastore is down" is the honest answer, not 401.
func NewBearer(querier TokenQuerier) *BearerMiddleware {
	return &BearerMiddleware{querier: querier, now: time.Now}
}

// Wrap returns a handler that admits the request only on a live, unexpired,
// unrevoked bearer token whose user is not disabled. Cookies are never read.
func (b *BearerMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, BearerPrefix) {
			unauthorized(w, r, "bearer token required")
			return
		}
		plaintext := strings.TrimPrefix(header, BearerPrefix)

		if b.querier == nil {
			webapi.ServeDegraded(w, r)
			return
		}

		tok, err := b.querier.LookupAPIToken(r.Context(), identity.HashToken(plaintext))
		if err != nil {
			switch {
			case errors.Is(err, ErrStoreUnavailable):
				webapi.ServeDegraded(w, r)
			case errors.Is(err, ErrTokenNotFound):
				unauthorized(w, r, "unknown token")
			default:
				webapi.WriteError(w, r, http.StatusInternalServerError,
					webapi.CodeInternal, "internal error", nil)
			}
			return
		}

		now := b.now()
		switch {
		case tok.RevokedAt != nil:
			unauthorized(w, r, "token revoked")
			return
		case tok.ExpiresAt != nil && tok.ExpiresAt.Before(now):
			unauthorized(w, r, "token expired")
			return
		case tok.UserDisabled:
			webapi.WriteError(w, r, http.StatusForbidden,
				webapi.CodeForbidden, "the token's user is disabled", nil)
			return
		}

		// The token's role ceiling is applied: the actor acts at the lower of
		// the user's role and the ceiling, and a grant can only ever have
		// added on top of global_role, never under the ceiling.
		role := tok.UserRole
		if tok.RoleCeiling.Rank() < role.Rank() {
			role = tok.RoleCeiling
		}

		// Best-effort usage stamp; a failed write must not fail the request.
		_ = b.querier.RecordTokenUse(r.Context(), tok.ID, now, r.RemoteAddr)

		actor := &TokenActor{
			Type:      "api_token",
			TokenID:   tok.ID,
			TokenName: tok.Name,
			UserID:    tok.UserID,
			Role:      role,
		}
		ctx := context.WithValue(r.Context(), tokenActorKey{}, actor)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// unauthorized writes the 401 envelope with the standard challenge header.
func unauthorized(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="gvm"`)
	webapi.WriteError(w, r, http.StatusUnauthorized, webapi.CodeUnauthenticated, message, nil)
}
