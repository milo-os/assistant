package auth

import "context"

type callerTokenKey struct{}

// ContextWithBearerToken stashes the caller's RAW bearer token on ctx.
//
// It is deliberately not a field on [Principal]: a Principal is passed around,
// compared, and logged, and a credential must never ride along with an
// identity. The token is carried separately for the one consumer that needs
// the credential itself rather than the identity it resolved to — forwarding
// it to a provider endpoint that acts as the caller (internal/capability).
// Only an already-authenticated request should have one attached.
func ContextWithBearerToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, callerTokenKey{}, token)
}

// BearerTokenFromContext returns the caller's raw bearer token, or "" when the
// request carried none (any non-HTTP entry point, and every unauthenticated
// path). Callers must treat "" as "no identity to forward", never as an error.
func BearerTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(callerTokenKey{}).(string)
	return token
}
