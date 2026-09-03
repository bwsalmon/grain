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
	"testing"

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

	if err := c.Close(ctx, created.ID); err != nil {
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

	pollInterval, maxConcurrent, geminiModel, claudeModel, githubHost := "1m", 2, "gemini-2.5-pro", "claude-sonnet-5", "github.com"
	after, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{
		PollInterval: &pollInterval, MaxConcurrent: &maxConcurrent,
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
