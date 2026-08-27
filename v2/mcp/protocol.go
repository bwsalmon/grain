// Package mcp is a small, self-contained port of grain/automation/mcp_server.py's
// hand-rolled MCP server: newline-delimited JSON-RPC 2.0 over a pair of
// io.Reader/io.Writer, with no dependency on an MCP SDK, mirroring v1's own
// choice not to take one on. It has two halves: Registry/Serve (the server)
// and Client (a minimal client for whatever process or in-process pipe is
// speaking the server side back at it).
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "grain-v2-sandbox"
	serverVersion   = "0.1.0"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func mustMarshalLine(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Every value passed to this ever came from our own handlers, never
		// from the wire, so a marshal failure here means a bug in this
		// package -- not something a caller can recover from meaningfully.
		panic(err)
	}
	return append(b, '\n')
}

// Serve reads newline-delimited JSON-RPC requests from r, dispatches each to
// registry, and writes any response to w -- the same framing v1's Python
// server uses (a bare readline()/write() loop, no Content-Length headers),
// which also happens to be real MCP stdio framing.
func Serve(registry *Registry, r *bufio.Reader, w *bufio.Writer) error {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				if resp := registry.Handle(trimmed); resp != nil {
					if _, werr := w.Write(resp); werr != nil {
						return werr
					}
					if ferr := w.Flush(); ferr != nil {
						return ferr
					}
				}
			}
		}
		if err != nil {
			return err
		}
	}
}
