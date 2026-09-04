package auth

import (
	"context"
	"log/slog"
	"os"

	"github.com/milo-os/assistant/internal/config"
)

// readServiceAccountCreds loads the assistant's own identity for a
// control-plane call.
//
// The reads are best-effort on purpose: an absent file leaves the caller on
// system roots with no bearer token, which the control plane answers with a
// rejection. That is the correct outcome — a service that cannot prove who it
// is must not authenticate or authorize anyone — and it surfaces at the first
// request rather than as a boot crash, so a pod with a late-mounted token
// recovers on its own instead of crash-looping.
func readServiceAccountCreds(tokenPath, caCertPath string) (token string, caCert []byte) {
	t, _ := os.ReadFile(tokenPath)
	ca, _ := os.ReadFile(caCertPath)
	return string(t), ca
}

// NewAuthenticator builds the TokenReview authenticator: a bearer token is
// resolved to an identity by the control plane, which is the only thing that
// can vouch for it. There is no local token store to fall back to.
func NewAuthenticator(_ context.Context, cfg *config.Config, logger *slog.Logger) (Authenticator, error) {
	token, caCert := readServiceAccountCreds(cfg.Auth.TokenReviewTokenPath, cfg.Auth.TokenReviewCACertPath)
	logger.Info("auth.tokenreview", "apiUrl", cfg.Auth.TokenReviewAPIURL)
	return NewTokenReviewAuthenticator(TokenReviewConfig{
		APIURL:      cfg.Auth.TokenReviewAPIURL,
		BearerToken: token,
		CACert:      caCert,
	})
}

// NewAuthorizer builds the SubjectAccessReview authorizer: project access is
// decided by the control plane per request, never from the credential's own
// contents.
func NewAuthorizer(cfg *config.Config, logger *slog.Logger) (Authorizer, error) {
	token, caCert := readServiceAccountCreds(cfg.Auth.SARTokenPath, cfg.Auth.SARCACertPath)
	logger.Info("authz.sar", "apiUrl", cfg.Auth.SARAPIURL)
	return NewSubjectAccessReviewAuthorizer(SARConfig{
		APIURL:      cfg.Auth.SARAPIURL,
		BearerToken: token,
		CACert:      caCert,
		Group:       cfg.Auth.SARGroup,
		Resource:    cfg.Auth.SARResource,
		Verb:        cfg.Auth.SARVerb,
	})
}
