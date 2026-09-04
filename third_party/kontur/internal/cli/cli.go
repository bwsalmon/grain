// Package cli implements the konturctl command-line tool: setting up the
// static kubelet described in deploy/static-kubelet/README.md, and
// submitting, updating and deleting the VM pods it runs, all from a single
// binary. See cmd/konturctl/main.go for the entrypoint.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

const usage = `konturctl manages kontur VMs, either as static pods on a standalone kubelet
or as plain containers on a local docker daemon.

Usage:
  konturctl setup [flags]                       install containerd, CNI and a standalone kubelet
  konturctl vm create <name> [flags]            start a new VM (-backend static-pod, the default, or docker)
  konturctl vm run <name> [flags] -- <command>  create a VM, wait for it, run one command, delete it
  konturctl vm exec <name> [flags] -- <command> run a command in a running VM's guest
  konturctl vm shell <name> [flags]             open an interactive shell in a running VM's guest
  konturctl vm wait <name> [flags]              block until a VM's guest answers a command
  konturctl vm status <name> [flags]            report whether a VM's guest is up, without waiting
  konturctl vm update <name> [flags]            change a VM's settings and re-submit it
  konturctl vm delete <name>                    remove a VM
  konturctl vm list                             list VMs kontur knows about
  konturctl guest build [flags]                 boot a guest image, provision it, commit a new one

Run "konturctl <command> -h" for a command's flags.
`

// Run executes konturctl with args (os.Args[1:]) and returns the process
// exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "setup":
		err = runSetup(ctx, rest, stdout, stderr)
	case "vm":
		err = runVM(ctx, rest, stdin, stdout, stderr)
	case "guest":
		err = runGuest(ctx, rest, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "konturctl: unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, usage)
		return 2
	}

	// A subcommand's own "-h" asked a question and has already been
	// answered: the flag package printed that command's flags on the way
	// out. Adding "konturctl: flag: help requested" underneath them and
	// exiting non-zero makes a satisfied request read as a mistake, and
	// makes "konturctl vm create -h" unusable in any script that checks
	// statuses. (cmd/kontur answers "kontur cp -h" the same way.)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}

	// A command run in a guest ("vm exec"/"vm shell"/"vm run") that exited
	// non-zero is reported as its own status and nothing else: konturctl
	// is a pipe here, and the command's stderr has already reached the
	// caller's. Printing "konturctl: ..." over the top would make every
	// failing guest command look like a konturctl failure.
	var status *exitStatusError
	if errors.As(err, &status) {
		return status.code
	}
	if err != nil {
		fmt.Fprintf(stderr, "konturctl: %v\n", err)
		return 1
	}
	return 0
}
