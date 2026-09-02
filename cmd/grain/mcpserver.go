// mcpserver.go implements `grain mcpserver`, formerly its own
// cmd/mcpserver binary before bwsalmon/agents#313 combined every mode
// into one: it serves the sandbox tools and the mocked GitHub-shaped
// escape hatches over its own stdin/stdout, so anything that can spawn a
// subprocess and speak MCP -- not just an in-process
// client -- can drive it. Real MCP clients (an actual `claude` or
// `gemini` CLI's --mcp-config, for instance) can point at this same
// grain binary, "mcpserver" and its own flags as the configured args, as
// pkg/agent/claude does.
//
// Its sandbox tools run against one of two backends: a local directory
// (-sandbox-root, unchanged since before bwsalmon/agents#256 -- what
// pkg/mcp's own tests and e2e/ still use, since neither wants a real VM),
// or a real kontur-managed VM (-kontur-vm, see package kontur and
// pkg/mcp's NewSSHSandboxTools) -- the wiring bwsalmon/agents#256 asked
// for, so a run_command/read_file/edit_file/write_file call actually
// lands inside one of the sandbox VMs kontur is running instead of the
// local-directory stand-in pkg/mcp/sandbox_tools.go's own doc comment
// describes. -kontur-backend selects which of kontur's two ways of
// running that VM -kontur-vm was created with -- "docker" (the default,
// bwsalmon/agents#353) needs just a local docker daemon; "static-pod"
// needs a standalone kubelet (deploy/static-kubelet/README.md) and is
// resolved via crictl instead.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/bwsalmon/grain/pkg/kontur"
	"github.com/bwsalmon/grain/pkg/mcp"
)

func mcpserver(args []string) {
	fs := flag.NewFlagSet("grain mcpserver", flag.ExitOnError)
	root := fs.String("sandbox-root", "",
		"directory the local sandbox tools are confined to (mutually exclusive with -kontur-vm)")

	konturVM := fs.String("kontur-vm", "",
		"name of a kontur-managed VM (bwsalmon/kontur) to run the sandbox tools against over SSH, "+
			"instead of a local directory (mutually exclusive with -sandbox-root)")
	sshUser := fs.String("ssh-user", "", "username to SSH into -kontur-vm as (required with -kontur-vm)")
	execKey := fs.String("exec-key", "", "path, *inside -kontur-vm's container*, of the private key `kontur exec` authenticates to the guest with. Optional: left unset, `kontur exec` uses the keypair `kontur run` generates for that guest at boot, which is what a stock bwsalmon/kontur guest image authorizes (see its internal/guestkey). Set it only for a custom guest image that authorizes a key of its own instead.")
	workspace := fs.String("workspace", "",
		"working directory run_command/read_file/edit_file/write_file operate in on -kontur-vm (required with -kontur-vm)")
	fs.Parse(args)

	var tools []mcp.Tool
	switch {
	case *konturVM != "" && *root != "":
		fmt.Fprintln(os.Stderr, "grain mcpserver: -sandbox-root and -kontur-vm are mutually exclusive")
		os.Exit(2)
	case *konturVM != "":
		tools = mustKonturSandboxTools(*konturVM, *sshUser, *execKey, *workspace)
	case *root != "":
		if info, err := os.Stat(*root); err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "grain mcpserver: -sandbox-root %q is not a directory: %v\n", *root, err)
			os.Exit(2)
		}
		tools = mcp.NewSandboxTools(*root)
	default:
		fmt.Fprintln(os.Stderr, "grain mcpserver: exactly one of -sandbox-root or -kontur-vm is required")
		os.Exit(2)
	}

	registry := mcp.NewRegistry()
	registry.Register(tools...)
	// No controller reads a mocked GitHub-shaped tool call back out of this
	// process today -- there is nothing downstream of it yet (see README.md).
	// The sink exists so calls fail loudly (a real Result, not a silent
	// no-op) rather than because there's anywhere for it to report to.
	registry.Register(mcp.NewMockTools(&mcp.MockSink{})...)

	// Serve returns io.EOF once its caller closes the write end of our
	// stdin -- the ordinary way a client signals "done", not a failure.
	// This process's own lifetime is the cancellation boundary: whatever
	// spawned it (claude -p's own --mcp-config fork, in production) is
	// killed as a whole when a run is cancelled, which takes this stdio
	// server down with it -- see Serve's own doc comment on why that
	// makes context.Background() the right ctx here, not a derived one.
	if err := mcp.Serve(context.Background(), registry, bufio.NewReader(os.Stdin), bufio.NewWriter(os.Stdout)); err != nil && !errors.Is(err, io.EOF) {
		log.Fatalf("grain mcpserver: %v", err)
	}
}

// mustKonturSandboxTools builds the sandbox tools against konturVM's
// guest, exiting the process (matching the -sandbox-root checks above) if
// any required flag is missing -- there is no useful degraded mode for an
// mcpserver process that can't reach the sandbox it was told to run
// against.
//
// Unlike the SSH transport this replaced, there is nothing to resolve
// first: the tools reach the guest by exec'ing into konturVM's own
// container (mcp.DockerExecRunner), whose name follows from the VM name
// alone, so no port, pod IP or state file lookup stands between this and
// a usable runner.
func mustKonturSandboxTools(konturVM, sshUser, execKey, workspace string) []mcp.Tool {
	if sshUser == "" {
		fmt.Fprintln(os.Stderr, "grain mcpserver: -ssh-user is required with -kontur-vm")
		os.Exit(2)
	}
	if workspace == "" {
		fmt.Fprintln(os.Stderr, "grain mcpserver: -workspace is required with -kontur-vm")
		os.Exit(2)
	}

	runner := &mcp.DockerExecRunner{
		Container: kontur.PodName(konturVM),
		User:      sshUser,
		KeyPath:   execKey,
	}
	return mcp.NewSSHSandboxTools(runner, workspace)
}
