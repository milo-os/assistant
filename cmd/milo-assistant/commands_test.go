package main

import (
	"testing"

	"github.com/spf13/cobra"
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
