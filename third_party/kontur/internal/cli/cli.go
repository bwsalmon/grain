// Package cli implements the konturctl command-line tool: setting up the
// static kubelet described in deploy/static-kubelet/README.md, and
// submitting, updating and deleting the VM pods it runs, all from a single
// binary. See cmd/konturctl/main.go for the entrypoint.
package cli

import (
	"context"
	"fmt"
	"io"
)

const usage = `konturctl manages kontur VMs, either as static pods on a standalone kubelet
or as plain containers on a local docker daemon.

Usage:
  konturctl setup [flags]              install containerd, CNI and a standalone kubelet
  konturctl vm create <name> [flags]   start a new VM (-backend static-pod, the default, or docker)
  konturctl vm update <name> [flags]   change a VM's settings and re-submit it
  konturctl vm delete <name>           remove a VM
  konturctl vm list                    list VMs kontur knows about
  konturctl guest build [flags]        boot a guest image, provision it, commit a new one

Run "konturctl <command> -h" for a command's flags.
`

// Run executes konturctl with args (os.Args[1:]) and returns the process
// exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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
		err = runVM(ctx, rest, stdout, stderr)
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

	if err != nil {
		fmt.Fprintf(stderr, "konturctl: %v\n", err)
		return 1
	}
	return 0
}
