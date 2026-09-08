// Command patch is the standalone Datum Cloud assistant (A2A) CLI: a thin
// client over the assistant service, driven by PATCH_URL/PATCH_TOKEN. The
// client itself lives in internal/patchcli, shared with the datumctl plugin in
// cmd/milo-patch; this file only wires the process environment to it.
package main

import (
	"context"
	"os"

	"github.com/milo-os/assistant/internal/patchcli"
)

func main() {
	code := patchcli.Run(context.Background(), os.Args[1:], os.Getenv, patchcli.StdIO())
	os.Exit(code)
}
