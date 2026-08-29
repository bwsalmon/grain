package e2e

// TestLiveIssueCompletesEndToEnd is agent/gemini/gemini_live_test.go's
// own gating, one layer up: it exercises the real Gemini API against the
// same real store/gitproxy/git rig e2e_test.go's scripted tests do, so it
// runs in CI (where GEMINI_API_KEY is unset) without failing, but proves
// -- wherever a key is available -- that an unscripted model, left to
// decide its own tool calls, actually completes an issue the way the
// scripted tests assume a model would:
//
//	GEMINI_API_KEY=... go test ./e2e/... -run TestLiveIssueCompletesEndToEnd -v

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/agent/gemini"
	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

func TestLiveIssueCompletesEndToEnd(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live Gemini integration test")
	}

	const slot = "sandbox-bd453be9-live-1"
	w := newWorld(t, []string{slot})
	w.newRepo("acme", "live")

	clock := baseTime
	fileIssue(w, "iss-live", human("tester"), model.RepoRef{Owner: "acme", Name: "live"})

	dispatches, err := dispatch.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil || len(dispatches) != 1 {
		t.Fatalf("Cycle: %v, %+v", err, dispatches)
	}
	d := dispatches[0]
	branch := model.BranchName("iss-live")
	remote := w.remote("acme", "live")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	fw, err := gemini.New(ctx, apiKey)
	if err != nil {
		t.Fatalf("gemini.New: %v", err)
	}

	prompt := "Your sandbox workspace has git already configured with credentials for one remote. " +
		"Using your run_command tool, do exactly the following as ordinary shell/git commands:\n" +
		"1. Clone " + remote + " into a directory named work.\n" +
		"2. Inside work, create a new branch named exactly " + branch + " (git checkout -b).\n" +
		"3. Append the exact line 'gemini was here' to a file named NOTES.md in that directory " +
		"(creating it if it does not exist).\n" +
		"4. Commit that change with any commit message.\n" +
		"5. Push the " + branch + " branch to the origin remote (not main).\n" +
		"Reply with a short confirmation once the push has succeeded."

	result, err := fw.Run(ctx, agent.RunConfig{Prompt: prompt, SandboxRoot: w.roots[slot], MaxTurns: 15})
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
	if !strings.Contains(notes, "gemini was here") {
		t.Fatalf("NOTES.md on %s = %q, want it to contain the requested line", branch, notes)
	}

	completedAt := clock.Add(2 * time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "iss-live", CompletedAt: &completedAt}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "iss-live", model.StateCompleted, false)
}
