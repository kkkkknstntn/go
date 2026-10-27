// Package auth handles accounts, passwords and JWT sessions.
package auth

import (
	"context"
	"slices"

	"awesomeProject/internal/domain"
)

// Principal is the caller, as stated by a verified access token.
type Principal struct {
	UserID string
	Role   domain.Role
}

func (p Principal) HasRole(roles ...domain.Role) bool { return slices.Contains(roles, p.Role) }

// CanAccess reports whether p may see an object owned by ownerID: its owner
// and admins may.
func (p Principal) CanAccess(ownerID string) bool {
	return p.UserID == ownerID || p.Role == domain.RoleAdmin
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
