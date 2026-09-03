package main

// daemonTasks is the hop a self-debug run's task tools take: a real
// `grain daemon`'s REST API in front of a real store, read by the same
// mcp.TaskReader the tools call. These prove the projection end to end
// -- a task written to the store comes back out of a tool call with its
// attempts, its recorded errors and its conversation intact -- since
// pkg/mcp's own tests answer only for the rendering, against a fake.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

const selfDebugTaskTime = "2026-01-02T03:04:05Z"

// failedTaskWorld is a deployment with one task in it that failed once
// and is on its second attempt -- the shape somebody opens the
// configuration agent to ask about.
func failedTaskWorld(t *testing.T) (mcp.TaskReader, string) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	started, err := time.Parse(time.RFC3339, selfDebugTaskTime)
	if err != nil {
		t.Fatal(err)
	}

	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	human := model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "operator"}}
	task := model.Task{
		ID: "task-7", Intent: model.IntentImplement,
		Origin:   model.Origin{Attribution: human, Reason: model.ReasonDirect},
		Title:    "Teach the merge queue to wait",
		Body:     "It merges before the checks finish.",
		Approval: &human, Target: &repo, Binding: model.BindingDirective,
		CreatedAt: &started,
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID: task.ID, Author: human, Body: "This has failed once already.", CreatedAt: started,
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "s1", Attempt: 1, StartedAt: started,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", started.Add(time.Minute), "failed",
		"cloning acme/widgets: exit status 128"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunPrompt(ctx, "r1", "You are working on task-7."); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunTranscript(ctx, "r1", "> run_command(git clone)\nexit=128"); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r2", TaskID: task.ID, Sandbox: "s2", Attempt: 2, StartedAt: started.Add(time.Hour),
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(ui.NewServer(ui.Config{
		Actor: ui.DefaultActor("tester"), Capabilities: ui.OfferedCapabilities(),
	}, store))
	t.Cleanup(srv.Close)
	return daemonTasks{client: ui.NewHTTPClient(srv.URL)}, task.ID
}

// callSelfDebugTool runs one of the task tools the way `grain mcpserver
// -self-debug` serves it, over the daemon reader rather than a fake.
func callSelfDebugTool(t *testing.T, reader mcp.TaskReader, name string, args map[string]any) mcp.Result {
	t.Helper()
	for _, tool := range mcp.NewTaskTools(reader) {
		if tool.Name == name {
			res := tool.Handler(context.Background(), args)
			if res.IsError {
				t.Fatalf("%s reported an error: %s", name, res.Text)
			}
			return res
		}
	}
	t.Fatalf("no tool named %q", name)
	return mcp.Result{}
}

func TestDaemonTasksListsTheDeploymentsTasks(t *testing.T) {
	reader, id := failedTaskWorld(t)
	res := callSelfDebugTool(t, reader, "list_grain_tasks", nil)
	for _, want := range []string{id, "acme/widgets", "Teach the merge queue to wait"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("list_grain_tasks = %q, want it to contain %q", res.Text, want)
		}
	}
}

// The whole point of the capability: the error a failed attempt recorded
// reaches the agent, alongside the conversation that explains what was
// being asked for.
func TestDaemonTasksCarriesAttemptsErrorsAndComments(t *testing.T) {
	reader, id := failedTaskWorld(t)
	res := callSelfDebugTool(t, reader, "read_grain_task", map[string]any{"task_id": id})
	for _, want := range []string{
		"Task task-7: Teach the merge queue to wait",
		"Filed by: operator (human)",
		"cloning acme/widgets: exit status 128",
		"#1  started " + selfDebugTaskTime,
		"still running",
		"This has failed once already.",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("read_grain_task = %q, want it to contain %q", res.Text, want)
		}
	}
}

func TestDaemonTasksReadsAPromptAndATranscript(t *testing.T) {
	reader, id := failedTaskWorld(t)

	prompt := callSelfDebugTool(t, reader, "read_grain_task_prompt", map[string]any{"task_id": id})
	if !strings.Contains(prompt.Text, "You are working on task-7.") {
		t.Errorf("read_grain_task_prompt = %q, want the recorded prompt", prompt.Text)
	}

	transcript := callSelfDebugTool(t, reader, "read_grain_task_transcript",
		map[string]any{"task_id": id, "attempt": float64(1)})
	if !strings.Contains(transcript.Text, "exit=128") {
		t.Errorf("read_grain_task_transcript = %q, want attempt 1's own transcript", transcript.Text)
	}
}
