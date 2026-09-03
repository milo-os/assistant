package auth

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
)

// newControlPlaneTransport builds the HTTP transport used for TokenReview and
// SubjectAccessReview calls against the control plane.
//
// caCert verifies the SERVER. clientCert/clientKey prove who WE are.
//
// The two credentials are not interchangeable, and which one the control plane
// accepts is a property of that control plane, not of this service. Milo runs
// with
//
//	--service-account-issuer=https://milo-apiserver.milo-system.svc.cluster.local
//	--client-ca-file=/etc/kubernetes/pki/trust/control-plane/ca.crt
//
// so it validates service-account tokens only against its OWN issuer. A token
// minted by the workload cluster (on GKE the issuer is
// container.googleapis.com/...) is signed by a key Milo has never seen, and is
// rejected with 401 before the TokenReview body is even read. Presenting a
// client certificate signed by the control-plane CA is the supported way for an
// in-cluster service to identify itself; see the assistant's Deployment, which
// mounts one via the cert-manager CSI driver.
//
// Both are optional so that a caller with neither still gets a working
// transport: it will simply fail to authenticate at the control plane, which is
// the correct outcome for a service that cannot prove who it is.
func newControlPlaneTransport(caCert, clientCert, clientKey []byte) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	configured := false

	if len(caCert) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, errors.New("auth: CACert is not valid PEM")
		}
		tlsCfg.RootCAs = pool
		configured = true
	}

	// A half-configured keypair is a deployment mistake, not a degraded mode:
	// fail loudly rather than silently falling back to an anonymous connection
	// that 401s on every request.
	switch {
	case len(clientCert) > 0 && len(clientKey) == 0:
		return nil, errors.New("auth: ClientCert set without ClientKey")
	case len(clientKey) > 0 && len(clientCert) == 0:
		return nil, errors.New("auth: ClientKey set without ClientCert")
	case len(clientCert) > 0:
		pair, err := tls.X509KeyPair(clientCert, clientKey)
		if err != nil {
			return nil, errors.New("auth: client certificate and key are not a valid pair: " + err.Error())
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
		configured = true
	}

	if configured {
		transport.TLSClientConfig = tlsCfg
	}
	return transport, nil
}
