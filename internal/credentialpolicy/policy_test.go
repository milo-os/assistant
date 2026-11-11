package credentialpolicy

import (
	"strings"
	"testing"
)

func TestCheckURL(t *testing.T) {
	cases := []struct {
		name          string
		allowInsecure bool
		url           string
		wantErr       bool
		wantHost      string
	}{
		{name: "https is allowed", url: "https://gateway.svc.cluster.local/mcp"},
		{name: "http is refused", url: "http://gateway.svc.cluster.local/mcp", wantErr: true, wantHost: "gateway.svc.cluster.local"},
		{name: "http is allowed by the escape hatch", allowInsecure: true, url: "http://gateway.svc.cluster.local/mcp"},
		{name: "no scheme is refused", url: "gateway.svc.cluster.local/mcp", wantErr: true},
		{name: "unparseable is refused", url: "http://[::1/mcp", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (Policy{AllowInsecure: tc.allowInsecure}).CheckURL(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected refusal for %q", tc.url)
				}
				if strings.Contains(err.Error(), "Bearer") || strings.Contains(err.Error(), "token=") {
					t.Fatalf("refusal leaked a credential: %v", err)
				}
				if tc.wantHost != "" && !strings.Contains(err.Error(), tc.wantHost) {
					t.Fatalf("refusal %q does not name host %q", err, tc.wantHost)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal for %q: %v", tc.url, err)
			}
		})
	}
}

func TestCheckConfigured(t *testing.T) {
	cases := []struct {
		name          string
		allowInsecure bool
		url           string
		wantErr       bool
	}{
		{name: "empty is unset", url: ""},
		{name: "https is allowed", url: "https://gateway/v1"},
		{name: "http is refused", url: "http://gateway/v1", wantErr: true},
		{name: "http is allowed by the escape hatch", allowInsecure: true, url: "http://gateway/v1"},
		{name: "bare host is refused", url: "gateway.svc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (Policy{AllowInsecure: tc.allowInsecure}).CheckConfigured("GATEWAY_URL", tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected refusal for %q", tc.url)
				}
				if !strings.Contains(err.Error(), "GATEWAY_URL") {
					t.Fatalf("refusal %q does not name the variable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal for %q: %v", tc.url, err)
			}
		})
	}
}
