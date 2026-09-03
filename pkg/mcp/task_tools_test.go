package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeTasks is a TaskReader over a fixed set of records -- the store
// stands in for nothing here, since what these tests are about is what a
// self-debug run reads back, not how the daemon found it.
type fakeTasks struct {
	summaries   []TaskSummary
	records     map[string]TaskRecord
	prompts     map[string]TaskPromptRecord
	transcripts map[string]string // "<id>#<attempt>"
	err         error
}

func (f *fakeTasks) ListTasks(context.Context) ([]TaskSummary, error) {
	return f.summaries, f.err
}

func (f *fakeTasks) Task(_ context.Context, id string) (TaskRecord, error) {
	if f.err != nil {
		return TaskRecord{}, f.err
	}
	record, ok := f.records[id]
	if !ok {
		return TaskRecord{}, fmt.Errorf("no task %s", id)
	}
	return record, nil
}

func (f *fakeTasks) TaskPrompt(_ context.Context, id string) (TaskPromptRecord, error) {
	if f.err != nil {
		return TaskPromptRecord{}, f.err
	}
	return f.prompts[id], nil
}

func (f *fakeTasks) AttemptTranscript(_ context.Context, id string, attempt int) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	transcript, ok := f.transcripts[fmt.Sprintf("%s#%d", id, attempt)]
	if !ok {
		return "", fmt.Errorf("no attempt %d for task %s", attempt, id)
	}
	return transcript, nil
}

// callTask invokes one of the four tools by name, the way a client
// would, so a rename of a tool breaks these tests rather than silently
// testing a function nothing serves.
func callTask(t *testing.T, reader TaskReader, name string, args map[string]any) Result {
	t.Helper()
	for _, tool := range NewTaskTools(reader) {
		if tool.Name == name {
			return tool.Handler(context.Background(), args)
		}
	}
	t.Fatalf("NewTaskTools registered no %s", name)
	return Result{}
}

func at(text string) time.Time {
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		panic(err)
	}
	return parsed
}

func worldOfTasks() *fakeTasks {
	filed := at("2026-01-02T03:04:05Z")
	finished := at("2026-01-02T03:30:00Z")
	return &fakeTasks{
		summaries: []TaskSummary{
			{ID: "task-1", Title: "Teach the merge queue to wait", State: "completed",
				Repo: "acme/widgets", CreatedAt: &filed},
			{ID: "task-2", Title: "Broken clone", State: "failed", Repo: "acme/widgets"},
			{ID: "task-3", Title: "Configuration agent", State: "running",
				Interactive: true, Configuration: true},
		},
		records: map[string]TaskRecord{
			"task-2": {
				TaskSummary: TaskSummary{ID: "task-2", Title: "Broken clone", State: "failed",
					Repo: "acme/widgets", CreatedAt: &filed},
				Description:       "The checkout fails before the agent gets a turn.",
				Author:            "operator",
				AuthorKind:        "human",
				Base:              "main",
				Capabilities:      []string{"github-sandbox"},
				FailedAttempts:    2,
				LastFailureReason: "cloning acme/widgets: exit status 128",
				Attempts: []TaskAttempt{
					{Number: 1, StartedAt: filed, FinishedAt: &finished, Outcome: "failed",
						Detail: "cloning acme/widgets: exit status 128"},
					{Number: 2, StartedAt: finished, Outcome: "running"},
				},
				Comments: []TaskComment{
					{Author: "operator", AuthorKind: "human", CreatedAt: filed,
						Body: "This has failed twice now."},
				},
			},
		},
		prompts: map[string]TaskPromptRecord{
			"task-2": {Prompt: "You are working on task-2.", Attempt: 2},
		},
		transcripts: map[string]string{
			"task-2#1": "attempt one: git clone failed",
			"task-2#2": "attempt two: still going",
		},
	}
}

func TestListGrainTasksNamesEveryTask(t *testing.T) {
	res := callTask(t, worldOfTasks(), "list_grain_tasks", nil)
	if res.IsError {
		t.Fatalf("list_grain_tasks reported an error: %s", res.Text)
	}
	for _, want := range []string{
		"task-1", "task-2", "task-3",
		"[completed]", "[failed]", "[running]",
		"acme/widgets", "Teach the merge queue to wait", "(configuration agent)",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text = %q, want it to contain %q", res.Text, want)
		}
	}
}

// The state filter is what makes this tool usable on a deployment with a
// long history: "which of these failed" is the question a debugging
// session actually starts from.
func TestListGrainTasksFiltersByState(t *testing.T) {
	res := callTask(t, worldOfTasks(), "list_grain_tasks", map[string]any{"state": "failed"})
	if res.IsError {
		t.Fatalf("list_grain_tasks reported an error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "task-2") {
		t.Errorf("text = %q, want the failed task in it", res.Text)
	}
	if strings.Contains(res.Text, "task-1") || strings.Contains(res.Text, "task-3") {
		t.Errorf("text = %q, want only the failed task", res.Text)
	}
}

// A limit that cuts the list has to say so: a reader shown 1 of 3 tasks
// with no notice would conclude the deployment has one task.
func TestListGrainTasksSaysWhatALimitDropped(t *testing.T) {
	res := callTask(t, worldOfTasks(), "list_grain_tasks", map[string]any{"limit": float64(1)})
	if res.IsError {
		t.Fatalf("list_grain_tasks reported an error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "2 older matching tasks are not shown") {
		t.Errorf("text = %q, want it to say what was dropped", res.Text)
	}
	// The newest end is what is kept: a debugging question is nearly
	// always about something recent.
	if !strings.Contains(res.Text, "task-3") || strings.Contains(res.Text, "task-1") {
		t.Errorf("text = %q, want the newest task kept and the oldest dropped", res.Text)
	}
}

func TestListGrainTasksSaysWhenNothingMatches(t *testing.T) {
	res := callTask(t, worldOfTasks(), "list_grain_tasks", map[string]any{"state": "closed"})
	if res.IsError {
		t.Fatalf("list_grain_tasks reported an error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "No tasks in state") {
		t.Errorf("text = %q, want a plain \"nothing matched\"", res.Text)
	}
}

// The whole point of read_grain_task: every attempt, and the error grain
// recorded for the ones that failed.
func TestReadGrainTaskShowsAttemptsAndTheirErrors(t *testing.T) {
	res := callTask(t, worldOfTasks(), "read_grain_task", map[string]any{"task_id": "task-2"})
	if res.IsError {
		t.Fatalf("read_grain_task reported an error: %s", res.Text)
	}
	for _, want := range []string{
		"Task task-2: Broken clone",
		"State: failed",
		"Filed by: operator (human)",
		"Consecutive failed attempts: 2",
		"cloning acme/widgets: exit status 128",
		"#1  started 2026-01-02T03:04:05Z",
		"still running",
		"This has failed twice now.",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text = %q, want it to contain %q", res.Text, want)
		}
	}
}

func TestReadGrainTaskReportsAnUnknownTask(t *testing.T) {
	res := callTask(t, worldOfTasks(), "read_grain_task", map[string]any{"task_id": "task-99"})
	if !res.IsError || !strings.Contains(res.Text, "task-99") {
		t.Errorf("text = %q (isError=%v), want an error naming the task", res.Text, res.IsError)
	}
}

func TestReadGrainTaskNeedsATaskID(t *testing.T) {
	res := callTask(t, worldOfTasks(), "read_grain_task", map[string]any{})
	if !res.IsError || !strings.Contains(res.Text, "task_id is required") {
		t.Errorf("text = %q (isError=%v), want the missing-argument error", res.Text, res.IsError)
	}
}

func TestReadGrainTaskPromptReadsWhatTheAgentWasTold(t *testing.T) {
	res := callTask(t, worldOfTasks(), "read_grain_task_prompt", map[string]any{"task_id": "task-2"})
	if res.IsError {
		t.Fatalf("read_grain_task_prompt reported an error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "attempt 2") || !strings.Contains(res.Text, "You are working on task-2.") {
		t.Errorf("text = %q, want the prompt and which attempt got it", res.Text)
	}
}

// A task nothing has been dispatched for is not an error: it is a task
// whose prompt does not exist yet, and saying so plainly is the answer.
func TestReadGrainTaskPromptSaysWhenThereIsNone(t *testing.T) {
	res := callTask(t, worldOfTasks(), "read_grain_task_prompt", map[string]any{"task_id": "task-1"})
	if res.IsError {
		t.Fatalf("read_grain_task_prompt reported an error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "no recorded prompt") {
		t.Errorf("text = %q, want the no-prompt-yet sentence", res.Text)
	}
}

func TestReadGrainTaskTranscriptReadsOneAttempt(t *testing.T) {
	res := callTask(t, worldOfTasks(), "read_grain_task_transcript",
		map[string]any{"task_id": "task-2", "attempt": float64(1)})
	if res.IsError {
		t.Fatalf("read_grain_task_transcript reported an error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "attempt one: git clone failed") {
		t.Errorf("text = %q, want attempt 1's own transcript", res.Text)
	}
}

// Omitting "attempt" means the most recent one, which the tool has to
// resolve off the task's own record -- a caller debugging a task usually
// wants the run that just happened, not the one it has to count to.
func TestReadGrainTaskTranscriptDefaultsToTheLatestAttempt(t *testing.T) {
	res := callTask(t, worldOfTasks(), "read_grain_task_transcript", map[string]any{"task_id": "task-2"})
	if res.IsError {
		t.Fatalf("read_grain_task_transcript reported an error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "attempt two: still going") {
		t.Errorf("text = %q, want the newest attempt's transcript", res.Text)
	}
}

func TestReadGrainTaskTranscriptSaysWhenATaskHasNeverRun(t *testing.T) {
	world := worldOfTasks()
	world.records["task-4"] = TaskRecord{TaskSummary: TaskSummary{ID: "task-4", State: "queued"}}
	res := callTask(t, world, "read_grain_task_transcript", map[string]any{"task_id": "task-4"})
	if !res.IsError || !strings.Contains(res.Text, "no attempts") {
		t.Errorf("text = %q (isError=%v), want the never-ran explanation", res.Text, res.IsError)
	}
}

// A store that cannot be read is reported as such rather than as an
// empty task list, which would read as a deployment with no tasks.
func TestTaskToolsReportAReadFailure(t *testing.T) {
	world := worldOfTasks()
	world.err = errors.New("reaching grain server: connection refused")
	res := callTask(t, world, "list_grain_tasks", nil)
	if !res.IsError || !strings.Contains(res.Text, "connection refused") {
		t.Errorf("text = %q (isError=%v), want the read failure reported", res.Text, res.IsError)
	}
}

// nil reader: all four tools still register, and each says why it can do
// nothing -- what a `grain mcpserver -self-debug` with no -server serves,
// and what each framework's allowedTools enumerates.
func TestTaskToolsRegisterWithoutAReader(t *testing.T) {
	tools := NewTaskTools(nil)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
		res := tool.Handler(context.Background(), map[string]any{"task_id": "task-1"})
		if !res.IsError || !strings.Contains(res.Text, "no route back to the grain daemon") {
			t.Errorf("%s answered %q (isError=%v), want the no-daemon explanation",
				tool.Name, res.Text, res.IsError)
		}
	}
	want := []string{"list_grain_tasks", "read_grain_task", "read_grain_task_prompt", "read_grain_task_transcript"}
	if len(names) != len(want) {
		t.Fatalf("NewTaskTools registered %v, want %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("NewTaskTools registered %v, want %v", names, want)
		}
	}
}

// A transcript longer than one result may carry comes back head-and-tail
// with a notice, the same cap run_command and read_file answer under: a
// run's whole session can be far larger than its own context.
func TestReadGrainTaskTranscriptIsCapped(t *testing.T) {
	world := worldOfTasks()
	world.transcripts["task-2#1"] = strings.Repeat("a line of transcript\n", 10000)
	res := callTask(t, world, "read_grain_task_transcript",
		map[string]any{"task_id": "task-2", "attempt": float64(1)})
	if res.IsError {
		t.Fatalf("read_grain_task_transcript reported an error: %s", res.Text)
	}
	if len(res.Text) > 2*maxToolResultBytes {
		t.Errorf("text is %d bytes, want it capped near %d", len(res.Text), maxToolResultBytes)
	}
	if !strings.Contains(res.Text, "grain's own UI") {
		t.Errorf("text = %q, want the elision notice saying where the rest is", res.Text)
	}
}
