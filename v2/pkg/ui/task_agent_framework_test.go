package ui_test

// A task's own agent framework: the per-task override of the
// deployment-wide default, filed with the task, edited like any other
// field, and reported back on the task itself so a frontend can show
// which framework a task will actually run under.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func TestCreateTaskCarriesItsOwnAgentFramework(t *testing.T) {
	client, store, ctx := testClient(t)

	task, err := client.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "port the parser", AgentFramework: model.AgentFrameworkClaude, Approved: true,
	})
	if err != nil {
		t.Fatalf("creating a task: %v", err)
	}
	if task.AgentFramework != model.AgentFrameworkClaude {
		t.Fatalf("task.agentFramework = %q, want %q", task.AgentFramework, model.AgentFrameworkClaude)
	}

	// Read back through the store, not the response: what orchestrator's
	// own dispatch will see is the stored column, and a field that only
	// ever existed in the reply would dispatch on the deployment default
	// anyway.
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("reading the task back: %v", err)
	}
	if stored.AgentFramework != model.AgentFrameworkClaude {
		t.Fatalf("stored AgentFramework = %q, want %q", stored.AgentFramework, model.AgentFrameworkClaude)
	}
}

func TestCreateTaskDefaultsToNoAgentFrameworkOverride(t *testing.T) {
	client, _, ctx := testClient(t)

	task, err := client.CreateTask(ctx, ui.CreateTaskRequest{Title: "fix it", Approved: true})
	if err != nil {
		t.Fatalf("creating a task: %v", err)
	}
	// Empty, not "gemini": a task filed without a choice must follow the
	// deployment wherever it is set later, not pin itself to whatever it
	// happened to be at the moment it was filed.
	if task.AgentFramework != "" {
		t.Fatalf("task.agentFramework = %q, want empty", task.AgentFramework)
	}
}

func TestCreateTaskRejectsAnUnknownAgentFramework(t *testing.T) {
	client, _, ctx := testClient(t)

	_, err := client.CreateTask(ctx, ui.CreateTaskRequest{Title: "fix it", AgentFramework: "gpt"})
	if err == nil {
		t.Fatal("creating a task with an unknown agent framework succeeded")
	}
	var invalid *ui.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
}

func TestUpdateTaskSetsAndClearsTheAgentFrameworkOverride(t *testing.T) {
	srv, client := testServer(t)
	ctx := context.Background()

	task, err := client.CreateTask(ctx, ui.CreateTaskRequest{Title: "fix it", Approved: true})
	if err != nil {
		t.Fatalf("creating a task: %v", err)
	}

	rec := do(t, srv, http.MethodPatch, "/api/tasks/"+task.ID, `{"agentFramework":"claude"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode[ui.Task](t, rec); got.AgentFramework != model.AgentFrameworkClaude {
		t.Fatalf("agentFramework = %q after the patch, want claude", got.AgentFramework)
	}

	// An empty string is a real edit here, not "leave it alone": it is
	// how the override is handed back to the deployment.
	rec = do(t, srv, http.MethodPatch, "/api/tasks/"+task.ID, `{"agentFramework":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clearing patch status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode[ui.Task](t, rec); got.AgentFramework != "" {
		t.Fatalf("agentFramework = %q after clearing, want empty", got.AgentFramework)
	}

	rec = do(t, srv, http.MethodPatch, "/api/tasks/"+task.ID, `{"agentFramework":"gpt"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown framework patch status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestConfigReportsTheDeploymentAgentFramework(t *testing.T) {
	srv, client := testServer(t)
	ctx := context.Background()

	// Nothing saved yet: the same "gemini" every deployment has always
	// run, rather than an empty string the frontend would have to know to
	// interpret.
	if got := decode[map[string]any](t, do(t, srv, http.MethodGet, "/api/config", "")); got["agentFramework"] != model.AgentFrameworkGemini {
		t.Fatalf("agentFramework = %v before any settings were saved, want gemini", got["agentFramework"])
	}

	if _, err := client.UpdateSettings(ctx, ui.UpdateSettingsRequest{
		PollInterval: ptr("30s"), MaxConcurrent: ptr(1),
		GeminiModel: ptr("gemini-3.1-pro"), GitHubHost: ptr("github.com"),
		AgentFramework: ptr(model.AgentFrameworkClaude),
	}); err != nil {
		t.Fatalf("saving settings: %v", err)
	}
	if got := decode[map[string]any](t, do(t, srv, http.MethodGet, "/api/config", "")); got["agentFramework"] != model.AgentFrameworkClaude {
		t.Fatalf("agentFramework = %v, want claude", got["agentFramework"])
	}
}

// ptr is the one-line "&literal" every UpdateSettingsRequest field above
// needs -- its pointer fields are what "leave this one alone" is spelled
// with, so a test setting one has nothing to take the address of.
func ptr[T any](v T) *T { return &v }
