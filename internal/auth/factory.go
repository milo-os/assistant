package auth

import (
	"context"
	"log/slog"
	"os"

	"github.com/milo-os/assistant/internal/config"
)

// NewAuthenticator selects the authenticator from config. In oidc mode it
// primes a cached remote JWKS from the issuer (failing at boot on a bad URL); in
// tokenreview mode it builds a control-plane TokenReview client (the assistant's
// own service-account token/CA, read from the mounted paths); otherwise it uses
// the static dev-token map.
func NewAuthenticator(ctx context.Context, cfg *config.Config, logger *slog.Logger) (Authenticator, error) {
	switch cfg.Auth.Mode {
	case config.AuthModeOIDC:
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
	case config.AuthModeTokenReview:
		// Best-effort read of the mounted service-account credentials: absent
		// files leave the reviewer on system roots / no bearer token, which the
		// TokenReview call surfaces as a fail-closed 401 rather than a boot
		// failure (mirrors selectAuthorizer's SAR credential handling).
		token, _ := os.ReadFile(cfg.Auth.TokenReviewTokenPath)
		caCert, _ := os.ReadFile(cfg.Auth.TokenReviewCACertPath)
		logger.Info("auth.mode.tokenreview", "apiUrl", cfg.Auth.TokenReviewAPIURL)
		return NewTokenReviewAuthenticator(TokenReviewConfig{
			APIURL:      cfg.Auth.TokenReviewAPIURL,
			BearerToken: string(token),
			CACert:      caCert,
		})
	}
	dev := NewDevAuthenticator(cfg.Auth.DevTokens)
	logger.Info("auth.mode.dev", "tokenCount", dev.Size())
	return dev, nil
}

// NewAuthorizer selects the authorizer. v0 always uses the credential-carried
// grants ([ClaimsAuthorizer]) for both auth modes. Production branches to the
// SAR-based authorizer from [NewSubjectAccessReviewAuthorizer] (wired by the
// service shell from AUTHZ_MODE=sar) without touching any call site — the
// [Authorizer] interface is the seam.
func NewAuthorizer(_ *config.Config, logger *slog.Logger) Authorizer {
	logger.Info("authz.mode", "type", "claims")
	return ClaimsAuthorizer{}
}
