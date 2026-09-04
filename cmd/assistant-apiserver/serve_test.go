package main

import (
	"testing"

	"k8s.io/apiserver/pkg/server/options"
)

// validate guards the one hard requirement to start: a conversation-store DSN.
// Everything else (auth, serving) is defaulted by RecommendedOptions.
func TestServerOptionsValidate(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"has dsn", "postgres://u:p@h:5432/db", false},
		{"empty dsn", "", true},
		{"whitespace-only dsn", "   ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &serverOptions{PostgresDSN: tc.dsn}
			err := o.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// Milo's aggregator sends the caller's UID in X-Remote-Uid, and this server
// only reads it when --requestheader-uid-headers names that header — which the
// generic apiserver accepts only while the RemoteRequestHeaderUID gate is on.
// It is Beta/default-on from 1.33, so nothing sets it explicitly; this asserts
// that stays true, because the failure it causes is both total and silent from
// here. A caller whose UID we drop produces a SubjectAccessReview with no UID,
// Milo's OpenFGA authorizer answers that with an error rather than a decision,
// and every request we serve is denied — logged only in another cluster's
// webhook, and indistinguishable from a missing IAM grant.
func TestRequestHeaderUIDHeadersAccepted(t *testing.T) {
	opts := options.NewDelegatingAuthenticationOptions()
	opts.RequestHeader.ClientCAFile = "/etc/kubernetes/pki/trust/ca.crt"
	opts.RequestHeader.UIDHeaders = []string{"X-Remote-Uid"}

	if errs := opts.RequestHeader.Validate(); len(errs) > 0 {
		t.Fatalf("--requestheader-uid-headers rejected, so the caller's UID would "+
			"be dropped and Milo would deny every request: %v", errs)
	}
}
