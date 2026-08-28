// bwsalmon/agents#249's own live test: the two flagship scenarios
// v2/e2e/e2e_test.go already proves against manual store.Observe calls
// standing in for "GitHub itself" (that file's own doc comments: "v2 has
// no completion detector of its own yet," "no code in v2 does this yet
// [merges/closes a PR]") -- driven here through RunCycle and a real
// github.Client against githubsim instead, the way gitproxy/live_test.go
// already holds the git-transport half to the same discipline.
package orchestrator_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/agent/gemini"
	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// credentialSlot gives slot's own sandbox directory a git identity, the
// same one-time-per-slot setup v2/e2e/harness_test.go's newWorld gives
// every slot up front -- mcp.ConfigureGitCredentials' own doc comment
// explains why a fresh sandbox otherwise has no git identity at all
// ("makes git commit fail outright"). This test clones straight off a
// bare repo path rather than through a real gitproxy, so the placeholder
// remote's scheme and host (never its path) are the only part of this
// call that matters here.
func credentialSlot(t *testing.T, sandboxes *orchestrator.HostSandboxes, slot string) {
	t.Helper()
	root, err := sandboxes.RootFor(slot)
	if err != nil {
		t.Fatal(err)
	}
	if err := mcp.ConfigureGitCredentials(root, "http://placeholder.example/x/y.git", "unused"); err != nil {
		t.Fatal(err)
	}
}

// --- scripting helpers for the gemini agent, duplicated from
// v2/e2e/harness_test.go the same deliberate way gitproxy/live_test.go's
// own comment explains: package-private test helpers are cheaper to
// duplicate than to share. ---------------------------------------------

type scriptedGenerator struct {
	responses []*genai.GenerateContentResponse
	calls     int
}

func (g *scriptedGenerator) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	if g.calls >= len(g.responses) {
		g.calls++
		return nil, nil
	}
	resp := g.responses[g.calls]
	g.calls++
	return resp, nil
}

func finalText(text string) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: genai.NewContentFromText(text, genai.RoleModel)}},
	}
}

func toolCall(name string, args map[string]any) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: genai.NewContentFromFunctionCall(name, args, genai.RoleModel)}},
	}
}

func pushScript(remote, branch, taskID string) []*genai.GenerateContentResponse {
	cmd := "git clone " + remote + " work && cd work && " +
		"git checkout -b " + branch + " && " +
		"echo 'change for " + taskID + "' >> NOTES.md && " +
		"git add NOTES.md && git commit -q -m 'agent commit for " + taskID + "' && " +
		"git push origin " + branch
	return []*genai.GenerateContentResponse{
		toolCall("run_command", map[string]any{"command": cmd}),
		finalText("pushed " + branch),
	}
}

func askScript(question string) []*genai.GenerateContentResponse {
	return []*genai.GenerateContentResponse{
		toolCall("ask_question", map[string]any{"question": question}),
		finalText("waiting on a reply"),
	}
}

func scriptedFramework(script []*genai.GenerateContentResponse) func() agent.Framework {
	return func() agent.Framework { return gemini.NewForTest(&scriptedGenerator{responses: script}) }
}

func TestRunCycleCompletesEndToEnd(t *testing.T) {
	const slot = "sandbox-249-1"
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	sim.Issues[1] = &githubsim.Issue{
		Title: "add a NOTES file", Body: "please add one\n\n/repo acme/widgets\n",
		Author: "alice", Labels: map[string]struct{}{"grain-agent": {}},
	}
	cfg := orchestrator.Config{TaskRepo: repo, TriggerLabel: "grain-agent"}
	sandboxes := orchestrator.NewHostSandboxes(t.TempDir())
	credentialSlot(t, sandboxes, slot)

	clock := baseTime
	taskID := orchestrator.TaskID(repo, 1)
	branch := model.BranchName(taskID)

	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes, Config: cfg, Slots: []string{slot},
		Framework: scriptedFramework(pushScript(sim.BareRepo, branch, taskID)),
	}
	if err := orchestrator.RunCycle(ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	st, err := store.State(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state after the first cycle = %q, want completed", st)
	}
	if len(sim.PullRequests) != 1 {
		t.Fatalf("expected a pull request to have been opened, got %+v", sim.PullRequests)
	}
	issue, err := client.GetIssue("acme", "widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if issue.HasLabel("grain-agent") {
		t.Fatal("expected the trigger label to have been removed")
	}

	// GitHub itself merges the PR -- nothing in this package does that; a
	// live test plays GitHub's part here the same way v2/e2e's own
	// mergeBranchIntoDefault does for the git side of a merge.
	for i := range sim.PullRequests {
		sim.PullRequests[i].State = "closed"
	}

	clock = clock.Add(time.Minute)
	if err := orchestrator.RunCycle(ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (second, sync-only): %v", err)
	}

	st, err = store.State(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state after the merge = %q, want closed", st)
	}
	issue, err = client.GetIssue("acme", "widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if issue.State != "closed" {
		t.Fatalf("task-repo issue state = %q, want closed", issue.State)
	}
}

func TestRunCycleParksOnAQuestionThenResumesAfterAReply(t *testing.T) {
	const slot = "sandbox-249-2"
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	sim.Issues[1] = &githubsim.Issue{
		Title: "ambiguous task", Body: "do the thing\n\n/repo acme/widgets\n",
		Author: "alice", Labels: map[string]struct{}{"grain-agent": {}},
	}
	cfg := orchestrator.Config{TaskRepo: repo, TriggerLabel: "grain-agent"}
	sandboxes := orchestrator.NewHostSandboxes(t.TempDir())
	credentialSlot(t, sandboxes, slot)
	taskID := orchestrator.TaskID(repo, 1)

	clock := baseTime
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes, Config: cfg, Slots: []string{slot},
		Framework: scriptedFramework(askScript("which file should this go in?")),
	}
	if err := orchestrator.RunCycle(ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	st, err := store.State(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateAwaitingReply {
		t.Fatalf("state after the question = %q, want awaiting_reply", st)
	}
	comments, err := client.ListComments("acme", "widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "which file should this go in?" {
		t.Fatalf("got %+v", comments)
	}

	// A human replies and re-applies the trigger label -- the only way an
	// awaiting_reply task ever queues again (poll.go's own doc comment).
	if _, err := client.CreateComment("acme", "widgets", 1, "put it in NOTES.md"); err != nil {
		t.Fatal(err)
	}
	if err := client.AddLabel("acme", "widgets", 1, "grain-agent"); err != nil {
		t.Fatal(err)
	}

	branch := model.BranchName(taskID)
	deps.Framework = scriptedFramework(pushScript(sim.BareRepo, branch, taskID))
	clock = clock.Add(time.Minute)
	if err := orchestrator.RunCycle(ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (resume): %v", err)
	}

	st, err = store.State(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state after resuming = %q, want completed", st)
	}
	if len(sim.PullRequests) != 1 {
		t.Fatalf("expected a pull request to have been opened, got %+v", sim.PullRequests)
	}
}
