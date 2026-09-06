package auth

import "context"

type callerTokenKey struct{}

// ContextWithBearerToken stashes the caller's RAW bearer token on ctx. Only an
// already-authenticated request should have one attached.
//
// Deliberately not a field on [Principal]: a Principal is passed around,
// compared, and logged, and a credential must never ride along with an
// identity.
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
