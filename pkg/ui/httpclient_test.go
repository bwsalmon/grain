package ui_test

// HTTPClient's own tests: the same operations client_test.go already
// proves against a direct model.Store caller, driven instead through a
// real net/http round trip against a real httptest.Server wrapping
// ui.Server -- bwsalmon/agents#363's whole point, that a CLI reaching
// the daemon over REST behaves the same as one that used to open the
// store itself.

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

func testHTTPClient(t *testing.T) (*ui.HTTPClient, context.Context) {
	t.Helper()
	storeClient, _, ctx := testClient(t)
	srv := httptest.NewServer(ui.NewServerWithClient(storeClient))
	t.Cleanup(srv.Close)
	return ui.NewHTTPClient(srv.URL), ctx
}

func TestHTTPClientCreateListAndGetRoundTrip(t *testing.T) {
	c, ctx := testHTTPClient(t)

	created, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "fix it", Description: "please", Approved: true,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created task has no id")
	}
	if created.State != model.StateQueued {
		t.Fatalf("state = %q, want queued", created.State)
	}

	tasks, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != created.ID {
		t.Fatalf("ListTasks = %+v, want just the created task", tasks)
	}

	got, err := c.Task(ctx, created.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("Task = %+v, want id %q", got, created.ID)
	}

	detail, err := c.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if detail.ID != created.ID || detail.Comments == nil {
		t.Fatalf("GetTask = %+v, want the task plus a (possibly empty, non-nil) comment slice", detail)
	}
}

func TestHTTPClientUpdateSetCapabilityCommentCloseReopen(t *testing.T) {
	c, ctx := testHTTPClient(t)
	created, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "fix it", Approved: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	title := "fix it for real"
	updated, err := c.UpdateTask(ctx, created.ID, ui.UpdateTaskRequest{Title: &title})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Title != title {
		t.Fatalf("title after update = %q, want %q", updated.Title, title)
	}

	if err := c.SetCapability(ctx, created.ID, "gemini-key", true); err != nil {
		t.Fatalf("SetCapability(attach): %v", err)
	}
	withCap, err := c.Task(ctx, created.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if len(withCap.Capabilities) != 1 || withCap.Capabilities[0] != "gemini-key" {
		t.Fatalf("capabilities after attach = %+v, want [gemini-key]", withCap.Capabilities)
	}
	if err := c.SetCapability(ctx, created.ID, "gemini-key", false); err != nil {
		t.Fatalf("SetCapability(detach): %v", err)
	}

	if err := c.AddComment(ctx, created.ID, "hello", nil); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	detail, err := c.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	// Two, not one: UpdateTask's own title edit above already added the
	// first, noting the change for the same reason a live run needs to
	// see it (bwsalmon/agents#523) -- AddComment's "hello" is the second.
	if len(detail.Comments) != 2 || detail.Comments[0].Body == "" || detail.Comments[1].Body != "hello" {
		t.Fatalf("comments = %+v, want the title-edit note followed by one saying hello", detail.Comments)
	}

	if err := c.Close(ctx, created.ID, ui.CloseOptions{}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := c.Task(ctx, created.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if closed.State != model.StateClosed {
		t.Fatalf("state after close = %q, want closed", closed.State)
	}

	if err := c.Reopen(ctx, created.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	reopened, err := c.Task(ctx, created.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if reopened.State != model.StateQueued {
		t.Fatalf("state after reopen = %q, want queued", reopened.State)
	}
}

// TestHTTPClientRetry rounds out HTTPClient's own tests with Retry, the
// one mutating method nothing here exercised through a real HTTP round
// trip before now -- server_test.go and client_test.go each already
// prove the route and the underlying Client method.
func TestHTTPClientRetry(t *testing.T) {
	c, ctx := testHTTPClient(t)
	created, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "fix it", Approved: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Retry on a task that has never failed is documented as a harmless
	// no-op (Client.Retry); this just proves the round trip itself
	// succeeds rather than erroring.
	if err := c.Retry(ctx, created.ID); err != nil {
		t.Fatalf("Retry: %v", err)
	}
}

// CloseOptions survives the round trip -- the half of `grain close
// -close-pull-request` that no direct-Client test can show, since the
// flag has to be encoded, sent, and read back off the request body
// before it reaches the same Client.Close everything else tests.
func TestHTTPClientCloseCarriesTheOptionAcrossTheWire(t *testing.T) {
	for _, want := range []bool{false, true} {
		storeClient, store, ctx := testClient(t)
		closer := &recordingCloser{}
		storeClient.Config.PullRequestCloser = closer
		storeClient.Config.PullRequestComments = &recordingComments{}
		srv := httptest.NewServer(ui.NewServerWithClient(storeClient))
		t.Cleanup(srv.Close)
		task, ref := linkedTask(t, storeClient, store, ctx)

		c := ui.NewHTTPClient(srv.URL)
		if err := c.Close(ctx, task.ID, ui.CloseOptions{ClosePullRequest: want}); err != nil {
			t.Fatalf("Close(%t): %v", want, err)
		}
		closed, err := c.Task(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if closed.State != model.StateClosed {
			t.Fatalf("state after close = %q, want closed", closed.State)
		}
		if want && (len(closer.closed) != 1 || closer.closed[0] != ref) {
			t.Fatalf("closed on GitHub = %+v, want %s", closer.closed, ref)
		}
		if !want && len(closer.closed) != 0 {
			t.Fatalf("closed on GitHub = %+v, want nothing", closer.closed)
		}
	}
}

func TestHTTPClientApproveOnAProposal(t *testing.T) {
	c, ctx := testHTTPClient(t)
	created, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "needs a look"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.State != model.StateProposed {
		t.Fatalf("state as created = %q, want proposed", created.State)
	}
	if err := c.Approve(ctx, created.ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	approved, err := c.Task(ctx, created.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if approved.State != model.StateQueued {
		t.Fatalf("state after approve = %q, want queued", approved.State)
	}

	// And back again, over the same wire: `grain withdraw`'s own round
	// trip, which is the only thing this client is for.
	if err := c.WithdrawApproval(ctx, created.ID); err != nil {
		t.Fatalf("WithdrawApproval: %v", err)
	}
	withdrawn, err := c.Task(ctx, created.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if withdrawn.State != model.StateProposed {
		t.Fatalf("state after withdrawing approval = %q, want proposed", withdrawn.State)
	}
}

func TestHTTPClientTaskNotFoundIsNotFoundError(t *testing.T) {
	c, ctx := testHTTPClient(t)
	_, err := c.GetTask(ctx, "does-not-exist")
	var nf *ui.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("GetTask on an unknown id: err = %v, want a *ui.NotFoundError", err)
	}
	// The server already formats its 404 body as "no task <id>" (see
	// Client's own NotFoundError.Error()); httpError used to wrap that
	// whole message back into NotFoundError{ID: message}, whose own
	// Error() prepended "no task " a second time, so every HTTPClient
	// caller -- the CLI, and the browser frontend, which displays the
	// JSON body's "error" field verbatim -- saw "no task no task
	// does-not-exist" instead of "no task does-not-exist".
	if got := nf.Error(); got != "no task does-not-exist" {
		t.Errorf("NotFoundError.Error() = %q, want %q", got, "no task does-not-exist")
	}
}

func TestHTTPClientCreateValidationErrorIsValidationError(t *testing.T) {
	c, ctx := testHTTPClient(t)
	_, err := c.CreateTask(ctx, ui.CreateTaskRequest{})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("CreateTask with no title: err = %v, want a *ui.ValidationError", err)
	}
}

// Config and GetSettings/UpdateSettings are the two other things the CLI
// reads over this same client -- Config carries the deployment's actor
// and default target as plain strings on the wire (configResponse), and
// this proves HTTPClient reconstructs them into the same shape a
// store-backed Client's own Config field already has.
func TestHTTPClientConfigReadsActorAndDefaultTarget(t *testing.T) {
	c, ctx := testHTTPClient(t)
	cfg, err := c.Config(ctx)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.Actor.ID != "alice" {
		t.Fatalf("actor = %+v, want alice", cfg.Actor)
	}
	if cfg.DefaultTarget == nil || cfg.DefaultTarget.String() != "acme/widgets" {
		t.Fatalf("default target = %v, want acme/widgets", cfg.DefaultTarget)
	}
	if len(cfg.Capabilities) != len(ui.OfferedCapabilities()) {
		t.Fatalf("capabilities = %d, want %d", len(cfg.Capabilities), len(ui.OfferedCapabilities()))
	}
}

// TestHTTPClientConfigReadsTargetRepos is
// TestHTTPClientConfigReadsActorAndDefaultTarget's own proof, extended
// to TargetRepos: it too rides configResponse's wire shape rather than
// GetSettings' (a daemon's TargetRepos is fixed at startup -- see
// cmd/grain/daemon.go's loadConfig doc comment -- so the CLI and the
// frontend both read it from here, not from the store-backed Settings).
func TestHTTPClientConfigReadsTargetRepos(t *testing.T) {
	storeClient, _, ctx := testClient(t)
	storeClient.Config.TargetRepos = []string{"acme/widgets", "acme/other"}
	srv := httptest.NewServer(ui.NewServerWithClient(storeClient))
	t.Cleanup(srv.Close)
	c := ui.NewHTTPClient(srv.URL)

	cfg, err := c.Config(ctx)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if len(cfg.TargetRepos) != 2 || cfg.TargetRepos[0] != "acme/widgets" || cfg.TargetRepos[1] != "acme/other" {
		t.Fatalf("targetRepos = %v, want [acme/widgets acme/other]", cfg.TargetRepos)
	}
}

func TestHTTPClientSettingsRoundTrip(t *testing.T) {
	c, ctx := testHTTPClient(t)
	before, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if before.Configured {
		t.Fatalf("settings on a fresh store = %+v, want Configured false", before)
	}

	pollInterval, maxWorkers, geminiModel, claudeModel, githubHost := "1m", 2, "gemini-2.5-pro", "claude-sonnet-5", "github.com"
	after, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{
		PollInterval: &pollInterval, MaxWorkers: &maxWorkers,
		GeminiModel: &geminiModel, ClaudeModel: &claudeModel, GitHubHost: &githubHost,
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	// PollInterval round-trips through time.Duration.String(), which
	// normalizes "1m" to "1m0s" -- Settings.PollInterval's own doc
	// comment already says as much.
	if !after.Configured || after.PollInterval != "1m0s" || after.GeminiModel != geminiModel {
		t.Fatalf("settings after update = %+v", after)
	}
}

// The repo family over the wire (grain/task-36): the four calls `grain
// repo` makes, driven end to end against a real server, since each one
// builds a path out of an owner/name and two of them are the only way
// the CLI can reach a repo's own stored defaults at all.
func TestHTTPClientRepoFamilyRoundTrip(t *testing.T) {
	c, ctx := testHTTPClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatalf("seeding settings: %v", err)
	}

	settings, err := c.AddTargetRepo(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("AddTargetRepo: %v", err)
	}
	if !reflect.DeepEqual(settings.TargetRepos, []string{"acme/widgets"}) {
		t.Fatalf("targetRepos after add = %v, want [acme/widgets]", settings.TargetRepos)
	}

	defaults, err := c.SetRepoDefaultCapabilities(ctx, "acme/widgets", []string{"self-debug"})
	if err != nil {
		t.Fatalf("SetRepoDefaultCapabilities: %v", err)
	}
	if !reflect.DeepEqual(defaults.DefaultCapabilities, []string{"self-debug"}) {
		t.Fatalf("own defaults after set = %v, want [self-debug]", defaults.DefaultCapabilities)
	}
	read, err := c.RepoDefaults(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("RepoDefaults: %v", err)
	}
	if !reflect.DeepEqual(read, defaults) {
		t.Fatalf("RepoDefaults = %+v, want what the write returned %+v", read, defaults)
	}

	// The other half of the same row (grain/task-114), written through
	// its own route: the text comes back, and the capabilities set just
	// above it survives -- PutRepoConfig replaces the row wholesale, so
	// "saving one pane wipes the other's work" is what this pins over the
	// wire as well as in Client's own tests.
	prompt, err := c.SetRepoPromptExtension(ctx, "acme/widgets", "Migrations live in db/.")
	if err != nil {
		t.Fatalf("SetRepoPromptExtension: %v", err)
	}
	if prompt.PromptExtension != "Migrations live in db/." {
		t.Fatalf("own prompt extension after set = %q, want the text just written", prompt.PromptExtension)
	}
	if !reflect.DeepEqual(prompt.DefaultCapabilities, []string{"self-debug"}) {
		t.Fatalf("own defaults after setting the prompt extension = %v, want [self-debug] still",
			prompt.DefaultCapabilities)
	}
	read, err = c.RepoDefaults(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("RepoDefaults: %v", err)
	}
	if !reflect.DeepEqual(read, prompt) {
		t.Fatalf("RepoDefaults = %+v, want what the write returned %+v", read, prompt)
	}

	if _, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "fix it", Repo: "acme/widgets", Approved: true}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	repos, err := c.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("ListRepos = %+v, want one row", repos)
	}
	want := ui.RepoSummary{
		Repo: "acme/widgets", Configured: true, Tasks: 1,
		States:              map[model.State]int{model.StateQueued: 1},
		DefaultCapabilities: []string{"self-debug"},
		// Whether, not what: a row per repo is no place for a paragraph,
		// so the listing only points at `grain repo prompt-extension`.
		PromptExtension: true,
	}
	if !reflect.DeepEqual(repos[0], want) {
		t.Fatalf("ListRepos row = %+v, want %+v", repos[0], want)
	}

	// Removing the repo from the allowlist leaves everything else about
	// it alone: the task still targets it, so it still has a row, and
	// its own defaults are still stored -- the independence
	// RemoveTargetRepo's own doc comment promises.
	settings, err = c.RemoveTargetRepo(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("RemoveTargetRepo: %v", err)
	}
	if len(settings.TargetRepos) != 0 {
		t.Fatalf("targetRepos after remove = %v, want empty", settings.TargetRepos)
	}
	repos, err = c.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos after remove: %v", err)
	}
	if len(repos) != 1 || repos[0].Configured || len(repos[0].DefaultCapabilities) != 1 {
		t.Fatalf("ListRepos after remove = %+v, want one unconfigured row that kept its defaults", repos)
	}
}

// An unknown capability is refused where whoever chose it can still see
// the refusal, rather than stored and quietly never granted -- the same
// answer UpdateSettings gives for the deployment-wide set.
func TestHTTPClientSetRepoDefaultCapabilitiesRejectsAnUnknownID(t *testing.T) {
	c, ctx := testHTTPClient(t)
	_, err := c.SetRepoDefaultCapabilities(ctx, "acme/widgets", []string{"no-such-capability"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("SetRepoDefaultCapabilities with an unknown id: error = %v, want a ValidationError", err)
	}
}

// A repo with no owner is caught before a request is sent, as the same
// ValidationError the server would have given -- without it, the path
// built from "widgets" matches no route and comes back as a bare 404
// about nothing in particular. AddTargetRepo is deliberately not in this
// list: it carries the repo in the body rather than the path, so there
// is no route to miss and the server's own refusal arrives intact.
func TestHTTPClientRepoMethodsRejectAMalformedRepoLocally(t *testing.T) {
	c := ui.NewHTTPClient("http://127.0.0.1:1") // never dialed
	ctx := context.Background()
	var ve *ui.ValidationError
	if _, err := c.RepoDefaults(ctx, "widgets"); !errors.As(err, &ve) {
		t.Errorf("RepoDefaults(\"widgets\"): error = %v, want a ValidationError", err)
	}
	if _, err := c.SetRepoDefaultCapabilities(ctx, "widgets", nil); !errors.As(err, &ve) {
		t.Errorf("SetRepoDefaultCapabilities(\"widgets\"): error = %v, want a ValidationError", err)
	}
	if _, err := c.SetRepoPromptExtension(ctx, "widgets", "text"); !errors.As(err, &ve) {
		t.Errorf("SetRepoPromptExtension(\"widgets\"): error = %v, want a ValidationError", err)
	}
	if _, err := c.RemoveTargetRepo(ctx, "widgets"); !errors.As(err, &ve) {
		t.Errorf("RemoveTargetRepo(\"widgets\"): error = %v, want a ValidationError", err)
	}
}

// The two reads `grain mcpserver` makes on behalf of a self-debug run's
// read_grain_task_prompt and read_grain_task_transcript tools: what
// another task's agent was told, and what its session actually did.
// Both are on HTTPClient rather than only on Client because that process
// holds no store handle of its own -- see pkg/mcp's task_tools.go.
func TestHTTPClientReadsATasksPromptAndTranscript(t *testing.T) {
	storeClient, store, ctx := testClient(t)
	srv := httptest.NewServer(ui.NewServerWithClient(storeClient))
	t.Cleanup(srv.Close)
	c := ui.NewHTTPClient(srv.URL)

	task := create(t, storeClient, ctx)
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "s1", Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", baseTime.Add(time.Minute), "failed", "cloning: exit status 128"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunPrompt(ctx, "r1", "You are working on it."); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunTranscript(ctx, "r1", "> run_command(git clone)\nexit=128"); err != nil {
		t.Fatal(err)
	}

	prompt, err := c.TaskPrompt(ctx, task.ID)
	if err != nil {
		t.Fatalf("TaskPrompt: %v", err)
	}
	if prompt.Prompt != "You are working on it." || prompt.Attempt != 1 {
		t.Errorf("TaskPrompt = %+v, want attempt 1's own prompt", prompt)
	}

	transcript, err := c.AttemptTranscript(ctx, task.ID, 1)
	if err != nil {
		t.Fatalf("AttemptTranscript: %v", err)
	}
	if transcript != "> run_command(git clone)\nexit=128" {
		t.Errorf("AttemptTranscript = %q", transcript)
	}
}

// A task nothing has been dispatched for has no prompt, and that is an
// answer rather than a failure -- the route itself draws that line, and
// the client must not turn it into an error.
func TestHTTPClientTaskPromptIsEmptyForAnUndispatchedTask(t *testing.T) {
	storeClient, _, ctx := testClient(t)
	srv := httptest.NewServer(ui.NewServerWithClient(storeClient))
	t.Cleanup(srv.Close)
	c := ui.NewHTTPClient(srv.URL)

	task := create(t, storeClient, ctx)
	prompt, err := c.TaskPrompt(ctx, task.ID)
	if err != nil {
		t.Fatalf("TaskPrompt: %v", err)
	}
	if prompt.Prompt != "" || prompt.Attempt != 0 {
		t.Errorf("TaskPrompt = %+v, want the empty prompt", prompt)
	}
}

// An attempt that never happened is a NotFoundError over the wire too,
// so read_grain_task_transcript can say which attempt it could not find
// rather than reporting an empty transcript.
func TestHTTPClientAttemptTranscriptOfAnUnknownAttemptIsNotFound(t *testing.T) {
	storeClient, _, ctx := testClient(t)
	srv := httptest.NewServer(ui.NewServerWithClient(storeClient))
	t.Cleanup(srv.Close)
	c := ui.NewHTTPClient(srv.URL)

	task := create(t, storeClient, ctx)
	if _, err := c.AttemptTranscript(ctx, task.ID, 4); err == nil {
		t.Fatal("AttemptTranscript err = nil, want a not-found error")
	} else {
		var notFound *ui.NotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("AttemptTranscript err = %v, want a *ui.NotFoundError", err)
		}
	}
}
