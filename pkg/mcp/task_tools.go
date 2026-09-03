package mcp

// task_tools.go is the read-only window onto grain's *other* tasks that
// the self-debug capability opens: list_grain_tasks, read_grain_task,
// read_grain_task_prompt and read_grain_task_transcript.
//
// Reading grain's own source (pkg/capability/selfdebug's own
// read_grain_source) explains what grain is built to do. It cannot
// explain what this deployment actually did -- why that task failed
// three times in a row, what its agent was really told, which tool call
// it was on when it gave up. That evidence lives in the task store, and
// until now a run had no way to reach any of it: the only task a run
// could see anything of was its own, and only through the prompt it was
// handed at dispatch. So debugging grain from inside grain meant asking
// a human to copy the interesting parts of another task's record into
// the chat by hand.
//
// These tools are served by the `grain mcpserver` process, which runs on
// the controller, and they reach the store the way every other write
// there does -- by asking the daemon over its own REST API
// (cmd/grain/mcpserver.go's daemonTasks), never by opening the store
// from a second process. docs/design.md's split surface is untouched:
// what crosses into the sandbox is rendered text, not a database handle
// and not a credential.
//
// They are registered only for a run whose task holds the self-debug
// grant (mcpserver's own -self-debug, passed by a Framework only when
// agent.RunConfig.SelfDebug says so). Everything they expose is
// everything anyone with the UI open can already read, so the gate is
// the grant a human attached rather than a confirmation per call --
// exactly the line pkg/capability/selfdebug draws for source reading,
// and the opposite of selfrepair's, which can change things.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TaskSummary is one task as a list entry: enough to pick the task worth
// looking at, and no more. It is deliberately not ui.Task -- pkg/ui
// imports pkg/capability/selfdebug, and this package is imported by both
// halves of that, so the wire shape stays out of here and
// cmd/grain/mcpserver.go's adapter projects one into the other.
type TaskSummary struct {
	ID            string
	Title         string
	State         string
	Repo          string
	Interactive   bool
	Configuration bool
	CreatedAt     *time.Time
}

// TaskAttempt is one run of a task: when it started, how it ended, and
// the one sentence grain recorded about how it got there
// (model.Run.Detail -- a tool error, the framework's own error text, or
// "finished without pushing, asking, or commenting"). This is where the
// errors are.
type TaskAttempt struct {
	Number     int
	StartedAt  time.Time
	FinishedAt *time.Time
	Outcome    string
	Detail     string
}

// TaskComment is one entry in a task's conversation, author included:
// who said a thing matters as much as what was said when the question is
// "why did this run do that", since a comment from a human is an
// instruction and one from grain is a relay.
type TaskComment struct {
	Author     string
	AuthorKind string
	Body       string
	CreatedAt  time.Time
}

// TaskRecord is one task in full: the summary above, plus what it asked
// for, what happened on every attempt, and everything said about it.
type TaskRecord struct {
	TaskSummary
	Description       string
	Author            string
	AuthorKind        string
	Base              string
	Capabilities      []string
	PullRequest       string
	FailedAttempts    int
	LastFailureAt     *time.Time
	LastFailureReason string
	Attempts          []TaskAttempt
	Comments          []TaskComment
}

// TaskPromptRecord is the prompt one attempt was actually handed, and
// which attempt that was -- a redispatched task's prompt grows with its
// conversation, so attempt 3's is not attempt 1's, and a reader needs to
// know which one it is holding.
type TaskPromptRecord struct {
	Prompt  string
	Attempt int
}

// TaskReader is the read-only slice of grain's own task store these
// tools need. Four reads, no writes: what an agent can reach through
// here is legible at a glance, the same reason PullRequestReader next
// door is narrowed to six methods of github.Client rather than being
// that whole interface.
//
// Every method takes a task id, and none of them is this run's own task
// -- that is the point of these tools. There is no scope to pin the way
// PullRequestScope pins a branch: a debugging question is about whichever
// task went wrong, which is never known when the process starts.
type TaskReader interface {
	ListTasks(ctx context.Context) ([]TaskSummary, error)
	Task(ctx context.Context, id string) (TaskRecord, error)
	TaskPrompt(ctx context.Context, id string) (TaskPromptRecord, error)
	AttemptTranscript(ctx context.Context, id string, attempt int) (string, error)
}

// NewTaskTools returns the four tools a self-debug run gets for reading
// grain's other tasks.
//
// reader nil returns all four anyway, refusing every call with a
// sentence saying why -- what lets each agent framework's allowedTools
// enumerate the names this package registers without holding a live
// reader, exactly as NewPullRequestTools and NewRecreateSandboxTools
// already do, and what a `grain mcpserver` given -self-debug but no
// -server (no daemon to ask) serves.
func NewTaskTools(reader TaskReader) []Tool {
	return []Tool{
		listTasksTool(reader),
		readTaskTool(reader),
		readTaskPromptTool(reader),
		readTaskTranscriptTool(reader),
	}
}

// noTaskReader is the answer every one of these tools gives when it has
// no route to the store -- a sentence about this run rather than "unknown
// tool", which would read like a bug in grain rather than a fact about
// how this one was dispatched.
const noTaskReader = "This run cannot read grain's other tasks: it holds the self-debug grant but was " +
	"dispatched with no route back to the grain daemon that owns the task store. Say so if it " +
	"matters to what you were asked to do; nothing you can do from here will change it."

// defaultTaskListLimit is how many tasks list_grain_tasks answers with
// when the call names no limit of its own. A deployment's store holds
// every task it has ever run, and a debugging question is nearly always
// about a recent one, so an unbounded list would spend a run's context
// on rows nobody asked for -- the same reasoning result_size.go's own
// cap follows for command output.
const defaultTaskListLimit = 50

var listTasksInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"state": map[string]any{
			"type": "string",
			"description": "only list tasks in this state: proposed, queued, running, awaiting_reply, " +
				"failed, awaiting_submit, completed or closed. Omit for every state.",
		},
		"limit": map[string]any{
			"type":        "number",
			"description": fmt.Sprintf("how many tasks to return at most (default %d)", defaultTaskListLimit),
		},
	},
}

func listTasksTool(reader TaskReader) Tool {
	return Tool{
		Name: "list_grain_tasks",
		Description: "List the other tasks in this grain deployment -- id, title, state, repo -- so you " +
			"can find the one behind whatever you are debugging or explaining. Read-only, and it " +
			"needs no permission. Narrow it with \"state\" (e.g. failed, running) and \"limit\"; then " +
			"use read_grain_task for one task's full record.",
		InputSchema: listTasksInputSchema,
		Handler: func(ctx context.Context, args map[string]any) Result {
			if reader == nil {
				return Result{Text: noTaskReader, IsError: true}
			}
			tasks, err := reader.ListTasks(ctx)
			if err != nil {
				return Result{Text: fmt.Sprintf("Error listing grain's tasks: %v", err), IsError: true}
			}
			state, _ := argString(args, "state")
			limit := defaultTaskListLimit
			if f, ok := argFloat(args, "limit"); ok && int(f) > 0 {
				limit = int(f)
			}
			return Result{Text: renderTaskList(tasks, state, limit)}
		},
	}
}

// renderTaskList is one line per task, newest last the way the store
// itself orders them, with a count and any elision stated outright: a
// list that silently stopped at 50 would have the reader concluding
// there are 50 tasks.
func renderTaskList(tasks []TaskSummary, state string, limit int) string {
	matched := make([]TaskSummary, 0, len(tasks))
	for _, t := range tasks {
		if state == "" || strings.EqualFold(t.State, state) {
			matched = append(matched, t)
		}
	}
	if len(matched) == 0 {
		if state != "" {
			return fmt.Sprintf("No tasks in state %q.", state)
		}
		return "This deployment has no tasks at all."
	}

	shown := matched
	dropped := 0
	if len(shown) > limit {
		dropped = len(shown) - limit
		shown = shown[dropped:]
	}

	var b strings.Builder
	for _, t := range shown {
		fmt.Fprintf(&b, "%s  [%s]", t.ID, t.State)
		if t.Repo != "" {
			fmt.Fprintf(&b, "  %s", t.Repo)
		}
		if t.Configuration {
			b.WriteString("  (configuration agent)")
		} else if t.Interactive {
			b.WriteString("  (interactive)")
		}
		if t.CreatedAt != nil {
			fmt.Fprintf(&b, "  filed %s", t.CreatedAt.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "\n    %s\n", singleLine(t.Title))
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "\n%d older matching tasks are not shown -- raise \"limit\" to see them.\n", dropped)
	}
	fmt.Fprintf(&b, "\n%d task(s) shown of %d matching.", len(shown), len(matched))
	return b.String()
}

var taskIDInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"task_id": map[string]any{
			"type":        "string",
			"description": "id of the task to read, as list_grain_tasks reports it",
		},
	},
	"required": []string{"task_id"},
}

func readTaskTool(reader TaskReader) Tool {
	return Tool{
		Name: "read_grain_task",
		Description: "Read one grain task's whole record: what it asked for, who filed it, which " +
			"capabilities it holds, every attempt it has had with how each one ended (the error " +
			"grain recorded for a failed run), and its full conversation. Read-only, and it needs " +
			"no permission. This is where you look to explain why another task behaved the way it " +
			"did; read_grain_task_prompt and read_grain_task_transcript give you the two big " +
			"pieces this deliberately leaves out.",
		InputSchema: taskIDInputSchema,
		Handler: func(ctx context.Context, args map[string]any) Result {
			if reader == nil {
				return Result{Text: noTaskReader, IsError: true}
			}
			id, _ := argString(args, "task_id")
			if id == "" {
				return Result{Text: "task_id is required", IsError: true}
			}
			task, err := reader.Task(ctx, id)
			if err != nil {
				return Result{Text: fmt.Sprintf("Error reading task %s: %v", id, err), IsError: true}
			}
			return Result{Text: elideMiddle(renderTaskRecord(task), maxToolResultBytes, elisionAdviceTaskRecord)}
		},
	}
}

// elisionAdviceTaskRecord is what to do about a task record too long to
// carry back whole -- name the narrower call rather than saying
// "truncated" and leaving the run to re-read the same thing, the rule
// result_size.go's own advice constants follow.
const elisionAdviceTaskRecord = "read the pieces on their own instead -- read_grain_task_prompt for what " +
	"the agent was told, and read_grain_task_transcript for one attempt's own story"

func renderTaskRecord(t TaskRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task %s: %s\n", t.ID, singleLine(t.Title))
	fmt.Fprintf(&b, "State: %s\n", t.State)
	if t.Author != "" {
		fmt.Fprintf(&b, "Filed by: %s (%s)\n", t.Author, t.AuthorKind)
	}
	if t.CreatedAt != nil {
		fmt.Fprintf(&b, "Filed at: %s\n", t.CreatedAt.UTC().Format(time.RFC3339))
	}
	if t.Repo != "" {
		fmt.Fprintf(&b, "Repo: %s", t.Repo)
		if t.Base != "" {
			fmt.Fprintf(&b, " (base %s)", t.Base)
		}
		b.WriteByte('\n')
	}
	if t.Configuration {
		b.WriteString("Kind: configuration agent\n")
	} else if t.Interactive {
		b.WriteString("Kind: interactive\n")
	}
	if len(t.Capabilities) > 0 {
		fmt.Fprintf(&b, "Capabilities: %s\n", strings.Join(t.Capabilities, ", "))
	}
	if t.PullRequest != "" {
		fmt.Fprintf(&b, "Pull request: %s\n", t.PullRequest)
	}
	if t.FailedAttempts > 0 {
		fmt.Fprintf(&b, "Consecutive failed attempts: %d\n", t.FailedAttempts)
		if t.LastFailureReason != "" {
			fmt.Fprintf(&b, "Last failure: %s\n", singleLine(t.LastFailureReason))
		}
	}

	b.WriteString("\nDescription:\n")
	b.WriteString(indent(blankAsNone(t.Description, "(none)")))

	b.WriteString("\nAttempts:\n")
	if len(t.Attempts) == 0 {
		b.WriteString("  (none -- nothing has been dispatched for this task)\n")
	}
	for _, a := range t.Attempts {
		fmt.Fprintf(&b, "  #%d  started %s", a.Number, a.StartedAt.UTC().Format(time.RFC3339))
		if a.FinishedAt != nil {
			fmt.Fprintf(&b, ", finished %s", a.FinishedAt.UTC().Format(time.RFC3339))
		} else {
			b.WriteString(", still running")
		}
		if a.Outcome != "" {
			fmt.Fprintf(&b, ", outcome %s", a.Outcome)
		}
		b.WriteByte('\n')
		if a.Detail != "" {
			fmt.Fprintf(&b, "      %s\n", singleLine(a.Detail))
		}
	}

	fmt.Fprintf(&b, "\nConversation (%d):\n", len(t.Comments))
	for _, c := range t.Comments {
		fmt.Fprintf(&b, "  [%s] %s (%s):\n", c.CreatedAt.UTC().Format(time.RFC3339), c.Author, c.AuthorKind)
		b.WriteString(indent(c.Body))
	}
	return strings.TrimRight(b.String(), "\n")
}

func readTaskPromptTool(reader TaskReader) Tool {
	return Tool{
		Name: "read_grain_task_prompt",
		Description: "Read the prompt another grain task's agent was actually handed on its most recent " +
			"attempt -- the whole assembled text, not the task's own description. Read-only, and it " +
			"needs no permission. This is the answer to \"why did that run think it was supposed to " +
			"do that\": what a task asks for and what its agent was told are not the same string.",
		InputSchema: taskIDInputSchema,
		Handler: func(ctx context.Context, args map[string]any) Result {
			if reader == nil {
				return Result{Text: noTaskReader, IsError: true}
			}
			id, _ := argString(args, "task_id")
			if id == "" {
				return Result{Text: "task_id is required", IsError: true}
			}
			prompt, err := reader.TaskPrompt(ctx, id)
			if err != nil {
				return Result{Text: fmt.Sprintf("Error reading task %s's prompt: %v", id, err), IsError: true}
			}
			if prompt.Prompt == "" {
				return Result{Text: fmt.Sprintf("Task %s has no recorded prompt: nothing has been "+
					"dispatched for it yet, or every attempt failed before its agent got a turn.", id)}
			}
			header := fmt.Sprintf("Prompt given to task %s, attempt %d:\n\n", id, prompt.Attempt)
			return Result{Text: header + elideMiddle(prompt.Prompt, maxToolResultBytes, elisionAdviceTaskText)}
		},
	}
}

// elisionAdviceTaskText is the advice for the two tools whose answer is
// one long recorded string. Unlike a task record there is no narrower
// call to make -- the head and the tail are what there is -- so it says
// where the rest can be read instead of naming a tool that would return
// the same thing again.
const elisionAdviceTaskText = "the whole of it is in grain's own UI, on that task's page -- ask whoever " +
	"you are working with to look at the part you need if the middle turns out to matter"

var transcriptInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"task_id": map[string]any{
			"type":        "string",
			"description": "id of the task whose transcript to read, as list_grain_tasks reports it",
		},
		"attempt": map[string]any{
			"type": "number",
			"description": "which attempt's transcript to read, as read_grain_task numbers them; " +
				"omit for that task's most recent attempt",
		},
	},
	"required": []string{"task_id"},
}

func readTaskTranscriptTool(reader TaskReader) Tool {
	return Tool{
		Name: "read_grain_task_transcript",
		Description: "Read another grain task's session transcript -- one attempt's whole story, its " +
			"agent's output interleaved with every tool call it made and what came back. Read-only, " +
			"and it needs no permission. Omit \"attempt\" for the most recent one. A run still in " +
			"flight has a transcript too, as far as it has got.",
		InputSchema: transcriptInputSchema,
		Handler: func(ctx context.Context, args map[string]any) Result {
			if reader == nil {
				return Result{Text: noTaskReader, IsError: true}
			}
			id, _ := argString(args, "task_id")
			if id == "" {
				return Result{Text: "task_id is required", IsError: true}
			}
			attempt := 0
			if f, ok := argFloat(args, "attempt"); ok {
				attempt = int(f)
			}
			if attempt <= 0 {
				latest, problem := latestAttempt(ctx, reader, id)
				if problem != "" {
					return Result{Text: problem, IsError: true}
				}
				attempt = latest
			}
			transcript, err := reader.AttemptTranscript(ctx, id, attempt)
			if err != nil {
				return Result{Text: fmt.Sprintf("Error reading task %s attempt %d's transcript: %v",
					id, attempt, err), IsError: true}
			}
			if transcript == "" {
				return Result{Text: fmt.Sprintf("Task %s attempt %d recorded no transcript. Some agent "+
					"frameworks do not write one; read_grain_task still has that attempt's outcome "+
					"and the error grain recorded for it.", id, attempt)}
			}
			header := fmt.Sprintf("Transcript of task %s, attempt %d:\n\n", id, attempt)
			return Result{Text: header + elideMiddle(transcript, maxToolResultBytes, elisionAdviceTaskText)}
		},
	}
}

// latestAttempt resolves "omit attempt for the most recent one" -- the
// task's own record is the only thing that knows how many attempts there
// have been, so this costs a second read rather than being guessed at
// from a number the caller supplied.
//
// It answers with a sentence for the agent rather than an error, since
// both ways of failing here (the task cannot be read, or it has never
// run) are facts about that task rather than something the caller got
// wrong, and both are what the tool result should say verbatim.
func latestAttempt(ctx context.Context, reader TaskReader, id string) (attempt int, problem string) {
	task, err := reader.Task(ctx, id)
	if err != nil {
		return 0, fmt.Sprintf("Error finding task %s's most recent attempt: %v", id, err)
	}
	if len(task.Attempts) == 0 {
		return 0, fmt.Sprintf("Task %s has had no attempts, so there is no transcript to read.", id)
	}
	return task.Attempts[len(task.Attempts)-1].Number, ""
}

// singleLine flattens a string onto one line, for the places that print
// one field per line: a title or an error detail with a newline in it
// would otherwise break the shape of the whole listing.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func blankAsNone(s, none string) string {
	if strings.TrimSpace(s) == "" {
		return none
	}
	return s
}

// indent shifts a block of prose in by two spaces, so a description or a
// comment body cannot be mistaken for the record's own fields.
func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	return b.String()
}
