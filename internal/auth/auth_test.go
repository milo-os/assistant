package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// ── ExtractBearerToken ────────────────────────────────────────

func TestExtractBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":   "abc",
		"bearer abc":   "abc", // case-insensitive scheme
		"Bearer  abc ": "abc", // trimmed
		"":             "",
		"Basic abc":    "",
		"abc":          "",
	}
	for in, want := range cases {
		if got := ExtractBearerToken(in); got != want {
			t.Errorf("ExtractBearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── Dev authenticator ─────────────────────────────────────────

func TestParseDevTokens(t *testing.T) {
	m := ParseDevTokens("good:alice:projA,projB;wild:bob:*;  ; bad-entry ;t3:carol:")
	if len(m) != 3 {
		t.Fatalf("parsed %d tokens, want 3: %+v", len(m), m)
	}
	if g := m["good"]; g.subject != "alice" || len(g.projects) != 2 {
		t.Errorf("good grant = %+v", g)
	}
	if g := m["wild"]; g.subject != "bob" || g.projects[0] != "*" {
		t.Errorf("wild grant = %+v", g)
	}
	if g := m["t3"]; g.subject != "carol" || len(g.projects) != 0 {
		t.Errorf("t3 grant = %+v (empty project list allowed)", g)
	}
}

func TestDevAuthenticator(t *testing.T) {
	a := NewDevAuthenticator("good:alice:projA,projB;wild:bob:*")
	if a.Size() != 2 {
		t.Fatalf("size = %d", a.Size())
	}

	p, err := a.Authenticate(context.Background(), "good")
	if err != nil {
		t.Fatalf("good token: %v", err)
	}
	if p.Subject != "alice" || p.GrantAll || len(p.Projects) != 2 {
		t.Errorf("principal = %+v", p)
	}

	p, err = a.Authenticate(context.Background(), "wild")
	if err != nil || !p.GrantAll {
		t.Errorf("wildcard principal = %+v err=%v", p, err)
	}

	_, err = a.Authenticate(context.Background(), "unknown")
	assertStatus(t, err, 401)
}

// ── Authorizers ───────────────────────────────────────────────

func TestClaimsAuthorizer(t *testing.T) {
	az := ClaimsAuthorizer{}
	ctx := context.Background()

	if err := az.AuthorizeProject(ctx, PrincipalFromProjects("s", []string{"projA"}), "projA"); err != nil {
		t.Errorf("granted project should allow: %v", err)
	}
	if err := az.AuthorizeProject(ctx, PrincipalFromProjects("s", []string{"*"}), "anything"); err != nil {
		t.Errorf("wildcard should allow: %v", err)
	}
	err := az.AuthorizeProject(ctx, PrincipalFromProjects("s", []string{"projA"}), "projB")
	assertStatus(t, err, 403)
}

func TestSubjectAccessReviewAuthorizer_FailsClosed(t *testing.T) {
	az := SubjectAccessReviewAuthorizer{APIURL: "http://milo"}
	err := az.AuthorizeProject(context.Background(), PrincipalFromProjects("s", []string{"*"}), "projA")
	assertStatus(t, err, 403) // unimplemented stub denies even a wildcard grant
}

// ── OIDC authenticator ────────────────────────────────────────

const (
	testIssuer   = "https://issuer.test"
	testAudience = "assistant"
)

func newSignedToken(t *testing.T, key *rsa.PrivateKey, iss, aud, sub string, projects any) string {
	t.Helper()
	b := jwt.NewBuilder().Issuer(iss).Audience([]string{aud})
	if sub != "" {
		b = b.Subject(sub)
	}
	if projects != nil {
		b = b.Claim("projects", projects)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := jwk.FromRaw(key)
	if err != nil {
		t.Fatal(err)
	}
	_ = priv.Set(jwk.KeyIDKey, "k1")
	_ = priv.Set(jwk.AlgorithmKey, jwa.RS256)
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, priv))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

func publicSet(t *testing.T, key *rsa.PrivateKey) jwk.Set {
	t.Helper()
	pub, err := jwk.FromRaw(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub.Set(jwk.KeyIDKey, "k1")
	_ = pub.Set(jwk.AlgorithmKey, jwa.RS256)
	set := jwk.NewSet()
	if err := set.AddKey(pub); err != nil {
		t.Fatal(err)
	}
	return set
}

func newOidc(t *testing.T, key *rsa.PrivateKey) *OidcAuthenticator {
	return NewOidcAuthenticator(OidcOptions{
		Issuer: testIssuer, Audience: testAudience, KeySet: publicSet(t, key),
	})
}

func TestOidc_ValidTokenWithProjectArray(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	a := newOidc(t, key)
	tokStr := newSignedToken(t, key, testIssuer, testAudience, "user-1", []string{"projA", "projB"})

	p, err := a.Authenticate(context.Background(), tokStr)
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if p.Subject != "user-1" || len(p.Projects) != 2 {
		t.Errorf("principal = %+v", p)
	}
}

func TestOidc_ProjectsClaimAsDelimitedString(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	a := newOidc(t, key)
	tokStr := newSignedToken(t, key, testIssuer, testAudience, "user-1", "projA projB,projC")

	p, err := a.Authenticate(context.Background(), tokStr)
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if len(p.Projects) != 3 {
		t.Errorf("projects = %v, want 3", p.Projects)
	}
}

func TestOidc_NoProjectsClaimGrantsNothing(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	a := newOidc(t, key)
	tokStr := newSignedToken(t, key, testIssuer, testAudience, "user-1", nil)

	p, err := a.Authenticate(context.Background(), tokStr)
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if p.GrantAll || len(p.Projects) != 0 {
		t.Errorf("expected no grants, got %+v", p)
	}
}

func TestOidc_RejectsWrongIssuerAudienceAndSignature(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	a := newOidc(t, key)
	ctx := context.Background()

	// Wrong issuer.
	assertStatus(t, mustErr(a.Authenticate(ctx, newSignedToken(t, key, "https://evil", testAudience, "u", nil))), 401)
	// Wrong audience.
	assertStatus(t, mustErr(a.Authenticate(ctx, newSignedToken(t, key, testIssuer, "other-aud", "u", nil))), 401)
	// Signed by a key not in the set.
	assertStatus(t, mustErr(a.Authenticate(ctx, newSignedToken(t, other, testIssuer, testAudience, "u", nil))), 401)
	// Garbage token.
	assertStatus(t, mustErr(a.Authenticate(ctx, "not-a-jwt")), 401)
}

func TestOidc_MissingSubject(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	a := newOidc(t, key)
	tokStr := newSignedToken(t, key, testIssuer, testAudience, "", nil)
	assertStatus(t, mustErr(a.Authenticate(context.Background(), tokStr)), 401)
}

// ── helpers ───────────────────────────────────────────────────

func mustErr[T any](_ T, err error) error { return err }

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	authErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is not *auth.Error: %v", err)
	}
	if authErr.Status != want {
		t.Errorf("status = %d, want %d (msg: %s)", authErr.Status, want, authErr.Message)
	}
}
