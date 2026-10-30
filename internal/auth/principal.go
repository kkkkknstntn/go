package auth

import (
	"context"
	"slices"

	"awesomeProject/internal/domain"
)

type Principal struct {
	UserID string
	Role   domain.Role
}

func (p Principal) HasRole(roles ...domain.Role) bool { return slices.Contains(roles, p.Role) }

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
