// Command assistant-apiserver is the milo aggregated API server that
// exposes resources under the assistant.miloapis.com API group, including
// durable chat conversations as a KRM resource (assistant.miloapis.com/v1alpha1
// Conversations, with a messages subresource) and CapabilityGapReports. It is
// a read view over the shared conversations/messages Postgres tables the A2A
// assistant service writes on the chat hot path.
package main

import (
	"os"

	"github.com/spf13/cobra"
	"k8s.io/component-base/cli"
)

func main() {
	os.Exit(cli.Run(newRootCommand()))
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assistant-apiserver",
		Short: "Aggregated API server exposing assistant conversations as a KRM resource",
	}
	cmd.AddCommand(newServeCommand())
	return cmd
}
