package main

import (
	"fmt"
	"io"
	"os"

	"github.com/bwsalmon/grain/pkg/agent/antigravity"
)

// agyToolHook is the "agy-tool-hook" subcommand: agy's own PreToolUse
// hook, which is to say the thing agy runs before every tool call a run
// makes and asks whether that call may proceed. It reads the call on
// stdin and writes the decision on stdout, which is the contract agy
// documents (pkg/agent/antigravity's hookConfigJSON quotes it).
//
// It is grain forking itself rather than a script agy's config points at,
// for the reason mcpserver is: the hook must be some executable named in
// a config file written per run, and the one executable a deployment is
// guaranteed to have is the grain binary already running. A script would
// have to be installed, found and kept in step with the tool names in
// pkg/agent/antigravity; a subcommand is compiled from them.
//
// Nothing here decides anything. antigravity.HookDecision holds the
// policy and the tests that cover it, because that package owns the list
// of agy tools grain withholds and is where a reader looking for the
// policy would go.
//
// It never exits non-zero. agy runs this as `sh -c` and reads its stdout;
// an error status buys nothing a decision cannot say, and a hook that
// fails noisily on a payload it did not expect would put a run's ability
// to call any tool at all behind grain's ability to parse a shape agy has
// not promised. Silence -- an empty decision -- is what agy reads as "no
// opinion", which is this command's answer to anything it does not
// recognise.
func agyToolHook(args []string) {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "grain %s: takes no arguments\n", antigravity.HookSubcommand)
	}
	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		payload = nil
	}
	os.Stdout.Write(antigravity.HookDecision(payload))
}
