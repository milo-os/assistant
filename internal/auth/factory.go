package auth

import (
	"context"
	"log/slog"

	"github.com/milo-os/assistant/internal/config"
)

// NewAuthenticator selects the authenticator from config. In oidc mode it
// primes a cached remote JWKS from the issuer (failing at boot on a bad URL);
// otherwise it uses the static dev-token map.
func NewAuthenticator(ctx context.Context, cfg *config.Config, logger *slog.Logger) (Authenticator, error) {
	if cfg.Auth.Mode == config.AuthModeOIDC {
		keySet, err := RemoteJWKS(ctx, cfg.Auth.OIDCIssuer, "")
		if err != nil {
			return nil, err
		}
		logger.Info("auth.mode.oidc", "issuer", cfg.Auth.OIDCIssuer, "audience", cfg.Auth.OIDCAudience)
		return NewOidcAuthenticator(OidcOptions{
			Issuer:        cfg.Auth.OIDCIssuer,
			Audience:      cfg.Auth.OIDCAudience,
			ProjectsClaim: cfg.Auth.OIDCProjectsClaim,
			KeySet:        keySet,
		}), nil
	}
	dev := NewDevAuthenticator(cfg.Auth.DevTokens)
	logger.Info("auth.mode.dev", "tokenCount", dev.Size())
	return dev, nil
}

// NewAuthorizer selects the authorizer. v0 always uses the credential-carried
// grants ([ClaimsAuthorizer]) for both auth modes. Production would branch here
// to a [SubjectAccessReviewAuthorizer] without touching any call site — the
// [Authorizer] interface is the seam.
func NewAuthorizer(_ *config.Config, logger *slog.Logger) Authorizer {
	logger.Info("authz.mode", "type", "claims")
	return ClaimsAuthorizer{}
}
