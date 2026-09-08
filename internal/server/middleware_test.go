package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// rawRPC posts body verbatim, bypassing json.Marshal, so a test can send the
// not-quite-JSON that a parser-differential attack depends on.
func rawRPC(t *testing.T, srv *httptest.Server, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/a2a", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestTrailingBytesCannotSkipAuthorization pins the fix for a parser
// disagreement that was a full authorization bypass: the middleware peeked with
// json.Unmarshal (which rejects trailing bytes) and shrugged the error off,
// while a2a-go dispatches with json.Decoder (which ignores them). One junk byte
// meant the request ran unauthorized. Every method below is one a caller could
// reach that way, so each stays denied.
func TestTrailingBytesCannotSkipAuthorization(t *testing.T) {
	sendParams := `{"message":{"role":"user","parts":[{"text":"hi"}],"metadata":{"projectName":"` + project + `"}}}`

	tests := []struct {
		name string
		body string
	}{
		{
			// bob holds other-project only; without the trailing byte this is a 403.
			name: "extended card for an ungranted project",
			body: `{"jsonrpc":"2.0","id":"x","method":"GetExtendedAgentCard","params":{"tenant":"` + project + `"}}x`,
		},
		{
			// Deny-by-default must survive too, not just the project checks.
			name: "deny-by-default method",
			body: `{"jsonrpc":"2.0","id":"x","method":"ListTasks","params":{}}x`,
		},
		{
			name: "chat turn against an ungranted project",
			body: `{"jsonrpc":"2.0","id":"x","method":"SendMessage","params":` + sendParams + `}x`,
		},
		{
			name: "trailing object rather than a bare byte",
			body: `{"jsonrpc":"2.0","id":"x","method":"ListTasks","params":{}} {}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			res := rawRPC(t, srv, wrongToken, tc.body)
			defer res.Body.Close()
			if res.StatusCode == http.StatusOK {
				t.Fatalf("status = 200: trailing bytes skipped authorization")
			}
		})
	}
}

// TestTrailingBytesDoNotReachTheAdvertiser is the disclosure half of the bypass:
// a 200 would have handed back another project's service names, tool names and
// MCP endpoints, so pin that neither the authorizer nor the card producer is
// ever consulted.
func TestTrailingBytesDoNotReachTheAdvertiser(t *testing.T) {
	adv := newFixtureAdvertiser(t)
	authz := &recordingAuthorizer{inner: stubAuthorizer{"bob": {"other-project"}}}
	srv := newCardServer(t, adv, authz)

	res := rawRPC(t, srv, wrongToken,
		`{"jsonrpc":"2.0","id":"x","method":"GetExtendedAgentCard","params":{"tenant":"`+project+`"}}x`)
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if got := adv.projects(); len(got) != 0 {
		t.Errorf("advertiser consulted: %v", got)
	}
	if got := authz.seen(); len(got) != 0 {
		t.Errorf("authorizer consulted: %v", got)
	}
}

// TestMalformedBodyRejected: a body the middleware cannot peek is now a 400
// here rather than being handed down for the JSON-RPC layer to report, because
// passing it down means dispatching something we failed to authorize.
func TestMalformedBodyRejected(t *testing.T) {
	srv := newTestServer(t)
	res := rawRPC(t, srv, goodToken, `{"jsonrpc":"2.0","id":"x","method":`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// TestTrailingWhitespaceStillAccepted guards the fix from over-reaching:
// whitespace after the request object is ordinary JSON, not an attack.
func TestTrailingWhitespaceStillAccepted(t *testing.T) {
	srv := newTestServer(t)
	res := rawRPC(t, srv, goodToken,
		`{"jsonrpc":"2.0","id":"x","method":"GetExtendedAgentCard","params":{"tenant":"`+project+`"}}`+"\n\t ")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}
