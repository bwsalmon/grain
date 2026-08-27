// Command mcpserver is the standalone form of the v2/mcp server: it serves
// the sandbox tools and the mocked GitHub-shaped escape hatches over its own
// stdin/stdout, so anything that can spawn a subprocess and speak MCP --
// not just v2/agent/gemini's in-process client -- can drive it. Real MCP
// clients (an actual `claude` or `gemini` CLI's --mcp-config, for instance)
// can point at this binary directly.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/bwsalmon/grain/v2/mcp"
)

func main() {
	root := flag.String("sandbox-root", "", "directory the sandbox tools are confined to (required)")
	flag.Parse()

	if *root == "" {
		fmt.Fprintln(os.Stderr, "mcpserver: -sandbox-root is required")
		os.Exit(2)
	}
	if info, err := os.Stat(*root); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "mcpserver: -sandbox-root %q is not a directory: %v\n", *root, err)
		os.Exit(2)
	}

	registry := mcp.NewRegistry()
	registry.Register(mcp.NewSandboxTools(*root)...)
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
