package mcp

import (
	"bufio"
	"io"
)

// NewInProcess connects a Client to registry over a pair of in-memory
// pipes, with Serve running in its own goroutine -- the same protocol a
// real subprocess would speak over its stdin/stdout, just without paying
// for a fork/exec. This is what v2/agent/gemini uses by default: the
// model's only route to any tool is through this Client, and the Client's
// only route to anything is through registry, which is what makes "Gemini
// can only take actions within the sandbox using the MCP tools" true by
// construction rather than by convention.
//
// A real subprocess remains available for cases that want an actual process
// boundary (see mcp/cmd/mcpserver) -- NewClient works identically over its
// Stdin/Stdout pipes.
func NewInProcess(registry *Registry) *Client {
	clientReadsFromServer, serverWritesToClient := io.Pipe()
	serverReadsFromClient, clientWritesToServer := io.Pipe()

	go func() {
		_ = Serve(registry, bufio.NewReader(serverReadsFromClient), bufio.NewWriter(serverWritesToClient))
		serverWritesToClient.Close()
	}()

	return NewClient(clientWritesToServer, clientReadsFromServer, clientWritesToServer)
}
