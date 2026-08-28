// Command mcpserver is the standalone form of the v2/mcp server: it serves
// the sandbox tools and the mocked GitHub-shaped escape hatches over its
// own stdin/stdout, so anything that can spawn a subprocess and speak MCP
// -- not just v2/agent/gemini's in-process client -- can drive it. Real
// MCP clients (an actual `claude` or `gemini` CLI's --mcp-config, for
// instance) can point at this binary directly.
//
// Its sandbox tools run against one of two backends: a local directory
// (-sandbox-root, unchanged since before bwsalmon/agents#256 -- what
// pkg/mcp's own tests and e2e/ still use, since neither wants a real VM),
// or a real kontur-managed VM behind a static kubelet (-kontur-vm, see
// package kontur and pkg/mcp's NewSSHSandboxTools) -- the wiring
// bwsalmon/agents#256 asked for, so a run_command/read_file/edit_file/
// write_file call actually lands inside one of the sandbox VMs the
// Kubelet is running instead of the local-directory stand-in
// pkg/mcp/sandbox_tools.go's own doc comment describes.
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

	"github.com/bwsalmon/grain/v2/pkg/kontur"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
)

func main() {
	root := flag.String("sandbox-root", "",
		"directory the local sandbox tools are confined to (mutually exclusive with -kontur-vm)")

	konturVM := flag.String("kontur-vm", "",
		"name of a kontur-managed VM (bwsalmon/kontur) to run the sandbox tools against over SSH, "+
			"instead of a local directory (mutually exclusive with -sandbox-root)")
	konturStateDir := flag.String("kontur-state-dir", kontur.DefaultStateDir,
		"kontur's VM state directory, used to look up -kontur-vm's external port")
	criRuntimeEndpoint := flag.String("cri-runtime-endpoint", kontur.DefaultRuntimeEndpoint,
		"containerd CRI socket, used to resolve -kontur-vm's pod IP via crictl")
	konturHost := flag.String("kontur-host", "",
		"override the address -kontur-vm resolves to, skipping the crictl pod-IP lookup")
	sshUser := flag.String("ssh-user", "", "username to SSH into -kontur-vm as (required with -kontur-vm)")
	sshKey := flag.String("ssh-key", "", "path to the SSH private key to authenticate to -kontur-vm with (required with -kontur-vm)")
	workspace := flag.String("workspace", "",
		"working directory run_command/read_file/edit_file/write_file operate in on -kontur-vm (required with -kontur-vm)")
	flag.Parse()

	var tools []mcp.Tool
	switch {
	case *konturVM != "" && *root != "":
		fmt.Fprintln(os.Stderr, "mcpserver: -sandbox-root and -kontur-vm are mutually exclusive")
		os.Exit(2)
	case *konturVM != "":
		tools = mustSSHSandboxTools(*konturVM, *konturStateDir, *criRuntimeEndpoint, *konturHost, *sshUser, *sshKey, *workspace)
	case *root != "":
		if info, err := os.Stat(*root); err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "mcpserver: -sandbox-root %q is not a directory: %v\n", *root, err)
			os.Exit(2)
		}
		tools = mcp.NewSandboxTools(*root)
	default:
		fmt.Fprintln(os.Stderr, "mcpserver: exactly one of -sandbox-root or -kontur-vm is required")
		os.Exit(2)
	}

	registry := mcp.NewRegistry()
	registry.Register(tools...)
	// No controller reads a mocked GitHub-shaped tool call back out of this
	// process today -- there is nothing downstream of it yet (see v2/README.md).
	// The sink exists so calls fail loudly (a real Result, not a silent
	// no-op) rather than because there's anywhere for it to report to.
	registry.Register(mcp.NewMockTools(&mcp.MockSink{})...)

	// Serve returns io.EOF once its caller closes the write end of our
	// stdin -- the ordinary way a client signals "done", not a failure.
	if err := mcp.Serve(registry, bufio.NewReader(os.Stdin), bufio.NewWriter(os.Stdout)); err != nil && !errors.Is(err, io.EOF) {
		log.Fatalf("mcpserver: %v", err)
	}
}

// mustSSHSandboxTools resolves konturVM's SSH endpoint and builds the
// sandbox tools against it, exiting the process (matching the -sandbox-root
// checks above) if any required flag is missing or the VM can't be
// resolved -- there is no useful degraded mode for an mcpserver process
// that can't reach the sandbox it was told to run against.
func mustSSHSandboxTools(konturVM, stateDir, criRuntimeEndpoint, konturHost, sshUser, sshKey, workspace string) []mcp.Tool {
	if sshUser == "" {
		fmt.Fprintln(os.Stderr, "mcpserver: -ssh-user is required with -kontur-vm")
		os.Exit(2)
	}
	if sshKey == "" {
		fmt.Fprintln(os.Stderr, "mcpserver: -ssh-key is required with -kontur-vm")
		os.Exit(2)
	}
	if workspace == "" {
		fmt.Fprintln(os.Stderr, "mcpserver: -workspace is required with -kontur-vm")
		os.Exit(2)
	}

	port, err := kontur.Port(stateDir, konturVM)
	if err != nil {
		log.Fatalf("mcpserver: %v", err)
	}

	host := konturHost
	if host == "" {
		host, err = kontur.PodIP(context.Background(), criRuntimeEndpoint, konturVM)
		if err != nil {
			log.Fatalf("mcpserver: %v", err)
		}
	}

	runner := &mcp.SSHRunner{User: sshUser, Host: host, Port: port, KeyPath: sshKey}
	return mcp.NewSSHSandboxTools(runner, workspace)
}
