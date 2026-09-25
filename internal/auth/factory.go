package auth

import (
	"context"
	"fmt"
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

// readClientCert loads the client keypair the assistant presents to identify
// itself. Empty paths mean "not configured" and return nothing, leaving the
// caller on its bearer token.
//
// Unlike readServiceAccountCreds these reads are NOT best-effort: a path that
// was configured but cannot be read is a deployment error, and continuing would
// silently fall back to a bearer token the control plane rejects — turning a
// clear boot failure into every request 401ing for reasons the logs do not
// explain.
func readClientCert(certPath, keyPath string) (cert, key []byte, err error) {
	if certPath == "" && keyPath == "" {
		return nil, nil, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, nil, fmt.Errorf("auth: client certificate needs both a cert and a key path (cert=%q key=%q)", certPath, keyPath)
	}
	if cert, err = os.ReadFile(certPath); err != nil {
		return nil, nil, fmt.Errorf("auth: read client certificate %q: %w", certPath, err)
	}
	if key, err = os.ReadFile(keyPath); err != nil {
		return nil, nil, fmt.Errorf("auth: read client key %q: %w", keyPath, err)
	}
	return cert, key, nil
}

// NewAuthenticator builds the TokenReview authenticator: a bearer token is
// resolved to an identity by the control plane, which is the only thing that
// can vouch for it. There is no local token store to fall back to.
func NewAuthenticator(_ context.Context, cfg *config.Config, logger *slog.Logger) (Authenticator, error) {
	token, caCert := readServiceAccountCreds(cfg.Auth.TokenReviewTokenPath, cfg.Auth.TokenReviewCACertPath)
	clientCert, clientKey, err := readClientCert(cfg.Auth.TokenReviewClientCertPath, cfg.Auth.TokenReviewClientKeyPath)
	if err != nil {
		return nil, err
	}
	logger.Info("auth.tokenreview", "apiUrl", cfg.Auth.TokenReviewAPIURL, "clientCert", clientCert != nil)
	return NewTokenReviewAuthenticator(TokenReviewConfig{
		APIURL:      cfg.Auth.TokenReviewAPIURL,
		BearerToken: token,
		CACert:      caCert,
		ClientCert:  clientCert,
		ClientKey:   clientKey,
	})
}

// NewAuthorizer builds the SubjectAccessReview authorizer: project access is
// decided by the control plane per request, never from the credential's own
// contents.
func NewAuthorizer(cfg *config.Config, logger *slog.Logger) (Authorizer, error) {
	token, caCert := readServiceAccountCreds(cfg.Auth.SARTokenPath, cfg.Auth.SARCACertPath)
	clientCert, clientKey, err := readClientCert(cfg.Auth.SARClientCertPath, cfg.Auth.SARClientKeyPath)
	if err != nil {
		return nil, err
	}
	logger.Info("authz.sar", "apiUrl", cfg.Auth.SARAPIURL, "clientCert", clientCert != nil)
	return NewSubjectAccessReviewAuthorizer(SARConfig{
		APIURL:      cfg.Auth.SARAPIURL,
		BearerToken: token,
		CACert:      caCert,
		ClientCert:  clientCert,
		ClientKey:   clientKey,
		Logger:      logger,
		Group:       cfg.Auth.SARGroup,
		Resource:    cfg.Auth.SARResource,
		Verb:        cfg.Auth.SARVerb,
	})
}

// ControlPlaneCredentials is the assistant's OWN identity on a control-plane
// call: the client certificate it presents, the CA bundle it verifies the
// server with, and (for completeness) the service-account token.
//
// The certificate is the one that works. Milo validates service-account tokens
// only against its own issuer, so a workload-cluster token is rejected with a
// 401 before the request body is read — see internal/auth/transport.go. The
// token is carried anyway because a plain Kubernetes apiserver (kind, dev,
// e2e) does accept it, and a caller that holds neither simply fails to
// authenticate, which is the correct outcome for a service that cannot prove
// who it is.
type ControlPlaneCredentials struct {
	BearerToken string
	CACert      []byte
	ClientCert  []byte
	ClientKey   []byte
}

// LoadControlPlaneCredentials reads the credential set at the given mount
// paths. Token and CA reads are best-effort (a late-mounted token recovers on
// its own); a configured-but-unreadable client keypair is an error, because
// falling back to a bearer token the control plane rejects turns a clear boot
// failure into every request 401ing for reasons the logs do not explain.
//
// Exported so the CRD capability source is configured from the SAME paths the
// SAR authorizer uses (AUTHZ_SAR_*). Two ways to name one control plane is two
// ways to point half the service at the wrong one.
func LoadControlPlaneCredentials(tokenPath, caCertPath, clientCertPath, clientKeyPath string) (ControlPlaneCredentials, error) {
	token, caCert := readServiceAccountCreds(tokenPath, caCertPath)
	clientCert, clientKey, err := readClientCert(clientCertPath, clientKeyPath)
	if err != nil {
		return ControlPlaneCredentials{}, err
	}
	return ControlPlaneCredentials{
		BearerToken: token,
		CACert:      caCert,
		ClientCert:  clientCert,
		ClientKey:   clientKey,
	}, nil
}
