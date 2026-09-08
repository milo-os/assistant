// Command milo-patch is the datumctl plugin form of the `patch` CLI: it
// makes the Datum Cloud assistant reachable as `datumctl patch`, sharing every
// line of client code with the standalone binary (internal/patchcli) and
// differing only in where the project and the bearer token come from.
//
// datumctl dispatches plugins with syscall.Exec on Unix — the plugin replaces
// the datumctl process image and inherits its stdio and controlling terminal —
// so the full-screen `chat --tui` works here exactly as it does standalone.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.datum.net/datumctl/plugin"

	"github.com/milo-os/assistant/internal/patchcli"
)

// version is stamped at build time with -ldflags "-X main.version=v1.2.3". It
// is reported through the --plugin-manifest handshake and nowhere else.
var version = "dev"

func main() {
	// First statement, before anything that could fail or write to stdout:
	// datumctl probes the manifest with a stripped environment (PATH, HOME and
	// the temp dirs only) under a short timeout, and treats a non-zero exit or
	// any leading output as "this binary has no manifest".
	plugin.ServeManifest(plugin.Manifest{
		Name:          "patch",
		Version:       version,
		Description:   "Chat with Patch, the Datum Cloud assistant",
		APIVersion:    1,
		MinAPIVersion: 1,
	})

	// Authentication failures should point at datumctl, not at the standalone
	// CLI's PATCH_TOKEN, which plays no part here.
	patchcli.AuthHint = "run 'datumctl auth login' if your session has expired"

	// The SDK's root command carries no context, so cmd.Context() would be
	// context.Background() and Ctrl-C would cancel nothing — a streaming chat
	// turn would keep reading until the server hung up. Wire the signal
	// context in before Execute so cancellation reaches the a2a client.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := newRootCmd()
	root.SetContext(ctx)

	err := root.Execute()

	// An interrupted run is not a failure of the request; report it the way
	// datumctl reports its own (130), separately from the 0/1/2 the shared CLI
	// uses for completed, failed, and misused.
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "\nAborted.")
		os.Exit(130)
	}
	if err == nil {
		os.Exit(0)
	}

	// patchcli has already written its own message to stderr; exitError only
	// carries the code out through cobra's RunE.
	var exit exitError
	if errors.As(err, &exit) {
		os.Exit(exit.code)
	}
	// Anything else is a usage or configuration problem cobra or this package
	// raised, and has not been printed (SilenceErrors is set).
	fmt.Fprintln(os.Stderr, "patch:", err)
	os.Exit(2)
}

// exitError carries an exit code from [patchcli.Invocation.Execute] out through
// cobra's RunE, which only speaks error. Its message is never shown — the
// shared CLI has already explained the failure on stderr.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
