package e2e

// TestLiveIssueCompletesEndToEnd is agent/antigravity's own live gating,
// one layer up: it exercises the real agy binary against the same real
// store/gitproxy/git rig e2e_test.go's scripted tests do, so it runs in
// CI (where neither GEMINI_API_KEY nor agy is present) without failing,
// but proves -- wherever both are available -- that an unscripted model,
// left to decide its own tool calls, actually completes an issue the way
// the scripted tests assume a model would:
//
//	GEMINI_API_KEY=... go test ./e2e/... -run TestLiveIssueCompletesEndToEnd -v

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/agent/antigravity"
	"github.com/bwsalmon/grain/pkg/dispatch"
	"github.com/bwsalmon/grain/pkg/model"
)

func TestLiveIssueCompletesEndToEnd(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live agent integration test")
	}
	agyPath, err := exec.LookPath("agy")
	if err != nil {
		t.Skip("agy binary not found on PATH; skipping live agent integration test")
	}

	w := newWorld(t)
	w.newRepo("acme", "live")

	clock := baseTime
	fileIssue(w, "iss-live", human("tester"), model.RepoRef{Owner: "acme", Name: "live"})

	dispatches, err := dispatch.Cycle(w.ctx, w.store, 1, clock)
	if err != nil || len(dispatches) != 1 {
		t.Fatalf("Cycle: %v, %+v", err, dispatches)
	}
	d := dispatches[0]
	branch := model.BranchName("iss-live")
	remote := w.remote("acme", "live")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// agy reaches the sandbox by forking this repo's own "mcpserver"
	// subcommand, so a live run needs a built grain binary as well as a
	// credential -- see agent/antigravity's own doc comment.
	fw := antigravity.New(agyPath, buildGrainBinary(t), antigravity.WithAPIKey(apiKey))

	prompt := "Your sandbox workspace has git already configured with credentials for one remote. " +
		"Using your run_command tool, do exactly the following as ordinary shell/git commands:\n" +
		"1. Clone " + remote + " into a directory named work.\n" +
		"2. Inside work, create a new branch named exactly " + branch + " (git checkout -b).\n" +
		"3. Append the exact line 'the agent was here' to a file named NOTES.md in that directory " +
		"(creating it if it does not exist).\n" +
		"4. Commit that change with any commit message.\n" +
		"5. Push the " + branch + " branch to the origin remote (not main).\n" +
		"Reply with a short confirmation once the push has succeeded."

	result, err := fw.Run(ctx, agent.RunConfig{Prompt: prompt, SandboxRoot: w.prepareSandbox(dispatches[0]), MaxTurns: 15})
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}
	for _, c := range result.ToolCalls {
		t.Logf("tool call: %s(%v) -> error=%v text=%q", c.Name, c.Arguments, c.IsError, c.Text)
	}

	if err := w.store.FinishRun(w.ctx, d.RunID, clock.Add(time.Minute), "succeeded", ""); err != nil {
		t.Fatal(err)
	}

	if !w.branchExists("acme", "live", branch) {
		t.Fatalf("expected branch %s to exist in acme/live after a live agent run; final answer: %q", branch, result.FinalText)
	}
	bare := filepath.Join(w.upstreamDir, "acme", "live.git")
	notes := runOutput(t, w.upstreamDir, "git", "--git-dir", bare, "show", branch+":NOTES.md")
	if !strings.Contains(notes, "the agent was here") {
		t.Fatalf("NOTES.md on %s = %q, want it to contain the requested line", branch, notes)
	}

	completedAt := clock.Add(2 * time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "iss-live", CompletedAt: &completedAt}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "iss-live", model.StateCompleted, false)
}
