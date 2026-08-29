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

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/ui"
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

	if err := c.AddComment(ctx, created.ID, "hello"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	detail, err := c.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].Body != "hello" {
		t.Fatalf("comments = %+v, want one saying hello", detail.Comments)
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
	if len(cfg.Capabilities) != len(ui.DefaultCapabilities()) {
		t.Fatalf("capabilities = %d, want %d", len(cfg.Capabilities), len(ui.DefaultCapabilities()))
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

	pollInterval, slots, geminiModel, githubHost := "1m", []string{"a", "b"}, "gemini-2.5-pro", "github.com"
	after, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{
		PollInterval: &pollInterval, Slots: &slots, GeminiModel: &geminiModel, GitHubHost: &githubHost,
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
