package auth

import (
	"context"
	"testing"

	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
)

// TestNewAuthenticator_TokenReview pins the config→constructor wiring: with
// AUTH_MODE=tokenreview and an explicit endpoint, NewAuthenticator builds the
// TokenReview authenticator (no live cluster needed — construction does not dial).
func TestNewAuthenticator_TokenReview(t *testing.T) {
	cfg, err := config.Load(config.MapGetenv(map[string]string{
		"AUTH_MODE":                 "tokenreview",
		"AUTHN_TOKENREVIEW_API_URL": "https://kubernetes.default.svc",
	}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	an, err := NewAuthenticator(context.Background(), cfg, logger.Silent())
	if err != nil {
		t.Fatalf("NewAuthenticator(tokenreview): %v", err)
	}
	if _, ok := an.(*tokenReviewAuthenticator); !ok {
		t.Fatalf("expected *tokenReviewAuthenticator, got %T", an)
	}
}

// TestNewAuthenticator_Dev pins the default branch: AUTH_MODE unset ⇒ the
// static dev-token authenticator.
func TestNewAuthenticator_Dev(t *testing.T) {
	cfg, err := config.Load(config.MapGetenv(map[string]string{"AUTH_DEV_TOKENS": "t:s:*"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	an, err := NewAuthenticator(context.Background(), cfg, logger.Silent())
	if err != nil {
		t.Fatalf("NewAuthenticator(dev): %v", err)
	}
	if _, ok := an.(*DevAuthenticator); !ok {
		t.Fatalf("expected *DevAuthenticator, got %T", an)
	}
}
