package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/milo-os/assistant/internal/patchcli"
)

// newTestCmd registers the persistent flags serviceInvocation reads, with --url
// set so it never attempts endpoint discovery.
func newTestCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "card"}
	cmd.Flags().String("url", "http://assistant.test", "")
	cmd.Flags().String("project", "", "")
	cmd.Flags().String("kubeconfig", "", "")
	cmd.Flags().String("output", "table", "")
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return cmd
}

// `card --project` must reach Execute with Project set, or the extended-card
// branch never runs and the command silently returns the PUBLIC card — a
// success exit code and plausible output for the wrong request. needProject is
// false for card (a bare `card` works with no project), so the flag has to be
// carried explicitly; this pins that.
func TestCardCarriesProject(t *testing.T) {
	cmd := newTestCmd(t, "--project", "demo-project")
	inv, err := cardInvocation(cmd)
	if err != nil {
		t.Fatalf("cardInvocation: %v", err)
	}
	if inv.Project != "demo-project" {
		t.Errorf("Project = %q, want %q — card would request the public card", inv.Project, "demo-project")
	}
}

// Without --project the card command must still resolve: the public card needs
// no project, and erroring here would break `datumctl assistant card`.
func TestCardWithoutProjectStillResolves(t *testing.T) {
	cmd := newTestCmd(t)
	inv, err := cardInvocation(cmd)
	if err != nil {
		t.Fatalf("cardInvocation with no project: %v", err)
	}
	if inv.Project != "" {
		t.Errorf("Project = %q, want empty", inv.Project)
	}
}

// Offline, datumctl's credentials helper fails with a DNS error buried under
// the SDK's "credentials helper failed: exit status 1\nstderr: ..." wrapper.
// The user should read that they could not connect, not a process exit status.
func TestHelperErrorOfflineReadsAsUnreachable(t *testing.T) {
	err := helperError(errors.New("credentials helper failed: exit status 1\n" +
		`stderr: error: failed to get token: Post "https://auth.staging.env.datum.net/oauth/v2/token": ` +
		"dial tcp: lookup auth.staging.env.datum.net: no such host\n"))

	got := err.Error()
	want := "could not connect: lookup auth.staging.env.datum.net: no such host\n" +
		"       check your network connection and try again"
	if got != want {
		t.Errorf("helperError =\n%q\nwant\n%q", got, want)
	}
}

// Any other helper failure keeps the helper's own words and only sheds the
// exit-status wrapper.
func TestHelperErrorKeepsHelperMessage(t *testing.T) {
	err := helperError(errors.New("credentials helper failed: exit status 1\n" +
		"stderr: error: failed to get token: no active session; run 'datumctl auth login'\n"))

	got := err.Error()
	want := "datumctl could not get a token: no active session; run 'datumctl auth login'"
	if got != want {
		t.Errorf("helperError = %q, want %q", got, want)
	}
	if !errors.As(err, new(credentialsError)) {
		t.Errorf("helperError should return a credentialsError, got %T", err)
	}
}

// A helper failure in an unexpected shape passes through untouched rather
// than being mangled by the stderr parsing.
func TestHelperErrorUnknownShapePassesThrough(t *testing.T) {
	err := helperError(errors.New("DATUM_CREDENTIALS_HELPER is not set; is this plugin running via datumctl?"))
	if got := err.Error(); !strings.Contains(got, "DATUM_CREDENTIALS_HELPER is not set") {
		t.Errorf("helperError = %q, want the original message", got)
	}
}

// Discovery asks for a token on the way to the endpoint. When that is what
// fails, the user is offline or signed out, and the message must say so
// plainly: not "could not discover the assistant", and no --url advice.
func TestServiceURLOfflineDoesNotBlameDiscovery(t *testing.T) {
	cmd := newTestCmd(t, "--url", "", "--project", "demo-project")
	inv := patchcli.Invocation{
		Project: "demo-project",
		APIHost: "api.test",
		Token: func() (string, error) {
			return "", helperError(errors.New("credentials helper failed: exit status 1\n" +
				`stderr: error: failed to get token: Post "https://auth.test/oauth/v2/token": ` +
				"dial tcp: lookup auth.test: no such host\n"))
		},
	}

	_, err := serviceURL(cmd, inv)
	if err == nil {
		t.Fatal("serviceURL should fail when no token can be minted")
	}
	got := err.Error()
	if !strings.HasPrefix(got, "could not connect: lookup auth.test: no such host") {
		t.Errorf("serviceURL error = %q, want it to lead with the network failure", got)
	}
	for _, noise := range []string{"could not discover", "--url", "PATCH_URL", "exit status", "stderr:"} {
		if strings.Contains(got, noise) {
			t.Errorf("serviceURL error should not mention %q: %q", noise, got)
		}
	}
}
