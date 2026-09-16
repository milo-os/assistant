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

// newConversationsTestCmd is newTestCmd for the conversations subcommands:
// the read-view flags, plus whatever flags the subcommand itself defines.
func newConversationsTestCmd(t *testing.T, define func(*cobra.Command), args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "conversations"}
	cmd.Flags().String("project", "", "")
	cmd.Flags().String("kubeconfig", "", "")
	cmd.Flags().String("output", "table", "")
	if define != nil {
		define(cmd)
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return cmd
}

// --archived must reach the invocation, or `list --archived` silently lists
// the everyday conversations instead of the archive.
func TestConversationsListCarriesArchived(t *testing.T) {
	cmd := newConversationsTestCmd(t, func(c *cobra.Command) { c.Flags().Bool("archived", false, "") },
		"--project", "demo-project", "--archived")
	inv, err := listInvocation(cmd)
	if err != nil {
		t.Fatalf("listInvocation: %v", err)
	}
	if inv.Kind != patchcli.KindConvList || !inv.Archived || inv.Project != "demo-project" {
		t.Errorf("inv = %+v, want an archived list in demo-project", inv)
	}
}

func TestConversationsDeleteCarriesYes(t *testing.T) {
	cmd := newConversationsTestCmd(t, func(c *cobra.Command) { c.Flags().BoolP("yes", "y", false, "") },
		"--project", "demo-project", "-y", "--output", "json")
	inv, err := deleteInvocation(cmd, []string{"ctx-1"})
	if err != nil {
		t.Fatalf("deleteInvocation: %v", err)
	}
	if inv.Kind != patchcli.KindConvDelete || !inv.Yes || inv.ContextID != "ctx-1" || !inv.JSON {
		t.Errorf("inv = %+v, want a confirmed JSON delete of ctx-1", inv)
	}
}

// Without a project there is nothing to archive in, and the error should say
// how to set one rather than reaching the API with an empty namespace.
func TestConversationsArchiveNeedsAProject(t *testing.T) {
	cmd := newConversationsTestCmd(t, nil)
	if _, err := conversationInvocation(cmd, patchcli.KindConvArchive, []string{"ctx-1"}); err == nil ||
		!strings.Contains(err.Error(), "no project set") {
		t.Fatalf("err = %v, want the missing-project guidance", err)
	}
}

// The cobra tree is the plugin's whole surface: pin that the lifecycle
// subcommands and their flags exist where the README says they do.
func TestConversationsLifecycleCommandsAreWired(t *testing.T) {
	conv := newConversationsCmd()
	find := func(name string) *cobra.Command {
		for _, c := range conv.Commands() {
			if c.Name() == name {
				return c
			}
		}
		t.Fatalf("conversations has no %q subcommand", name)
		return nil
	}
	find("archive")
	find("unarchive")
	if f := find("delete").Flags().Lookup("yes"); f == nil || f.Shorthand != "y" {
		t.Errorf("delete --yes/-y flag = %+v", f)
	}
	if find("list").Flags().Lookup("archived") == nil {
		t.Error("list has no --archived flag")
	}
}
