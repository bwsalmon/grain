// Command granule is PID 1 in a grain's container: it boots the VMM
// beside it, provisions the sandbox, runs the agent, and writes the
// whole run to stdout as records.
//
// It takes no arguments at all, and that is the contract rather than an
// economy (docs/grain.md, "The wire"). Everything a controller does to a
// grain is a container-runtime operation -- create it, list it, tail its
// logs, destroy it -- so there is no subcommand for one to call, and
// adding one later would be adding a way to call into a grain.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwsalmon/grain/pkg/granule"
)

func main() {
	if len(os.Args) > 1 {
		// Not silently ignored: an argument means somebody believes this
		// takes one, and the likeliest reason is a controller built
		// against a granule that did.
		fmt.Fprintf(os.Stderr, "granule: takes no arguments, got %q\n", os.Args[1])
		fmt.Fprintln(os.Stderr, "granule: configuration is the environment and the tree at /grain; see docs/grain.md")
		os.Exit(granule.ExitFailed)
	}

	// SIGTERM is how a grain is stopped, and being PID 1 means it
	// arrives here rather than at the VMM. Cancelling the context is
	// what turns it into the graceful path: the agent stops, a Result is
	// written, and the guest is powered off, all inside the runtime's
	// grace period before SIGKILL.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// stderr is the shim's human-facing diagnostics and stdout is
	// records only, so that a stray warning is never mistaken for a
	// damaged record (docs/grain-options.md, "Can stdout and stderr be
	// told apart?").
	stream := granule.NewStream(os.Stdout, nil)

	cfg := granule.DefaultConfig()
	deps := granule.Deps{
		VMM:   granule.NewVMM(""),
		Guest: granule.NewGuest(""),
		// No agent yet: this build provisions a sandbox and reports how
		// it went. Wiring the agent CLI in is the next step, and keeping
		// it a field rather than a build tag is what lets the
		// provisioning half be run and tested on its own.
		Agent: nil,
	}

	os.Exit(granule.Run(ctx, cfg, deps, stream))
}
