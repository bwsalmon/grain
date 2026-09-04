// Command konturctl is the operator-facing CLI for a kontur node: it
// installs the standalone kubelet described in
// deploy/static-kubelet/README.md ("konturctl setup"), and submits,
// updates and deletes the VM pods it runs ("konturctl vm ..."), all
// without needing kubectl (there's no apiserver to talk to) or a checkout
// of this repo on the node.
//
// This is distinct from cmd/kontur, which is the container-facing binary
// that actually runs inside those VM pods.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwsalmon/kontur/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
