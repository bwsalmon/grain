package mcp

import (
	"context"
	"fmt"
	"sync"
)

// ProposedTask is one propose_task call, recorded rather than filed as a
// real GitHub issue -- v2 has no GitHub client yet.
type ProposedTask struct {
	ID        string
	Title     string
	Body      string
	DependsOn []string
}

// ReviewComment is one add_review_comment call, recorded rather than
// attached to a real draft review.
type ReviewComment struct {
	Body string
	Path string
	Line int
	// HasLocation distinguishes "no path/line given" from "path/line
	// given as the zero value", since 0 is a real line number.
	HasLocation bool
}

// MockSink accumulates every escape-hatch tool call made during one run, in
// place of the real effect: no GitHub comment, issue, or review actually
// gets posted. It exists so a caller (a test, or eventually a v2 core.py
// equivalent) can inspect what an agent asked for without that agent having
// had any live GitHub credential in reach at all.
type MockSink struct {
	mu             sync.Mutex
	question       string
	comment        string
	proposedTasks  []ProposedTask
	reviewComments []ReviewComment
}

func (s *MockSink) Question() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.question
}

func (s *MockSink) Comment() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.comment
}

func (s *MockSink) ProposedTasks() []ProposedTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProposedTask, len(s.proposedTasks))
	copy(out, s.proposedTasks)
	return out
}

func (s *MockSink) ReviewComments() []ReviewComment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ReviewComment, len(s.reviewComments))
	copy(out, s.reviewComments)
	return out
}

// NewMockTools returns v1's four escape-hatch tools -- ask_question,
// comment_on_issue, propose_task, add_review_comment -- with identical
// names, descriptions and input schemas, so an agent's understanding of
// when and how to call them carries over unchanged. Only the effect
// differs: v1 writes a file for core.py to turn into a real GitHub call
// once the dispatch finishes; here nothing outside sink is touched, and the
// confirmation text says so plainly rather than repeating v1's "this will
// be posted" claim.
func NewMockTools(sink *MockSink) []Tool {
	return []Tool{
		askQuestionTool(sink),
		commentOnIssueTool(sink),
		proposeTaskTool(sink),
		addReviewCommentTool(sink),
	}
}

func askQuestionTool(sink *MockSink) Tool {
	return Tool{
		Name: "ask_question",
		Description: "Ask the human a clarifying question when you cannot safely or " +
			"correctly proceed without their input. This ends your turn: the " +
			"question is posted as a comment on the GitHub issue or pull " +
			"request, the task is taken out of the queue, and a human must " +
			"reply and re-apply the trigger label before another attempt " +
			"runs. Do not call this for routine progress updates or when you " +
			"can reasonably proceed on your own judgment -- only when you are " +
			"genuinely blocked. After calling this, do not take any further " +
			"actions.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"question": map[string]any{"type": "string"},
			},
			"required": []string{"question"},
		},
		Handler: func(_ context.Context, args map[string]any) Result {
			question, ok := argString(args, "question")
			if !ok || question == "" {
				return Result{Text: "question is required", IsError: true}
			}
			sink.mu.Lock()
			sink.question = question
			sink.mu.Unlock()
			return Result{Text: "Recorded (mocked -- no GitHub comment was posted). In a real " +
				"dispatch this ends your turn and waits on a human reply; treat it the same way here."}
		},
	}
}

func commentOnIssueTool(sink *MockSink) Tool {
	return Tool{
		Name: "comment_on_issue",
		Description: "Leave a comment on the task's GitHub issue. Use this when the " +
			"task only asked for an answer, an investigation, or a " +
			"recommendation -- not a code change: if you never push a " +
			"branch, this comment becomes the task's closing note and no " +
			"pull request is opened. If you do push commits, a pull " +
			"request is opened for them regardless of whether you also " +
			"call this -- calling it does not by itself prevent a pull " +
			"request from opening. After calling this, do not take any " +
			"further actions unless you still intend to push commits.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"comment": map[string]any{"type": "string"},
			},
			"required": []string{"comment"},
		},
		Handler: func(_ context.Context, args map[string]any) Result {
			comment, ok := argString(args, "comment")
			if !ok || comment == "" {
				return Result{Text: "comment is required", IsError: true}
			}
			sink.mu.Lock()
			sink.comment = comment
			sink.mu.Unlock()
			return Result{Text: "Recorded (mocked -- no GitHub comment was posted)."}
		},
	}
}

func proposeTaskTool(sink *MockSink) Tool {
	return Tool{
		Name: "propose_task",
		Description: "Propose a new task for the task queue -- for example, " +
			"splitting follow-up work out of the task you were given, or " +
			"flagging work you noticed is needed but is out of scope for " +
			"this one. Each proposed task is filed as a new GitHub issue " +
			"once this run finishes, but it requires a human to apply the " +
			"trigger label (or comment /lgtm) before the agent set will " +
			"ever attempt it -- proposing a task never starts it. Call " +
			"this once per task you want to propose. Give a short `id` if " +
			"a later propose_task call in this same run should list this " +
			"one in its own depends_on -- for example, to propose a small " +
			"chain of tasks where the second can't start until the first " +
			"is done.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"id": map[string]any{
					"type": "string",
					"description": "A short local name for this proposal, used only " +
						"so a later propose_task call in this same run can " +
						"name it in its own depends_on -- not shown to the " +
						"human and not the eventual issue number.",
				},
				"title": map[string]any{"type": "string"},
				"body":  map[string]any{"type": "string"},
				"depends_on": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
					"description": "Existing issue numbers, and/or ids given to " +
						"earlier propose_task calls in this same run, that " +
						"this task must wait on before it can start.",
				},
			},
			"required": []string{"title", "body"},
		},
		Handler: func(_ context.Context, args map[string]any) Result {
			title, hasTitle := argString(args, "title")
			body, hasBody := argString(args, "body")
			if !hasTitle || !hasBody {
				return Result{Text: "title and body are required", IsError: true}
			}
			id, _ := argString(args, "id")
			task := ProposedTask{ID: id, Title: title, Body: body, DependsOn: argStringSlice(args, "depends_on")}

			sink.mu.Lock()
			sink.proposedTasks = append(sink.proposedTasks, task)
			n := len(sink.proposedTasks)
			sink.mu.Unlock()

			return Result{Text: fmt.Sprintf("Recorded (%d proposed task(s) so far this run, mocked -- "+
				"no GitHub issue was filed).", n)}
		},
	}
}

func addReviewCommentTool(sink *MockSink) Tool {
	return Tool{
		Name: "add_review_comment",
		Description: "Leave one piece of feedback as part of a draft code review on " +
			"the pull request you were asked to review. Give both `path` " +
			"and `line` to attach the comment to a specific line of a " +
			"specific file, as shown in the diff against the base branch; " +
			"omit both for a general remark that isn't tied to one line. " +
			"Call this once per point of feedback -- every call this run " +
			"accumulates into a single draft review, posted once you " +
			"finish. This never pushes commits and never posts anything " +
			"immediately: nothing is visible to anyone until a human opens " +
			"the draft review and submits it themselves.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"body": map[string]any{"type": "string"},
				"path": map[string]any{
					"type": "string",
					"description": "File path, relative to the repo root, the comment " +
						"applies to. Omit for a general remark.",
				},
				"line": map[string]any{
					"type": "integer",
					"description": "Line number in the new version of the file (as " +
						"shown in the diff) the comment applies to. " +
						"Required if path is given.",
				},
			},
			"required": []string{"body"},
		},
		Handler: func(_ context.Context, args map[string]any) Result {
			body, ok := argString(args, "body")
			if !ok || body == "" {
				return Result{Text: "body is required", IsError: true}
			}
			path, hasPath := argString(args, "path")
			lineF, hasLine := argFloat(args, "line")
			if hasPath != hasLine {
				return Result{
					Text: "Give both path and line to attach this to a specific " +
						"line, or neither for a general remark -- not just one.",
					IsError: true,
				}
			}

			comment := ReviewComment{Body: body, HasLocation: hasPath}
			if hasPath {
				comment.Path = path
				comment.Line = int(lineF)
			}

			sink.mu.Lock()
			sink.reviewComments = append(sink.reviewComments, comment)
			n := len(sink.reviewComments)
			sink.mu.Unlock()

			return Result{Text: fmt.Sprintf("Recorded (%d comment(s) so far this run, mocked -- "+
				"no draft review exists on GitHub).", n)}
		},
	}
}
