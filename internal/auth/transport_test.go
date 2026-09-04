package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKeyPair mints a self-signed client certificate usable for mTLS.
func testKeyPair(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// TestClientCertIsPresented is the regression test for the staging failure that
// motivated client-cert support: the assistant sent a workload-cluster
// service-account token, Milo rejected it 401, and every request failed. The
// server here demands a client certificate the way Milo's --client-ca-file
// does, so a transport that presents none cannot complete a request.
func TestClientCertIsPresented(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t, "agent@assistant.datumapis.com")

	var gotCN string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			gotCN = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	transport, err := newControlPlaneTransport(caPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newControlPlaneTransport: %v", err)
	}
	res, err := (&http.Client{Transport: transport}).Get(srv.URL)
	if err != nil {
		t.Fatalf("request with client cert: %v", err)
	}
	defer res.Body.Close()

	if gotCN != "agent@assistant.datumapis.com" {
		t.Fatalf("server saw CN %q, want agent@assistant.datumapis.com", gotCN)
	}
}

// TestNoClientCertFailsAgainstMTLSServer pins the failure this feature fixes:
// without a keypair the handshake has no identity to offer.
func TestNoClientCertFailsAgainstMTLSServer(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	transport, err := newControlPlaneTransport(caPEM, nil, nil)
	if err != nil {
		t.Fatalf("newControlPlaneTransport: %v", err)
	}
	res, err := (&http.Client{Transport: transport}).Get(srv.URL)
	if err == nil {
		res.Body.Close()
		t.Fatal("request succeeded without a client certificate, want failure")
	}
}

// TestHalfConfiguredKeypairIsRejected: a cert without a key (or vice versa) is a
// deployment mistake. Failing here beats silently connecting anonymously and
// 401ing on every request, which is exactly how the original bug presented.
func TestHalfConfiguredKeypairIsRejected(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t, "x")
	for name, tc := range map[string]struct{ cert, key []byte }{
		"cert without key": {certPEM, nil},
		"key without cert": {nil, keyPEM},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newControlPlaneTransport(nil, tc.cert, tc.key); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestMalformedInputsRejected(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t, "x")
	if _, err := newControlPlaneTransport([]byte("not pem"), nil, nil); err == nil {
		t.Fatal("want error for bad CA PEM")
	}
	// A valid cert paired with an unrelated key must not build a transport.
	_, otherKey := testKeyPair(t, "y")
	if _, err := newControlPlaneTransport(nil, certPEM, otherKey); err == nil {
		t.Fatal("want error for mismatched keypair")
	}
	if _, err := newControlPlaneTransport(nil, []byte("not pem"), keyPEM); err == nil {
		t.Fatal("want error for bad cert PEM")
	}
}

// TestNoCredentialsStillBuilds: a caller with neither CA nor keypair gets a
// working transport on system roots rather than a construction error.
func TestNoCredentialsStillBuilds(t *testing.T) {
	tr, err := newControlPlaneTransport(nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr == nil {
		t.Fatal("nil transport")
	}
	if tr.TLSClientConfig != nil && len(tr.TLSClientConfig.Certificates) > 0 {
		t.Fatal("unexpected client certificates")
	}
}

func TestReadClientCertPathValidation(t *testing.T) {
	if _, _, err := readClientCert("", ""); err != nil {
		t.Fatalf("unset pair should be a no-op, got %v", err)
	}
	if _, _, err := readClientCert("/tmp/cert.pem", ""); err == nil {
		t.Fatal("want error when only the cert path is set")
	}
	_, _, err := readClientCert("/nonexistent/cert.pem", "/nonexistent/key.pem")
	if err == nil || !strings.Contains(err.Error(), "read client certificate") {
		t.Fatalf("want a read error naming the certificate, got %v", err)
	}
}
