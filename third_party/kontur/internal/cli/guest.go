package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bwsalmon/kontur/internal/guestbuild"
)

// repeatedFlag collects a flag given more than once, in order.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return fmt.Sprint(*r) }
func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// runGuest dispatches "konturctl guest <subcommand>".
func runGuest(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("guest: expected a subcommand (build)")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "build":
		return runGuestBuild(ctx, rest, stdout, stderr)
	default:
		return fmt.Errorf("guest: unknown subcommand %q", cmd)
	}
}

// runGuestBuild boots a guest image, provisions it from inside, and
// commits the result -- see internal/guestbuild for what that buys over
// the Dockerfile's own GUEST_SETUP_SCRIPT, and what it costs.
func runGuestBuild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("konturctl guest build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "base image to boot and customize (required)")
	setup := fs.String("setup", "", "path to a script to run as root inside the booted guest (required)")
	tag := fs.String("t", "", "image reference to commit the result to (required)")
	readyTimeout := fs.Duration("ready-timeout", 3*time.Minute, "how long to wait for the guest to become reachable")
	shutdownTimeout := fs.Duration("shutdown-timeout", 60*time.Second, "how long to wait for the guest to power off before giving up")
	keep := fs.Bool("keep-on-failure", false, "leave the container in place when the build fails, for inspection")
	var runArgs repeatedFlag
	fs.Var(&runArgs, "docker-run-arg", "extra argument for the underlying `docker run` (repeatable) -- how a build reaches a proxy, a CA bundle or a private registry, none of which the guest inherits on its own")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *setup == "" || *tag == "" {
		fs.Usage()
		return fmt.Errorf("-from, -setup and -t are all required")
	}

	// Read the script here rather than in guestbuild: a path that
	// doesn't exist should fail before anything boots, not after a VM
	// has come up and the caller has waited for it.
	script, err := os.ReadFile(*setup)
	if err != nil {
		return fmt.Errorf("reading the setup script: %w", err)
	}

	return guestbuild.Build(ctx, guestbuild.Options{
		From:            *from,
		Setup:           string(script),
		Tag:             *tag,
		ExtraRunArgs:    runArgs,
		ReadyTimeout:    *readyTimeout,
		ShutdownTimeout: *shutdownTimeout,
		KeepOnFailure:   *keep,
		Stdout:          stdout,
		Stderr:          stderr,
	})
}
