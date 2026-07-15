package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// OidcAuthenticator verifies a bearer JWT against an issuer's JWKS and checks
// the audience. The signing keys are provided as a [jwk.Set] so the type is
// testable with a locally generated key and no live IdP (which is exactly how
// the contract exercises it — see [NewOidcAuthenticator] vs a static set).
//
// Project authorization reads the granted projects from a JWT claim (default
// `projects`, configurable). The claim may be a string array or a
// space/comma-delimited string. A token with no such claim grants no projects
// (every project check → 403) — the conservative default; wiring OIDC to a real
// entitlement source is a documented follow-up.
type OidcAuthenticator struct {
	issuer        string
	audience      string
	projectsClaim string
	keySet        jwk.Set
}

// OidcOptions configures [NewOidcAuthenticator].
type OidcOptions struct {
	Issuer        string
	Audience      string
	ProjectsClaim string
	// KeySet resolves the JWT signing keys. Production: a cached remote JWKS
	// (see [RemoteJWKS]); tests: a static set built from a local key.
	KeySet jwk.Set
}

// NewOidcAuthenticator returns an [OidcAuthenticator] from the given options.
func NewOidcAuthenticator(opts OidcOptions) *OidcAuthenticator {
	claim := opts.ProjectsClaim
	if claim == "" {
		claim = "projects"
	}
	return &OidcAuthenticator{
		issuer:        opts.Issuer,
		audience:      opts.Audience,
		projectsClaim: claim,
		keySet:        opts.KeySet,
	}
}

// Authenticate verifies the token's signature, issuer, and audience, then
// resolves it to a [Principal]. Any verification failure yields a 401 [*Error].
func (a *OidcAuthenticator) Authenticate(ctx context.Context, bearerToken string) (Principal, error) {
	tok, err := jwt.Parse([]byte(bearerToken),
		jwt.WithKeySet(a.keySet, jws.WithInferAlgorithmFromKey(true)),
		jwt.WithValidate(false),
	)
	if err != nil {
		return Principal{}, Unauthenticated(fmt.Sprintf("JWT verification failed: %v", err))
	}
	if err := jwt.Validate(tok, jwt.WithIssuer(a.issuer), jwt.WithAudience(a.audience)); err != nil {
		return Principal{}, Unauthenticated(fmt.Sprintf("JWT verification failed: %v", err))
	}

	subject := tok.Subject()
	if subject == "" {
		return Principal{}, Unauthenticated(`JWT is missing a "sub" claim`)
	}

	claim, _ := tok.Get(a.projectsClaim)
	return PrincipalFromProjects(subject, extractProjects(claim)), nil
}

// RemoteJWKS builds a cached remote JWKS key set. When jwksURI is empty it
// defaults to `${issuer}/.well-known/jwks.json`; override for IdPs that publish
// keys elsewhere (e.g. Keycloak's /protocol/openid-connect/certs). The cache
// refreshes in the background for the lifetime of ctx.
func RemoteJWKS(ctx context.Context, issuer, jwksURI string) (jwk.Set, error) {
	uri := jwksURI
	if uri == "" {
		uri = strings.TrimRight(issuer, "/") + "/.well-known/jwks.json"
	}
	cache := jwk.NewCache(ctx)
	if err := cache.Register(uri); err != nil {
		return nil, fmt.Errorf("register jwks %q: %w", uri, err)
	}
	// Prime the cache so a bad JWKS URL fails at boot, not on first request.
	if _, err := cache.Refresh(ctx, uri); err != nil {
		return nil, fmt.Errorf("fetch jwks %q: %w", uri, err)
	}
	return jwk.NewCachedSet(cache, uri), nil
}

// extractProjects normalizes a projects claim into a slice: a JSON array of
// strings, or a single space/comma-delimited string. Anything else → empty.
func extractProjects(claim any) []string {
	switch v := claim.(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		var out []string
		for _, s := range strings.FieldsFunc(v, func(r rune) bool {
			return r == ' ' || r == ',' || r == '\t' || r == '\n' || r == '\r'
		}) {
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}
