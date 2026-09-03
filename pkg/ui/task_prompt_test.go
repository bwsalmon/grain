package ui_test

// GET /api/tasks/{id}/prompt is the API surface behind the UI's "show
// the full prompt" button (grain/task-91): the whole text
// orchestrator.RunDispatch handed the agent, recorded per run by
// Store.SetRunPrompt. These cover the route's status codes and JSON
// shape together with what Client.TaskPrompt picks out of the store,
// since the interesting part is which attempt's prompt a task with
// several answers with.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// promptResponse mirrors ui.TaskPrompt's wire shape, decoded here rather
// than imported so a rename of the JSON keys shows up as a failing test
// instead of silently still compiling.
type promptResponse struct {
	Prompt  string `json:"prompt"`
	Attempt int    `json:"attempt"`
}

// startRun records one attempt at taskID with the prompt its agent was
// given -- the pair RunDispatch writes (StartRun, then SetRunPrompt just
// before the agent's first turn). prompt "" is an attempt that never
// reached its agent, which records none.
func startRun(t *testing.T, store *model.Store, ctx context.Context, taskID, runID string, attempt int, prompt string) {
	t.Helper()
	if err := store.StartRun(ctx, model.Run{
		ID: runID, TaskID: taskID, Sandbox: "s" + runID,
		Attempt: attempt, StartedAt: baseTime.Add(time.Duration(attempt) * time.Hour),
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if prompt == "" {
		return
	}
	if err := store.SetRunPrompt(ctx, runID, prompt); err != nil {
		t.Fatal(err)
	}
}

func TestGetTaskPromptReturnsWhatTheAgentWasGiven(t *testing.T) {
	srv, client := testServer(t)
	ctx := context.Background()
	task := create(t, client, ctx)

	const prompt = "Fix the thing\n\ndetails\n\nWork in acme/widgets. Push your change to a new branch named \"grain/task-1\"."
	startRun(t, client.Store, ctx, task.ID, "r1", 1, prompt)

	rec := do(t, srv, http.MethodGet, "/api/tasks/"+task.ID+"/prompt", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[promptResponse](t, rec)
	if got.Prompt != prompt {
		t.Fatalf("prompt = %q, want %q", got.Prompt, prompt)
	}
	if got.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 -- the attempt the prompt belongs to", got.Attempt)
	}
}

// A redispatched task's prompt grows with its conversation, so the
// newest attempt's is the one worth showing: it is what this task looks
// like to an agent now.
func TestGetTaskPromptPrefersTheMostRecentAttempt(t *testing.T) {
	srv, client := testServer(t)
	ctx := context.Background()
	task := create(t, client, ctx)

	startRun(t, client.Store, ctx, task.ID, "r1", 1, "the first prompt")
	if err := client.Store.FinishRun(ctx, "r1", baseTime.Add(time.Minute), "failed", ""); err != nil {
		t.Fatal(err)
	}
	startRun(t, client.Store, ctx, task.ID, "r2", 2, "the second prompt, with the conversation so far")

	got := decode[promptResponse](t, do(t, srv, http.MethodGet, "/api/tasks/"+task.ID+"/prompt", ""))
	if got.Prompt != "the second prompt, with the conversation so far" || got.Attempt != 2 {
		t.Fatalf("prompt = %+v, want attempt 2's", got)
	}
}

// An attempt that died in setup (a checkout that would not clone, a
// capability that would not mint) never reached its agent and so
// recorded no prompt. That must not hide the prompt an earlier attempt
// really was given.
func TestGetTaskPromptSkipsAnAttemptThatNeverReachedItsAgent(t *testing.T) {
	srv, client := testServer(t)
	ctx := context.Background()
	task := create(t, client, ctx)

	startRun(t, client.Store, ctx, task.ID, "r1", 1, "the prompt attempt 1 was given")
	if err := client.Store.FinishRun(ctx, "r1", baseTime.Add(time.Minute), "failed", ""); err != nil {
		t.Fatal(err)
	}
	startRun(t, client.Store, ctx, task.ID, "r2", 2, "")

	got := decode[promptResponse](t, do(t, srv, http.MethodGet, "/api/tasks/"+task.ID+"/prompt", ""))
	if got.Prompt != "the prompt attempt 1 was given" || got.Attempt != 1 {
		t.Fatalf("prompt = %+v, want attempt 1's, which is the only one there is", got)
	}
}

// A task nothing has been dispatched for is a 200 with an empty prompt,
// not a 404: the task exists, and "nothing has run yet" is an answer the
// frontend renders rather than an error it reports.
func TestGetTaskPromptOfATaskThatHasNeverRunIsEmpty(t *testing.T) {
	srv, client := testServer(t)
	task := create(t, client, context.Background())

	rec := do(t, srv, http.MethodGet, "/api/tasks/"+task.ID+"/prompt", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[promptResponse](t, rec)
	if got.Prompt != "" || got.Attempt != 0 {
		t.Fatalf("prompt = %+v, want nothing recorded", got)
	}
}

func TestGetTaskPromptOfAnUnknownTaskIs404(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/tasks/nosuchtask/prompt", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}
