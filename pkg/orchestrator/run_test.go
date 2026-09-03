package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/dispatch"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// agentFunc adapts a plain function to agent.Framework, so each test can
// script exactly the run it wants without a scripted Gemini transcript --
// ported from pkg/orchestrate's own test helper (bwsalmon/agents#254) when
// that package's capability-handling tests moved here (bwsalmon/agents#263).
type agentFunc func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error)

func (f agentFunc) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	return f(ctx, cfg)
}

// openingFramework is an agentFunc that also implements
// agent.PullRequestFramework, standing in for a real Framework built
// WithGrainServer -- the only kind whose runs are offered
// open_pull_request at all.
type openingFramework struct{ agentFunc }

func (openingFramework) CanOpenPullRequest() bool { return true }

func pushed() *agent.Result {
	return &agent.Result{
		FinalText: "pushed the change",
		ToolCalls: []agent.ToolCall{{Name: "run_command", Text: "ok"}},
	}
}

// fakeCapability is a model.CapabilityProvider a test configures to
// refuse, or to mint a lease and a placement, recording every Revoke call
// it gets. Ported from pkg/orchestrate's own test helper (bwsalmon/agents#254).
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

// dispatchTask puts an approved (human-filed) task directly, standing in
// for what dispatch.Cycle would already have found ready by the time
// RunDispatch runs -- these tests are about what RunDispatch does with a
// task's own Grants, not about approval or scheduling.
func dispatchTask(t *testing.T, ctx context.Context, store *model.Store, id string, grants ...model.Grant) model.Task {
	t.Helper()
	human := model.Principal{Kind: model.PrincipalHuman, ID: "alice"}
	task := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "Do the thing", Body: "details",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human},
		Target:   &model.RepoRef{Owner: "acme", Name: "widgets"},
		Binding:  model.BindingDirective,
		Grants:   grants,
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("filing task %s: %v", id, err)
	}
	return task
}

// startRun records the task_run row dispatch.Cycle would already have written
// by the time RunDispatch ever sees a Dispatch -- RunDispatch only ever
// UPDATEs it (via store.FinishRun), the same "the run is already durable"
// assumption pkg/orchestrate's own runDispatch documented.
func startRun(t *testing.T, ctx context.Context, store *model.Store, d dispatch.Dispatch, at time.Time) {
	t.Helper()
	if err := store.StartRun(ctx, model.Run{
		ID: d.RunID, TaskID: d.TaskID, Sandbox: d.RunID, Attempt: d.Attempt, StartedAt: at,
	}, model.Limits{}); err != nil {
		t.Fatalf("starting run %s: %v", d.RunID, err)
	}
}

// BuildPrompt tells the agent about a read-only repo, but the wording
// makes clear it grants nothing beyond a fetch -- gitproxy/authorize.go
// is what actually refuses a push to one; this only informs.
func TestBuildPromptMentionsReadOnlyRepos(t *testing.T) {
	task := model.Task{
		ID: "t1", Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"},
		Reads: []model.RepoRef{
			{Owner: "acme", Name: "shared-lib"},
			{Owner: "acme", Name: "schema"},
		},
	}
	prompt := orchestrator.BuildPrompt(task, "", false)
	if !strings.Contains(prompt, "acme/shared-lib") || !strings.Contains(prompt, "acme/schema") {
		t.Fatalf("prompt does not mention both read-only repos: %q", prompt)
	}
	if !strings.Contains(prompt, "never push") {
		t.Fatalf("prompt does not warn against pushing to a read-only repo: %q", prompt)
	}
}

// With a checkout prepared for it (RunDispatch's own prepareCheckout),
// the prompt says so -- where the clone is, which branch is checked out,
// and that pushing is all the git the agent has left to do. Without one
// the wording is unchanged, which is what every deployment running no
// proxy, and every other test here, still gets.
func TestBuildPromptNamesAPreparedCheckout(t *testing.T) {
	task := model.Task{
		ID: "t1", Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"},
	}
	prompt := orchestrator.BuildPrompt(task, orchestrator.CheckoutDir, false)
	for _, want := range []string{"./" + orchestrator.CheckoutDir, model.BranchName("t1"), "rather than cloning"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt does not mention %q: %q", want, prompt)
		}
	}
	// Against the checkout sentence's own phrasing rather than against
	// CheckoutDir alone: that constant is "work", an ordinary English word
	// the rest of the prompt is entitled to use (proposalSection does), so
	// a bare substring search for it fails on prose that mentions no
	// checkout at all. "./work" and "rather than cloning" are what only
	// that sentence says, which is what this is checking is absent.
	bare := orchestrator.BuildPrompt(task, "", false)
	for _, unwanted := range []string{"./" + orchestrator.CheckoutDir, "rather than cloning"} {
		if strings.Contains(bare, unwanted) {
			t.Fatalf("prompt mentions %q, a checkout that was never prepared: %q", unwanted, bare)
		}
	}
}

// The push/check/repair loop pull_request_status exists for is only
// usable if the prompt says it is there: nothing about the tool's own
// description tells a run that it may push more than once, and the
// sentences before it read like one final act.
func TestBuildPromptDescribesThePushAndCheckCILoop(t *testing.T) {
	task := model.Task{
		ID: "t1", Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"},
	}
	prompt := orchestrator.BuildPrompt(task, orchestrator.CheckoutDir, false)
	for _, want := range []string{"pull_request_status", "Push as often as you like", "check fails"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not mention %q: %q", want, prompt)
		}
	}

	// A task with no repo has no branch to push and no CI to watch, so
	// the paragraph must not appear at all -- the same reason the
	// pushing/branching sentences do not.
	bare := orchestrator.BuildPrompt(model.Task{ID: "t2", Title: "Think", Body: "details"}, "", false)
	if strings.Contains(bare, "pull_request_status") {
		t.Errorf("prompt offers a CI tool to a task with no repo: %q", bare)
	}
}

// The other half of that loop: when it is over. Every sentence before it
// is about how to push and how to look, and a run that pushed once, read
// one status and stopped has obeyed all of them -- so the prompt says
// outright that unfinished checks are not passes and that a conflict
// with the base is the run's own to resolve while it still has the
// checkout.
func TestBuildPromptSaysGreenChecksAndACleanMergeAreTheFinishLine(t *testing.T) {
	task := model.Task{
		ID: "t1", Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"},
	}
	prompt := orchestrator.BuildPrompt(task, orchestrator.CheckoutDir, false)
	for _, want := range []string{"not done", "merges cleanly", "carries no verdict", "conflicts with", "git fetch origin"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not mention %q: %q", want, prompt)
		}
	}
	// With no /base directive grain does not know the repo's default
	// branch, so the sentence describes the base rather than naming one:
	// a guessed branch name is one an agent would try to merge and fail.
	if strings.Contains(prompt, "conflicts with `") {
		t.Errorf("prompt names a base branch this task never set: %q", prompt)
	}

	task.Base = "release-2"
	based := orchestrator.BuildPrompt(task, orchestrator.CheckoutDir, false)
	if !strings.Contains(based, "`release-2`") {
		t.Errorf("prompt does not name the base this task is built on: %q", based)
	}

	// A task with no repo has no branch, no base and no CI, the same
	// reason the paragraph above it is absent.
	bare := orchestrator.BuildPrompt(model.Task{ID: "t2", Title: "Think", Body: "details"}, "", false)
	if strings.Contains(bare, "merges cleanly") {
		t.Errorf("prompt talks about merging to a task with no repo: %q", bare)
	}
}

// open_pull_request is only on a run's roster when the Framework driving
// it was given a daemon to ask (agent.PullRequestFramework), so the
// prompt names it on exactly that condition: a run that has it and is
// never told finishes without ever seeing its own CI, which is the whole
// thing the tool exists to fix, and a run told to call one it does not
// have burns turns on an error it cannot do anything about.
func TestBuildPromptOffersOpenPullRequestOnlyToARunThatHasIt(t *testing.T) {
	task := model.Task{
		ID: "t1", Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"},
	}
	prompt := orchestrator.BuildPrompt(task, orchestrator.CheckoutDir, true)
	for _, want := range []string{"open_pull_request", "push again", "never opens a second one"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not mention %q: %q", want, prompt)
		}
	}

	without := orchestrator.BuildPrompt(task, orchestrator.CheckoutDir, false)
	if strings.Contains(without, "open_pull_request") {
		t.Errorf("prompt names a tool this run's mcpserver never registered: %q", without)
	}

	// A task with no repo has no branch, no pull request and no CI, so
	// the sentence has nothing to attach itself to even where the tool is
	// registered -- registration turns on -server/-task, neither of which
	// knows whether the task has a target.
	bare := orchestrator.BuildPrompt(model.Task{ID: "t2", Title: "Think", Body: "details"}, "", true)
	if strings.Contains(bare, "open_pull_request") {
		t.Errorf("prompt offers to open a pull request for a task with no repo: %q", bare)
	}
}

// The prompt's half of the fact only the Framework holds: RunDispatch
// asks the one it is about to run, so a deployment whose daemon serves a
// UI/API tells its runs about open_pull_request and one without says
// nothing. openingFramework is the "yes" answer; a bare agentFunc, which
// implements no such method, is every other test here saying "no".
func TestRunDispatchTellsARunItCanOpenItsOwnPullRequest(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	var gotPrompt string
	fw := openingFramework{agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		return pushed(), nil
	})}
	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if !strings.Contains(gotPrompt, "open_pull_request") {
		t.Errorf("prompt = %q, want it to name open_pull_request -- this framework's runs have it", gotPrompt)
	}
}

// A task filed with CreateTaskRequest.NoRepo (bwsalmon/agents#614) has a
// nil Target -- prepareCheckout already skips cloning outright for one
// (checkout.go), so this is the other half: the prompt has to say so in
// words, rather than formatting a nil *model.RepoRef with %s and reading
// like a clone that silently failed.
func TestBuildPromptExplainsThereIsNoRepo(t *testing.T) {
	task := model.Task{ID: "t1", Title: "Do the thing", Body: "details"}
	prompt := orchestrator.BuildPrompt(task, "", false)
	if !strings.Contains(prompt, "no repo") {
		t.Fatalf("prompt does not say there is no repo: %q", prompt)
	}
	if strings.Contains(prompt, "Push your change") || strings.Contains(prompt, "<nil>") {
		t.Fatalf("prompt still talks about pushing/branching, or leaked a nil format: %q", prompt)
	}
}

// An agent can only follow propose_task's own etiquette if it knows two
// things about itself grain never otherwise tells it: which task it is
// (what a proposal's depends_on names) and whether that task auto-merges
// (what caps a proposal's auto_merge -- orchestrator.proposedAutoMerge).
func TestBuildPromptTellsAnAgentWhatItsProposalsCanSay(t *testing.T) {
	task := model.Task{
		ID: "t1", Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"},
	}
	prompt := orchestrator.BuildPrompt(task, "", false)
	for _, want := range []string{"task t1", "depends_on"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not mention %q: %q", want, prompt)
		}
	}
	// Nothing to say about auto_merge to a task that cannot pass it on.
	if strings.Contains(prompt, "auto_merge") {
		t.Errorf("prompt offers auto_merge to a task that is not an auto-merge job: %q", prompt)
	}

	task.AutoMerge = true
	prompt = orchestrator.BuildPrompt(task, "", false)
	if !strings.Contains(prompt, "auto_merge") {
		t.Errorf("prompt does not tell an auto-merge job it can pass that on: %q", prompt)
	}
}

// Matched against the Reads section's own opening words rather than the
// bare substring "read", which this used to look for: every other
// sentence BuildPrompt writes is free to use the word for something
// else, and the first one that did (the pull_request_status paragraph:
// "see what GitHub's checks made of it") would have failed this test
// while the Reads section was correctly absent.
func TestBuildPromptOmitsReadsSectionWhenThereAreNone(t *testing.T) {
	task := model.Task{
		ID: "t1", Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"},
	}
	prompt := orchestrator.BuildPrompt(task, "", false)
	if strings.Contains(prompt, "You may also read") {
		t.Fatalf("prompt mentions reading a repo with no Reads set: %q", prompt)
	}
}

func TestRunDispatchMaterializesAppliesPromptsAndRevokesACapability(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1", model.Grant{Capability: "keyed", Via: model.GrantByLabel})
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	root := t.TempDir()
	var gotPrompt string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		return pushed(), nil
	})
	cap := &fakeCapability{name: "keyed", path: "/home/debian/.secret", content: "sh-sh-sh"}
	cfg := orchestrator.Config{Capabilities: model.NewCapabilityRegistry(cap)}

	result, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, root, "", nil, baseTime)
	if err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if result == nil {
		t.Fatal("expected a result")
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

// TestRunDispatchPlacesAttachmentsAndMentionsThemInThePrompt covers
// bwsalmon/agents#522: a file the task carries (or one carried by a
// comment in its conversation) lands in the sandbox and is named in the
// prompt, even for a task with no capability Grants at all -- unlike a
// capability's own SideSandbox placement, an attachment is written via
// the slot's own write_file tool (orchestrator.placeAttachments), not
// applyPlacements, which is why this test passes real tools rather than
// the nil every other RunDispatch test in this file gets away with.
func TestRunDispatchPlacesAttachmentsAndMentionsThemInThePrompt(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	if _, err := store.AddAttachment(ctx, model.Attachment{
		TaskID: "t1", Filename: "repro.zip", ContentType: "application/zip",
		Content: []byte("PK\x03\x04fake"), Size: 9, CreatedAt: baseTime,
	}); err != nil {
		t.Fatalf("adding attachment: %v", err)
	}
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	root := t.TempDir()
	var gotPrompt string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		return pushed(), nil
	})

	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, mcp.NewSandboxTools(root), root, "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}

	wantPath := orchestrator.AttachmentsDir + "/1-repro.zip"
	if !strings.Contains(gotPrompt, wantPath) {
		t.Errorf("prompt %q does not mention %q", gotPrompt, wantPath)
	}
	got, err := os.ReadFile(filepath.Join(root, wantPath))
	if err != nil {
		t.Fatalf("attachment was not written into the sandbox: %v", err)
	}
	if string(got) != "PK\x03\x04fake" {
		t.Errorf("attachment content = %q", got)
	}
}

// TestRunDispatchRecordsTheAgentsTranscript covers bwsalmon/agents#446:
// a framework's own agent.Result.Transcript should end up readable back
// off the store, against the task and attempt number RunDispatch was
// given -- the one thing FinishRun's own outcome/detail never carried.
func TestRunDispatchRecordsTheAgentsTranscript(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return &agent.Result{
			FinalText:  "pushed the change",
			ToolCalls:  []agent.ToolCall{{Name: "run_command", Text: "ok"}},
			Transcript: "> run_command(...)\nok\n\npushed the change",
		}, nil
	})
	cfg := orchestrator.Config{}

	if _, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}

	transcript, found, err := store.RunTranscript(ctx, "t1", 1)
	if err != nil || !found {
		t.Fatalf("RunTranscript: (%q, %v, %v)", transcript, found, err)
	}
	if !strings.Contains(transcript, "pushed the change") {
		t.Errorf("transcript = %q, want it to contain the agent's own transcript text", transcript)
	}
}

// TestRunDispatchRecordsThePromptItGaveTheAgent covers grain/task-91's
// half of "show the full prompt": whatever RunDispatch actually handed
// framework.Run has to be readable back off the store afterwards, since
// the prompt is assembled once from a task and a conversation that both
// move on afterwards, and nothing else records it.
//
// It is recorded before the agent's first turn, not after it returns, so
// a run still in flight can be asked what it was told -- which is what
// the agent's own view of the store proves here: by the time
// framework.Run is called, the row already carries it.
func TestRunDispatchRecordsThePromptItGaveTheAgent(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	var gotPrompt, recordedDuringRun string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		recordedDuringRun, _, _ = store.RunPrompt(ctx, "t1", 1)
		return pushed(), nil
	})

	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}

	prompt, found, err := store.RunPrompt(ctx, "t1", 1)
	if err != nil || !found {
		t.Fatalf("RunPrompt: (%q, %v, %v)", prompt, found, err)
	}
	if prompt != gotPrompt {
		t.Errorf("recorded prompt = %q, want exactly what the agent was given: %q", prompt, gotPrompt)
	}
	if recordedDuringRun != gotPrompt {
		t.Errorf("prompt readable mid-run = %q, want it already recorded before the agent's first turn", recordedDuringRun)
	}
}

func TestRunDispatchFinishesTheRunAsFailedWhenACapabilityIsRefused(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1", model.Grant{Capability: "locked", Via: model.GrantByLabel})
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	ran := false
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		ran = true
		return pushed(), nil
	})
	cap := &fakeCapability{name: "locked", refuse: "not for you"}
	cfg := orchestrator.Config{Capabilities: model.NewCapabilityRegistry(cap)}

	if _, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), "", nil, baseTime); err == nil {
		t.Fatal("expected RunDispatch to report the refusal")
	}
	if ran {
		t.Fatal("agent must not run when a capability was refused")
	}

	// A failed run still gets finished -- an unfinished one would hold its
	// slot forever -- and returns the task straight to queued, for a
	// retry, the same semantics e2e's TestFailedRunReturnsTaskToQueueForRetry
	// exercises one layer up through a real push denial instead.
	occupied, err := store.LiveRunCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if occupied != 0 {
		t.Errorf("occupied slots after a refused capability = %v, want none", occupied)
	}
	state, err := store.State(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateQueued {
		t.Errorf("state = %q, want %q", state, model.StateQueued)
	}
}

// The line pkg/metrics splits a run's own duration at: everything before
// the agent's first turn is setup this deployment could speed up (a
// sandbox, a clone, a capability mint), and everything after it is the
// agent framework's own. Recorded by RunDispatch, because it is the only
// thing that knows the moment it hands the run over.
func TestRunDispatchRecordsWhenItsAgentStarted(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	// The run is dispatched at baseTime and its agent only gets going
	// four minutes later, which is exactly the gap worth measuring.
	agentStarted := baseTime.Add(4 * time.Minute)
	cfg := orchestrator.Config{Now: func() time.Time { return agentStarted }}
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return pushed(), nil
	})

	if _, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}

	runs, err := store.RunTimings(ctx)
	if err != nil {
		t.Fatalf("RunTimings: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("read %d run timings, want 1", len(runs))
	}
	if runs[0].AgentStartedAt == nil || !runs[0].AgentStartedAt.Equal(agentStarted) {
		t.Errorf("AgentStartedAt = %v, want %v", runs[0].AgentStartedAt, agentStarted)
	}
}

// A run whose capability is refused never reaches an agent at all, so
// there is no agent start to record -- and recording one anyway would
// report setup latency for a run that never finished setting up.
func TestRunDispatchRecordsNoAgentStartWhenTheAgentNeverRan(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1", model.Grant{Capability: "locked", Via: model.GrantByLabel})
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return pushed(), nil
	})
	cfg := orchestrator.Config{
		Capabilities: model.NewCapabilityRegistry(&fakeCapability{name: "locked", refuse: "not for you"}),
	}

	if _, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), "", nil, baseTime); err == nil {
		t.Fatal("RunDispatch succeeded, want the refused capability to fail the run")
	}

	runs, err := store.RunTimings(ctx)
	if err != nil {
		t.Fatalf("RunTimings: %v", err)
	}
	if len(runs) != 1 || runs[0].AgentStartedAt != nil {
		t.Fatalf("runs = %+v, want one attempt with no agent start", runs)
	}
}

func TestRunDispatchFailsARunThatMadeNoToolCall(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return &agent.Result{FinalText: "nothing to do here"}, nil
	})

	result, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime)
	if err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if result == nil || len(result.ToolCalls) != 0 {
		t.Fatalf("result = %+v, want the agent's own (tool-call-less) result back", result)
	}

	occupied, err := store.LiveRunCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if occupied != 0 {
		t.Errorf("occupied slots after a tool-call-less run = %v, want none", occupied)
	}
	state, err := store.State(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateQueued {
		t.Errorf("state = %q, want %q (a failed run with nothing observed is eligible for retry)", state, model.StateQueued)
	}
}

// TestRunDispatchIncludesTheCommentThreadOnARedispatch is
// bwsalmon/agents#402's own scenario: a run parks on ask_question,
// ProcessResult relays that question into the store the same way a real
// dispatch would, a human answers it exactly as `grain comment` or the UI
// would, and the task's second dispatch must see both -- otherwise the
// redispatched agent has no way to know its question was already
// answered and asks it again, forever. This drives two full RunDispatch
// calls (with a real ProcessResult and a real store-backed comment
// between them) rather than calling BuildPrompt directly, since the bug
// this reproduces was in what RunDispatch's caller never did, not in
// BuildPrompt itself.
func TestRunDispatchIncludesTheCommentThreadOnARedispatch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	_ = sim
	task := dispatchTask(t, ctx, store, "t1")
	task.Target = &model.RepoRef{Owner: "acme", Name: "widgets"}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("re-filing task with a real target: %v", err)
	}

	d1 := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d1, baseTime)

	const question = "should the new field be snake_case or camelCase?"
	fw1 := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return &agent.Result{ToolCalls: []agent.ToolCall{
			{Name: "ask_question", Arguments: map[string]any{"question": question}},
		}}, nil
	})
	got, err := store.GetTask(ctx, "t1")
	if err != nil || got == nil {
		t.Fatalf("reading task: %v", err)
	}
	result1, err := orchestrator.RunDispatch(ctx, store, fw1, orchestrator.Config{}, *got, d1, nil, t.TempDir(), "", nil, baseTime)
	if err != nil {
		t.Fatalf("first RunDispatch: %v", err)
	}
	if err := orchestrator.ProcessResult(ctx, store, client, *got, result1, d1.RunID, baseTime); err != nil {
		t.Fatalf("ProcessResult after the question: %v", err)
	}

	const answer = "snake_case, to match the rest of the schema"
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID:    "t1",
		Author:    model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "alice"}},
		Body:      answer,
		CreatedAt: baseTime.Add(time.Minute),
	}); err != nil {
		t.Fatalf("answering the question: %v", err)
	}

	d2 := dispatch.Dispatch{TaskID: "t1", RunID: "r2", Attempt: 2}
	startRun(t, ctx, store, d2, baseTime.Add(2*time.Minute))
	got2, err := store.GetTask(ctx, "t1")
	if err != nil || got2 == nil {
		t.Fatalf("reading task: %v", err)
	}

	var gotPrompt string
	fw2 := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		return pushed(), nil
	})
	if _, err := orchestrator.RunDispatch(ctx, store, fw2, orchestrator.Config{}, *got2, d2, nil, t.TempDir(), "", nil, baseTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("second RunDispatch: %v", err)
	}

	if !strings.Contains(gotPrompt, question) {
		t.Errorf("second prompt does not contain the question it asked itself: %q", gotPrompt)
	}
	if !strings.Contains(gotPrompt, answer) {
		t.Errorf("second prompt does not contain the human's answer: %q", gotPrompt)
	}
}

// TestRunDispatchOmitsTheCommentThreadOnAFirstDispatch checks that a
// task's first dispatch, with no conversation yet, gets exactly
// BuildPrompt's own prompt back -- no empty "conversation so far" section
// tacked onto every prompt whether or not there is anything to say.
func TestRunDispatchOmitsTheCommentThreadOnAFirstDispatch(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	var gotPrompt string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		return pushed(), nil
	})
	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if gotPrompt != orchestrator.BuildPrompt(*task, "", false) {
		t.Errorf("prompt = %q, want exactly BuildPrompt's own prompt with no conversation yet", gotPrompt)
	}
}

// TestRunDispatchLetsAFrameworkPollForACommentAddedMidRun is bwsalmon/
// agents#523's own scenario: a comment posted while a run is still live
// has to reach a Framework whose own loop polls for one
// (agent.RunConfig.Addenda), not just the task's next dispatch. fw here
// plays that Framework itself, polling before and after adding a comment
// mid-run to prove the second poll sees what the first could not have.
func TestRunDispatchLetsAFrameworkPollForACommentAddedMidRun(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	const addendum = "actually, use snake_case for the new field"
	var gotBefore, gotAfter []string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		var err error
		if gotBefore, err = cfg.Addenda(ctx); err != nil {
			t.Fatalf("polling before the comment landed: %v", err)
		}
		if _, err := store.AddComment(ctx, model.Comment{
			TaskID:    "t1",
			Author:    model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "alice"}},
			Body:      addendum,
			CreatedAt: baseTime.Add(time.Minute),
		}); err != nil {
			t.Fatalf("adding a comment mid-run: %v", err)
		}
		if gotAfter, err = cfg.Addenda(ctx); err != nil {
			t.Fatalf("polling after the comment landed: %v", err)
		}
		return pushed(), nil
	})
	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if len(gotBefore) != 0 {
		t.Errorf("polled before any comment existed, got %v, want none", gotBefore)
	}
	if len(gotAfter) != 1 || !strings.Contains(gotAfter[0], addendum) {
		t.Errorf("polled after a comment landed mid-run, got %v, want one containing %q", gotAfter, addendum)
	}
}

// TestRunDispatchSeedsTheAddendaCursorPastCommentsAlreadyInThePrompt
// checks the other half of the same rule: on a redispatch, a comment
// already folded into the prompt by commentThreadSection must not also
// come back out of the first Addenda poll -- a Framework that acted on
// both would fold the same comment into its conversation twice.
func TestRunDispatchSeedsTheAddendaCursorPastCommentsAlreadyInThePrompt(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID:    "t1",
		Author:    model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "alice"}},
		Body:      "already folded into the prompt",
		CreatedAt: baseTime,
	}); err != nil {
		t.Fatalf("adding a comment before dispatch: %v", err)
	}

	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	var got []string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		var err error
		if got, err = cfg.Addenda(ctx); err != nil {
			t.Fatalf("polling: %v", err)
		}
		return pushed(), nil
	})
	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Addenda returned a comment already folded into the prompt: %v", got)
	}
}

// closeTask marks id closed by writing task_observation directly, the
// same effect ui.Client.Close has -- restated here rather than importing
// pkg/ui, matching orchestrator_test.go's own "duplicated per file" style
// note on newSim.
func closeTask(t *testing.T, ctx context.Context, store *model.Store, id string, at time.Time) {
	t.Helper()
	if err := store.ObserveField(ctx, id, at, func(o *model.Observation) { o.ClosedAt = &at }); err != nil {
		t.Fatalf("closing task %s: %v", id, err)
	}
}

// TestRunDispatchCancelsTheAgentWhenItsTaskIsClosedMidFlight is
// bwsalmon/agents#346's own scenario: a task closed while its run is
// still live must actually stop that run's agent, not just prevent
// dispatch.Cycle from starting another one and ProcessResult from opening
// a pull request for it (tests/e2e/close_while_live_test.go already
// covered those two). fw here blocks on the very ctx RunDispatch hands
// framework.Run until it is cancelled, so this test only passes if
// closing the task mid-run actually reaches that ctx -- proving
// watchForTaskClosed's store-polled cancellation signal works end to
// end, deterministically and fast (CancelPollInterval is set to a few
// milliseconds), rather than relying on a real subprocess's own timing.
func TestRunDispatchCancelsTheAgentWhenItsTaskIsClosedMidFlight(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	started := make(chan struct{})
	fw := agentFunc(func(runCtx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		close(started)
		<-runCtx.Done()
		return nil, runCtx.Err()
	})
	cfg := orchestrator.Config{CancelPollInterval: 5 * time.Millisecond}

	type runOutcome struct {
		result *agent.Result
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		result, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), "", nil, baseTime)
		done <- runOutcome{result, err}
	}()

	<-started
	closeTask(t, ctx, store, "t1", baseTime)

	select {
	case out := <-done:
		if out.result != nil {
			t.Errorf("result = %+v, want nil for a run cancelled mid-flight", out.result)
		}
		if out.err == nil || !strings.Contains(out.err.Error(), "closed") {
			t.Errorf("RunDispatch err = %v, want an error naming the task's closure", out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunDispatch did not return after its task was closed mid-flight -- the agent's ctx was never cancelled")
	}

	occupied, err := store.LiveRunCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if occupied != 0 {
		t.Errorf("occupied slots after a cancelled run = %v, want none: FinishRun still frees the slot", occupied)
	}
}

// TestRunDispatchCancelsAnAgentThatOutlivesMaxRunRuntime is bwsalmon/
// agents#575's own regression test for the run-level wall-clock cap:
// v1 had AutomationConfig.max_runtime_minutes plus a sweeper for a run
// that is alive but stuck making no progress (e.g. a run_command with
// no timeout of its own, hung forever); v2 had nothing playing that
// role until Config.MaxRunRuntime. fw here blocks on the very ctx
// RunDispatch hands framework.Run until it is cancelled, exactly like
// TestRunDispatchCancelsTheAgentWhenItsTaskIsClosedMidFlight, except
// nothing ever closes the task here -- only MaxRunRuntime elapsing
// should end it.
func TestRunDispatchCancelsAnAgentThatOutlivesMaxRunRuntime(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	started := make(chan struct{})
	fw := agentFunc(func(runCtx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		close(started)
		<-runCtx.Done()
		return nil, runCtx.Err()
	})
	cfg := orchestrator.Config{MaxRunRuntime: 20 * time.Millisecond}

	type runOutcome struct {
		result *agent.Result
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		result, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), "", nil, baseTime)
		done <- runOutcome{result, err}
	}()

	<-started

	select {
	case out := <-done:
		if out.result != nil {
			t.Errorf("result = %+v, want nil for a run cancelled by MaxRunRuntime", out.result)
		}
		if out.err == nil || !strings.Contains(out.err.Error(), "wall-clock") {
			t.Errorf("RunDispatch err = %v, want an error naming the wall-clock limit", out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunDispatch did not return after MaxRunRuntime elapsed -- the agent's ctx was never cancelled")
	}

	occupied, err := store.LiveRunCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if occupied != 0 {
		t.Errorf("occupied slots after a MaxRunRuntime-cancelled run = %v, want none: FinishRun still frees the slot", occupied)
	}

	runs, err := store.Runs(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want exactly one", runs)
	}
	if runs[0].Outcome != "cancelled" {
		t.Errorf("run outcome = %q, want \"cancelled\"", runs[0].Outcome)
	}
	if !strings.Contains(runs[0].Detail, "wall-clock") {
		t.Errorf("run detail = %q, want it to name the wall-clock limit", runs[0].Detail)
	}
}

// TestRunDispatchNeverLetsAnAlreadyClosedTaskReachARealToolCall is the
// race tests/e2e/close_while_live_test.go itself exercises:
// dispatch.Cycle claims a slot while a task is still running, the task
// is closed before RunDispatch ever gets called for that
// already-claimed run, and only then does RunDispatch actually run.
// Leaving this to watchForTaskClosed's own polling ticker would make
// whether the agent's first tool call ever reaches a real sandbox a
// race against CancelPollInterval; RunDispatch instead checks
// synchronously, before framework.Run is ever invoked, which this
// proves by using the default (multi-second) CancelPollInterval and
// still finishing fast, and by checking that framework.Run's own ctx
// already reads cancelled the instant it starts.
func TestRunDispatchNeverLetsAnAlreadyClosedTaskReachARealToolCall(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}
	closeTask(t, ctx, store, "t1", baseTime)

	sawCancelledCtx := false
	fw := agentFunc(func(runCtx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		sawCancelledCtx = runCtx.Err() != nil
		return nil, runCtx.Err()
	})

	start := time.Now()
	// Config{} leaves CancelPollInterval at its multi-second default --
	// deliberately, so this test can only pass quickly because of the
	// synchronous check, not because a short poll interval happened to
	// win a race.
	result, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("RunDispatch took %s against an already-closed task, want near-instant (no waiting on CancelPollInterval)", elapsed)
	}

	if result != nil {
		t.Errorf("result = %+v, want nil for a task closed before its run ever started", result)
	}
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("err = %v, want an error naming the task's closure", err)
	}
	if !sawCancelledCtx {
		t.Error("framework.Run's own ctx was not already cancelled when it started -- an already-closed task's first tool call could still reach a real sandbox")
	}
}

// TestRunDispatchGivesTheFrameworkATranscriptPathAndCleansItUp proves the
// two halves of bwsalmon/agents#467's wiring: RunDispatch tells the
// framework where to mirror a still-running run's own transcript (a file
// named after the run ID, under Config.TranscriptDir, matching what
// claude.LiveTranscriptDir would look for), and removes that file once
// the run is over -- its own store row already carries the same story by
// then (SetRunTranscript, just above in RunDispatch), so nothing is ever
// left behind for a finished run to read back out of the file.
func TestRunDispatchGivesTheFrameworkATranscriptPathAndCleansItUp(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	transcriptDir := t.TempDir()
	var gotPath string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPath = cfg.TranscriptPath
		if err := os.WriteFile(gotPath, []byte("still going"), 0o644); err != nil {
			t.Fatalf("writing transcript file: %v", err)
		}
		return pushed(), nil
	})
	cfg := orchestrator.Config{TranscriptDir: transcriptDir}

	if _, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}

	if want := filepath.Join(transcriptDir, "r1"); gotPath != want {
		t.Errorf("TranscriptPath = %q, want %q", gotPath, want)
	}
	if _, err := os.Stat(gotPath); !os.IsNotExist(err) {
		t.Errorf("transcript file %s still exists after RunDispatch returned, want it removed", gotPath)
	}
}

// TestRunDispatchLeavesTranscriptPathEmptyWithoutATranscriptDir proves
// the opt-in half of the same wiring: a deployment that never sets
// Config.TranscriptDir gets the pre-bwsalmon/agents#467 behaviour back,
// exactly -- no file, no path, nothing for a framework that doesn't look
// at RunConfig.TranscriptPath to trip over.
func TestRunDispatchLeavesTranscriptPathEmptyWithoutATranscriptDir(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	gotPath := "unset"
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPath = cfg.TranscriptPath
		return pushed(), nil
	})

	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if gotPath != "" {
		t.Errorf("TranscriptPath = %q, want empty with no Config.TranscriptDir set", gotPath)
	}
}

// finished_at used to be stamped with the same `at` RunCycle passed in as
// the run's StartedAt, so every run ever recorded read back as having
// taken zero seconds -- the UI's attempt timeline showed a run that had
// worked for an hour and one that died on its first turn identically,
// which is the first thing anyone wants to know about a failed run.
func TestRunDispatchRecordsWhenTheRunActuallyFinished(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return &agent.Result{ToolCalls: []agent.ToolCall{{Name: "run_command", Text: "ok"}}}, nil
	})

	// baseTime is the fixed clock this package's tests dispatch against,
	// far from wall-clock now -- which is exactly what makes it able to
	// tell the two timestamps apart.
	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}

	runs, err := store.Runs(ctx, "t1")
	if err != nil || len(runs) != 1 {
		t.Fatalf("Runs = (%+v, %v)", runs, err)
	}
	if runs[0].FinishedAt == nil {
		t.Fatal("FinishedAt = nil, want the run to be finished")
	}
	if !runs[0].FinishedAt.After(runs[0].StartedAt) {
		t.Errorf("FinishedAt (%s) is not after StartedAt (%s); the run's own duration is unreadable",
			runs[0].FinishedAt, runs[0].StartedAt)
	}
}
