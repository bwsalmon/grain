package orchestrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
)

var baseTime = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) (*model.Store, context.Context) {
	t.Helper()
	db, err := dolt.Open(dolt.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded dolt: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	return store, ctx
}

// fileTask puts an approved (human-filed) task the way model.LandsQueued
// says a human-authored one should land -- queued, not proposed --
// matching e2e/e2e_test.go's own fileIssue helper.
func fileTask(t *testing.T, store *model.Store, ctx context.Context, tk model.Task) {
	t.Helper()
	actor := model.Principal{Kind: model.PrincipalHuman, ID: "alice"}
	tk.Origin = model.Origin{Attribution: model.Attribution{Actor: actor}, Reason: model.ReasonDirect}
	tk.Approval = &model.Attribution{Actor: actor}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatalf("filing task %s: %v", tk.ID, err)
	}
}

// --- fakes -------------------------------------------------------------

// agentFunc adapts a plain function to agent.Framework, so each test can
// script exactly the run it wants without a scripted Gemini transcript.
type agentFunc func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error)

func (f agentFunc) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	return f(ctx, cfg)
}

func pushed() *agent.Result {
	return &agent.Result{
		FinalText: "pushed the change",
		ToolCalls: []agent.ToolCall{{Name: "run_command", Text: "ok"}},
	}
}

func toolError(text string) *agent.Result {
	return &agent.Result{ToolCalls: []agent.ToolCall{{Name: "run_command", Text: text, IsError: true}}}
}

func askedQuestion(question string) *agent.Result {
	return &agent.Result{ToolCalls: []agent.ToolCall{
		{Name: "ask_question", Arguments: map[string]any{"question": question}},
	}}
}

func commentedOnIssue(comment string) *agent.Result {
	return &agent.Result{ToolCalls: []agent.ToolCall{
		{Name: "comment_on_issue", Arguments: map[string]any{"comment": comment}},
	}}
}

func proposedTask(title, body string) *agent.Result {
	return &agent.Result{ToolCalls: []agent.ToolCall{
		{Name: "propose_task", Arguments: map[string]any{"title": title, "body": body}},
	}}
}

// fakeGitHub implements github.Client by hand rather than scripting
// FakeTransport's JSON wire shapes -- github_test.go already proves
// RESTClient decodes the wire correctly; what this package's own tests
// need is control over which repo/branch has an open PR and to record
// what orchestrate asked for.
type fakeGitHub struct {
	mu            sync.Mutex
	branches      map[string]bool // "owner/repo/branch"
	openPRs       map[string]*github.PullRequest
	defaultBranch string
	created       []createdPR
	branchCalls   int
	comments      []createdComment
	issues        []createdIssue
	nextCommentID int
}

type createdPR struct {
	Owner, Repo, Head, Base, Title, Body string
}

type createdComment struct {
	Owner, Repo string
	Number      int
	Body        string
}

type createdIssue struct {
	Owner, Repo, Title, Body string
	Labels                   []string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		branches: map[string]bool{}, openPRs: map[string]*github.PullRequest{},
		defaultBranch: "main", nextCommentID: 1000,
	}
}

func key(owner, repo, branch string) string { return owner + "/" + repo + "/" + branch }

func (f *fakeGitHub) BranchExists(owner, repo, branch string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branchCalls++
	return f.branches[key(owner, repo, branch)], nil
}

func (f *fakeGitHub) FindOpenPullRequestForBranch(owner, repo, branch string) (*github.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openPRs[key(owner, repo, branch)], nil
}

func (f *fakeGitHub) CreatePullRequest(owner, repo, head, base, title, body string) (github.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, createdPR{owner, repo, head, base, title, body})
	pr := github.PullRequest{Number: len(f.created), HTMLURL: "https://example/pr/" + head}
	f.openPRs[key(owner, repo, head)] = &pr
	return pr, nil
}

func (f *fakeGitHub) DefaultBranch(owner, repo string) (string, error) { return f.defaultBranch, nil }

// closePR removes branch's open PR, the way GitHub itself would once a
// human merges or closes it -- what lets a sync test observe the
// "disappeared" half of syncGitHub's own logic.
func (f *fakeGitHub) closePR(owner, repo, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.openPRs, key(owner, repo, branch))
}

func (f *fakeGitHub) openPR(owner, repo, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branches[key(owner, repo, branch)] = true
	f.openPRs[key(owner, repo, branch)] = &github.PullRequest{Number: 1, HTMLURL: "https://example/pr/" + branch}
}

// The rest of github.Client is unused by orchestrate -- stubbed out flat
// so *fakeGitHub satisfies the interface.
func (f *fakeGitHub) ListIssues(owner, repo, label string) ([]github.Issue, error) { return nil, nil }
func (f *fakeGitHub) GetIssue(owner, repo string, n int) (github.Issue, error) {
	return github.Issue{}, nil
}
func (f *fakeGitHub) AddLabel(owner, repo string, n int, label string) error    { return nil }
func (f *fakeGitHub) RemoveLabel(owner, repo string, n int, label string) error { return nil }
func (f *fakeGitHub) CloseIssue(owner, repo string, n int) error                { return nil }
func (f *fakeGitHub) ReopenIssue(owner, repo string, n int) error               { return nil }
func (f *fakeGitHub) GetBranchHead(owner, repo, branch string) (*github.BranchHead, error) {
	return nil, nil
}
func (f *fakeGitHub) CreateIssue(owner, repo, title, body string, labels []string) (github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues = append(f.issues, createdIssue{owner, repo, title, body, labels})
	return github.Issue{Number: len(f.issues), Title: title, Body: body}, nil
}
func (f *fakeGitHub) MergePullRequest(owner, repo string, n int) error { return nil }
func (f *fakeGitHub) GetPullRequest(owner, repo string, n int) (github.PullRequestDetail, error) {
	return github.PullRequestDetail{}, nil
}
func (f *fakeGitHub) ListReviewComments(owner, repo string, n int) ([]github.ReviewComment, error) {
	return nil, nil
}
func (f *fakeGitHub) ListCheckRuns(owner, repo, ref string) ([]github.CheckRun, error) {
	return nil, nil
}
func (f *fakeGitHub) ListComments(owner, repo string, n int) ([]github.Comment, error) {
	return nil, nil
}
func (f *fakeGitHub) CreateComment(owner, repo string, n int, body string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, createdComment{owner, repo, n, body})
	f.nextCommentID++
	return f.nextCommentID - 1, nil
}
func (f *fakeGitHub) CreateReview(owner, repo string, n int, body string, comments []github.NewReviewComment) (int, error) {
	return 0, nil
}

// fakeCapability is a model.CapabilityProvider a test configures to
// refuse, or to mint a lease and a placement, recording every Revoke
// call it gets.
type fakeCapability struct {
	name    string
	refuse  string // non-empty means Resolve refuses with this reason
	path    string // placement path (absolute, like a real provider's)
	content string

	mu      sync.Mutex
	revoked []model.Lease
}

func (c *fakeCapability) Spec() model.CapabilitySpec {
	return model.CapabilitySpec{Name: c.name, Provision: model.ProvisionMint}
}

func (c *fakeCapability) Resolve(ctx context.Context, cc model.CapabilityContext) (model.Resolution, error) {
	if c.refuse != "" {
		return model.RefusedBecause(c.refuse), nil
	}
	return model.Honoured(), nil
}

func (c *fakeCapability) Materialize(ctx context.Context, cc model.CapabilityContext) (model.Materialization, error) {
	return model.Materialization{
		Lease: &model.Lease{Capability: c.name, Resource: "res-1", MintedBy: model.CredentialRef{Name: "test"}, IssuedAt: cc.Now},
		Placements: []model.Placement{
			{Side: model.SideSandbox, Path: c.path, Content: c.content},
		},
	}, nil
}

func (c *fakeCapability) PromptSection(ctx context.Context, cc model.CapabilityContext, placements []model.Placement) (string, error) {
	return "capability " + c.name + " is ready", nil
}

func (c *fakeCapability) Revoke(ctx context.Context, cc model.CapabilityContext, lease model.Lease) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = append(c.revoked, lease)
	return nil
}

// --- tests ---------------------------------------------------------

func TestTickDispatchesRunsTheAgentAndFinishesTheRun(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
	})

	root := t.TempDir()
	var gotPrompt, gotRoot string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt, gotRoot = cfg.Prompt, cfg.SandboxRoot
		return pushed(), nil
	})

	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": root},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(),
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}

	if gotRoot != root {
		t.Errorf("agent ran against root %q, want %q", gotRoot, root)
	}
	if gotPrompt != "Do the thing\n\ndetails" {
		t.Errorf("prompt = %q", gotPrompt)
	}
	if n, _ := store.Attempts(ctx, "t1"); n != 1 {
		t.Errorf("Attempts = %d, want 1", n)
	}
	if occ, _ := store.OccupiedSlots(ctx); len(occ) != 0 {
		t.Errorf("occupied slots after finish = %v, want none", occ)
	}
}

func TestTickFailsTheRunWhenACapabilityIsRefused(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "Do the thing",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
		Grants: []model.Grant{{Capability: "locked", Via: model.GrantByLabel}},
	})

	ran := false
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		ran = true
		return pushed(), nil
	})
	cap := &fakeCapability{name: "locked", refuse: "not for you"}

	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": t.TempDir()},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(cap),
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}

	if ran {
		t.Fatal("agent must not run when a capability was refused")
	}
	// A failed run returns the task straight to queued, for a retry --
	// the same semantics e2e's TestFailedRunReturnsTaskToQueueForRetry
	// exercises one layer up, through a real push denial instead of a
	// refused capability.
	state, err := store.State(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateQueued {
		t.Errorf("state = %q, want %q", state, model.StateQueued)
	}
}

func TestTickMaterializesAppliesPromptsAndRevokesACapability(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "Do the thing",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
		Grants: []model.Grant{{Capability: "keyed", Via: model.GrantByLabel}},
	})

	root := t.TempDir()
	var gotPrompt string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		return pushed(), nil
	})
	cap := &fakeCapability{name: "keyed", path: "/home/debian/.secret", content: "sh-sh-sh"}

	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": root},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(cap),
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}

	if want := "capability keyed is ready"; !strings.Contains(gotPrompt, want) {
		t.Errorf("prompt %q does not mention %q", gotPrompt, want)
	}
	placed := filepath.Join(root, "home/debian/.secret")
	data, err := os.ReadFile(placed)
	if err != nil {
		t.Fatalf("placement was not written to %s: %v", placed, err)
	}
	if string(data) != "sh-sh-sh" {
		t.Errorf("placement content = %q", data)
	}

	if len(cap.revoked) != 1 || cap.revoked[0].Resource != "res-1" {
		t.Fatalf("revoked = %+v, want exactly one lease for res-1", cap.revoked)
	}
	live, err := store.LiveLeases(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("live leases after revoke = %+v, want none", live)
	}
}

func TestTickOpensAPullRequestAfterASuccessfulPush(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "Do the thing", Body: "why",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
	})

	gh := newFakeGitHub()
	branch := model.BranchName("t1")
	gh.branches[key("acme", "widgets", branch)] = true // the agent "pushed" it

	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) { return pushed(), nil })
	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": t.TempDir()},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(), GitHub: gh,
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}

	if len(gh.created) != 1 {
		t.Fatalf("created PRs = %+v, want exactly one", gh.created)
	}
	got := gh.created[0]
	if got.Owner != "acme" || got.Repo != "widgets" || got.Head != branch || got.Base != "main" || got.Title != "Do the thing" {
		t.Errorf("created PR = %+v", got)
	}
}

func TestTickNeverOpensAPullRequestWithNoGitHubClientConfigured(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "Do the thing",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
	})
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) { return pushed(), nil })
	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": t.TempDir()},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(),
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}
	if state, _ := store.State(ctx, "t1"); state != model.StateQueued {
		t.Errorf("state = %q, want queued (nothing observed the push)", state)
	}
}

func TestTickFailsTheRunWhenTheAgentErrors(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "Do the thing",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
	})
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return nil, errors.New("boom")
	})
	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": t.TempDir()},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(),
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}
	if state, _ := store.State(ctx, "t1"); state != model.StateQueued {
		t.Errorf("state = %q, want queued for a retry", state)
	}
}

func TestTickFailsTheRunWhenAToolCallErrors(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "Do the thing",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
	})
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return toolError("permission denied"), nil
	})
	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": t.TempDir()},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(),
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}
	if state, _ := store.State(ctx, "t1"); state != model.StateQueued {
		t.Errorf("state = %q, want queued for a retry", state)
	}
}

func TestSyncGitHubMarksATaskCompletedWhenAPullRequestAppearsOpen(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "x",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
	})
	if err := store.StartRun(ctx, model.Run{
		ID: "t1-r1", TaskID: "t1", Slot: "local", Sandbox: "local", Attempt: 1, StartedAt: baseTime,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "t1-r1", baseTime.Add(time.Minute), "succeeded"); err != nil {
		t.Fatal(err)
	}

	gh := newFakeGitHub()
	gh.openPR("acme", "widgets", model.BranchName("t1"))

	r := New(Config{Store: store, Capabilities: model.NewCapabilityRegistry(), GitHub: gh})
	if err := r.Tick(ctx, baseTime.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	state, err := store.State(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateCompleted {
		t.Fatalf("state = %q, want completed", state)
	}
}

func TestSyncGitHubClosesATaskWhenItsOpenPullRequestDisappears(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "x",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
	})
	completedAt := baseTime
	if err := store.Observe(ctx, model.Observation{TaskID: "t1", CompletedAt: &completedAt}); err != nil {
		t.Fatal(err)
	}

	gh := newFakeGitHub() // no open PR -- it merged or was closed

	r := New(Config{Store: store, Capabilities: model.NewCapabilityRegistry(), GitHub: gh})
	if err := r.Tick(ctx, baseTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	state, err := store.State(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateClosed {
		t.Fatalf("state = %q, want closed", state)
	}
}

func TestTickParksATaskAndPostsARealCommentWhenTheAgentAsksAQuestion(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "acme/tasks/1", Intent: model.IntentImplement, Title: "Do the thing",
		Target:      &model.RepoRef{Owner: "acme", Name: "widgets"},
		Binding:     model.BindingDirective,
		ExternalRef: model.ExternalRef(model.RepoRef{Owner: "acme", Name: "tasks"}, 1),
	})

	gh := newFakeGitHub()
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return askedQuestion("Which repo should this land in?"), nil
	})
	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": t.TempDir()},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(), GitHub: gh,
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}

	if len(gh.comments) != 1 {
		t.Fatalf("comments = %+v, want exactly one", gh.comments)
	}
	got := gh.comments[0]
	if got.Owner != "acme" || got.Repo != "tasks" || got.Number != 1 || got.Body != "Which repo should this land in?" {
		t.Errorf("posted comment = %+v", got)
	}
	if len(gh.created) != 0 {
		t.Errorf("created PRs = %+v, want none -- a question ends the turn", gh.created)
	}

	state, err := store.State(ctx, "acme/tasks/1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateAwaitingReply {
		t.Fatalf("state = %q, want awaiting_reply", state)
	}
}

func TestTickClosesATaskWithARealCommentWhenTheAgentAnswersWithoutPushing(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "acme/tasks/2", Intent: model.IntentAnalyze, Title: "What does this do?",
		Target:      &model.RepoRef{Owner: "acme", Name: "widgets"},
		Binding:     model.BindingDirective,
		ExternalRef: model.ExternalRef(model.RepoRef{Owner: "acme", Name: "tasks"}, 2),
	})

	gh := newFakeGitHub()
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return commentedOnIssue("It computes the widget count."), nil
	})
	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": t.TempDir()},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(), GitHub: gh,
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}

	if len(gh.comments) != 1 {
		t.Fatalf("comments = %+v, want exactly one", gh.comments)
	}
	got := gh.comments[0]
	if got.Owner != "acme" || got.Repo != "tasks" || got.Number != 2 || got.Body != "It computes the widget count." {
		t.Errorf("posted comment = %+v", got)
	}

	// Closed outright, not merely completed -- see closeWithComment's own
	// doc comment on why a comment-only task (no pull request ever) must
	// not be left for syncGitHub's branch-existence re-derivation to
	// mistake for a since-merged-or-closed one.
	state, err := store.State(ctx, "acme/tasks/2")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateClosed {
		t.Fatalf("state = %q, want closed", state)
	}
}

func TestTickFilesAProposedTaskAsARealIssue(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "acme/tasks/3", Intent: model.IntentImplement, Title: "Do the thing", Body: "why",
		Target:      &model.RepoRef{Owner: "acme", Name: "widgets"},
		Binding:     model.BindingDirective,
		ExternalRef: model.ExternalRef(model.RepoRef{Owner: "acme", Name: "tasks"}, 3),
	})

	gh := newFakeGitHub()
	branch := model.BranchName("acme/tasks/3")
	gh.branches[key("acme", "widgets", branch)] = true // the agent pushed, and also proposed a follow-up

	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		result := pushed()
		result.ToolCalls = append(result.ToolCalls, agent.ToolCall{
			Name:      "propose_task",
			Arguments: map[string]any{"title": "Follow-up", "body": "worth doing next"},
		})
		return result, nil
	})
	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": t.TempDir()},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(), GitHub: gh,
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}

	if len(gh.issues) != 1 {
		t.Fatalf("filed issues = %+v, want exactly one", gh.issues)
	}
	got := gh.issues[0]
	if got.Owner != "acme" || got.Repo != "tasks" || got.Title != "Follow-up" || got.Body != "worth doing next" {
		t.Errorf("filed issue = %+v", got)
	}
	// The push still opened its own pull request -- propose_task
	// accompanies other work rather than replacing it.
	if len(gh.created) != 1 {
		t.Errorf("created PRs = %+v, want exactly one", gh.created)
	}
}

func TestTickRelaysNothingWhenATaskHasNoTrackingIssue(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "Do the thing",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
	})

	gh := newFakeGitHub()
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return askedQuestion("Which repo?"), nil
	})
	r := New(Config{
		Store: store, Slots: []string{"local"}, Roots: map[string]string{"local": t.TempDir()},
		Agent: fw, Capabilities: model.NewCapabilityRegistry(), GitHub: gh,
	})
	if err := r.Tick(ctx, baseTime); err != nil {
		t.Fatal(err)
	}

	if len(gh.comments) != 0 {
		t.Errorf("comments = %+v, want none -- t1 has no ExternalRef to relay to", gh.comments)
	}
	// Nothing to park against either -- the task lands back in queued,
	// the same as any other run that made a tool call and produced
	// nothing to act on.
	state, err := store.State(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateQueued {
		t.Errorf("state = %q, want queued", state)
	}
}

func TestSyncGitHubIgnoresATaskWithNoGitHubClientConfigured(t *testing.T) {
	store, ctx := newStore(t)
	fileTask(t, store, ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "x",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Binding: model.BindingDirective,
	})
	if err := store.StartRun(ctx, model.Run{
		ID: "t1-r1", TaskID: "t1", Slot: "local", Sandbox: "local", Attempt: 1, StartedAt: baseTime,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "t1-r1", baseTime.Add(time.Minute), "succeeded"); err != nil {
		t.Fatal(err)
	}

	r := New(Config{Store: store, Capabilities: model.NewCapabilityRegistry()})
	if err := r.Tick(ctx, baseTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if state, _ := store.State(ctx, "t1"); state != model.StateQueued {
		t.Errorf("state = %q, want queued (no GitHub client to observe anything)", state)
	}
}
