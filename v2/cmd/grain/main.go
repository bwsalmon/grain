// Command grain is grain's single operator binary. bwsalmon/agents#275
// asks for the UI to be "a command from the cli" rather than its own
// binary, so this replaces what were previously two separate `go build`
// outputs (cmd/ui and cmd/graind) with one: `grain ui` serves pkg/ui's
// JSON API and static frontend (ui.go, née cmd/ui/main.go), and `grain
// daemon` runs pkg/orchestrate's reconcile loop (daemon.go, née
// cmd/graind/main.go). Each subcommand keeps its own flag.FlagSet, so
// `grain ui -h` / `grain daemon -h` show only the flags that command
// takes rather than the union of both.
//
// cmd/mcpserver stays a separate binary on purpose: pkg/agent/claude
// execs it by path as a subprocess (via --mcp-config) rather than a
// human running it directly, so folding it in here would not remove a
// binary an operator ever types the name of -- it would just make the
// one mcpserver needs harder to point at.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch cmd := os.Args[1]; cmd {
	case "ui":
		runUI(os.Args[2:])
	case "daemon":
		runDaemon(os.Args[2:])
	case "-h", "-help", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "grain: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: grain <command> [flags]

commands:
  ui       serve the task UI (pkg/ui) on localhost and open it in a browser
  daemon   run pkg/orchestrate's reconcile loop against one data directory

Run "grain <command> -h" to see a command's own flags.`)
}
