package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode"
)

// ProposedTask is one propose_task call as this process recorded it.
// Filing it as a real task happens elsewhere and later
// (orchestrator.relayProposedTasks, off agent.Result.ToolCalls); this is
// the shape a caller holding a MockSink sees.
//
// AutoMerge is a pointer because "the agent said nothing" and "the agent
// said false" are different answers: an unset auto_merge inherits the
// proposing task's own setting (orchestrator.relayProposedTasks says why),
// and an explicit false opts a proposal out of an inheritance it would
// otherwise get.
type ProposedTask struct {
	ID        string
	Title     string
	Body      string
	DependsOn []string
	AutoMerge *bool
}

// ReviewComment is one add_review_comment call, recorded rather than
// attached to a real draft review -- and, unlike the other three,
// recorded is all it ever is: nothing downstream relays it (see
// addReviewCommentTool).
type ReviewComment struct {
	Body string
	Path string
	Line int
	// HasLocation distinguishes "no path/line given" from "path/line
	// given as the zero value", since 0 is a real line number.
	HasLocation bool
}

// SecretRequest is one request_secret call: the name of a credential a
// run needs and cannot have, and what the run says it is for.
//
// No value is anywhere in this shape, and none ever passes through this
// package. The whole point of the tool is that the human types the value
// into grain's own UI and it goes straight into the secret store -- see
// requestSecretTool.
type SecretRequest struct {
	Secret string
	Reason string
}

// MockSink accumulates every escape-hatch tool call made during one run,
// so a caller (a test, or an in-process client) can inspect what an agent
// asked for. It is the whole of what *this process* does with such a
// call: nothing is created from here, and no agent ever has a live
// GitHub credential in reach.
//
// It is a test seam, not the production path. What a question, a closing
// comment or a proposal actually turns into happens after the run, from
// agent.Result.ToolCalls, in orchestrator.ProcessResult -- which is why
// the tools below describe that and never mention this sink. See
// NewMockTools.
type MockSink struct {
	mu             sync.Mutex
	question       string
	comment        string
	secretRequest  *SecretRequest
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

// SecretRequest is the credential this run asked a human to set, or nil
// if it asked for none.
func (s *MockSink) SecretRequest() *SecretRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secretRequest == nil {
		return nil
	}
	req := *s.secretRequest
	return &req
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

// NewMockTools returns the five escape-hatch tools -- ask_question,
// request_secret, comment_on_issue, propose_task, add_review_comment --
// four of them under the names v1 gave them, which is also the
// vocabulary orchestrator.ProcessResult matches against (see
// toolnames.go).
//
// "Mock" describes this process, not the effect. Nothing outside sink is
// touched from here, but three of these four really do reach the human:
// ProcessResult recovers each call from agent.Result.ToolCalls once the
// run finishes and relays it for real -- a question becomes a comment
// the task is parked on until somebody replies, a closing comment
// becomes a comment on the task, a proposal becomes an unapproved task
// with real depends_on edges. So the descriptions and the confirmations
// below say *that*, in v2's own vocabulary (task, conversation, task
// queue), and say nothing about the sink.
//
// They used to say the opposite -- "mocked -- no GitHub comment was
// posted" -- on every production run, and describe v1's GitHub issue and
// trigger label besides. Neither has been true since tasks became rows,
// and the cost fell on the run: an agent told its question went nowhere
// asks it again, works around the block it was supposed to park on, or
// downgrades it into its final text, which nothing relays at all
// (docs/agent-ergonomics.md, finding 1).
//
// add_review_comment is the one exception, and its own text says so:
// nothing relays it anywhere (ProcessResult's doc comment), because the
// review dispatch that would have a pull request to attach a draft
// review to does not exist yet. Rather than promise a draft review no
// human will ever open, it points anything that needs to be read at
// comment_on_issue.
func NewMockTools(sink *MockSink) []Tool {
	return []Tool{
		askQuestionTool(sink),
		requestSecretTool(sink),
		commentOnIssueTool(sink),
		proposeTaskTool(sink),
		addReviewCommentTool(sink),
	}
}

func askQuestionTool(sink *MockSink) Tool {
	return Tool{
		Name: "ask_question",
		Description: "Ask the human a clarifying question when you cannot safely or " +
			"correctly proceed without their input. This ends your turn: " +
			"when this run finishes, grain relays the question into this " +
			"task's own conversation, in grain's UI, and parks the task on " +
			"it -- the task is not attempted again until a human replies " +
			"there, and their reply puts it back in the queue. The next " +
			"attempt is given that whole conversation, so the answer reaches " +
			"the run that carries on this work. Only the first ask_question " +
			"call in a run is relayed. Do not call this for routine progress " +
			"updates or when you can reasonably proceed on your own " +
			"judgment -- only when you are genuinely blocked. After calling " +
			"this, do not take any further actions.",
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
			return Result{Text: "Recorded. When this run finishes, grain relays this question into the " +
				"task's conversation and parks the task there until a human replies. This ends " +
				"your turn: stop here and take no further actions."}
		},
	}
}

// requestSecretTool is ask_question for the one answer a question must
// never be used to get: a credential.
//
// It parks the task the same way, through the same
// Observation.PendingQuestionCommentID, and for the same reason -- the
// run cannot go on until a human acts. What differs is what the human is
// offered. A parked question offers a reply box, and a reply is a
// comment: stored in the task's conversation in plain text, shown to
// everyone who opens the task, and fed back into the next run's own
// prompt (orchestrator's commentThreadSection). A credential pasted
// there is a credential handed to an agent and written into grain's
// state repository as prose. So this call makes the UI offer a
// write-only box instead, whose value goes straight into the secret
// store (ui.Client.SetPendingSecret) and into no comment, no prompt and
// no tool result.
//
// The agent therefore never learns the value it asked for -- deliberately,
// and the description says so plainly rather than leaving a run to
// discover it by asking for the secret back. What a later run gets is
// not the material but the effect: whatever resolves that name -- a
// capability's own credential (model.CredentialRef), grain's own
// github/agent keys -- resolves where it previously did not.
func requestSecretTool(sink *MockSink) Tool {
	return Tool{
		Name: "request_secret",
		Description: "Ask the human to set a credential this deployment does not " +
			"have and this work needs -- an API key, a token, a password. " +
			"This ends your turn the way ask_question does: when this run " +
			"finishes, grain relays the request into this task's own " +
			"conversation, parks the task on it, and offers a box in the " +
			"task pane for the value. Only the first request_secret call " +
			"in a run is relayed. What the human types goes straight into " +
			"grain's encrypted secret store and is never shown to you: not " +
			"in this run, not in a later one, and there is nothing here " +
			"that reads a stored value back. A later run gets the use of " +
			"it, not the material -- whatever resolves that name resolves " +
			"where it did not before. For the same reason, never ask for a " +
			"credential through ask_question or comment_on_issue: a reply " +
			"there is a plain-text comment on the task, and it is fed back " +
			"to the agent. After calling this, do not take any further " +
			"actions.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"secret": map[string]any{
					"type": "string",
					"description": "The name to store it under, as grain resolves " +
						"one: \"stripe-api-key\" for a secret holding a single " +
						"value, or \"<secret>/<key>\" (\"github-app/app-id\") to " +
						"name one key of a secret that holds several. Use the " +
						"name whatever will consume it already looks up, if " +
						"something does.",
				},
				"reason": map[string]any{
					"type": "string",
					"description": "What the credential is, where the human gets " +
						"one, and what it will be used for -- this is all they " +
						"have to go on when deciding what to paste, and it is " +
						"relayed to them verbatim.",
				},
			},
			"required": []string{"secret", "reason"},
		},
		Handler: func(_ context.Context, args map[string]any) Result {
			secret, ok := argString(args, "secret")
			if !ok || secret == "" {
				return Result{Text: "secret is required", IsError: true}
			}
			if !validSecretName(secret) {
				return Result{
					Text: fmt.Sprintf("%q is not a name grain can store a secret under: "+
						"give \"<secret>\" or \"<secret>/<key>\", each part non-empty and "+
						"free of spaces, backslashes and path components like \".\".", secret),
					IsError: true,
				}
			}
			reason, ok := argString(args, "reason")
			if !ok || reason == "" {
				return Result{
					Text: "reason is required: the human deciding what to paste is " +
						"told nothing else about it",
					IsError: true,
				}
			}
			sink.mu.Lock()
			// First call wins, matching what is actually relayed:
			// orchestrator.ProcessResult reads the first non-error
			// request_secret call off agent.Result.ToolCalls.
			if sink.secretRequest == nil {
				sink.secretRequest = &SecretRequest{Secret: secret, Reason: reason}
			}
			sink.mu.Unlock()
			return Result{Text: fmt.Sprintf("Recorded. When this run finishes, grain asks for %s in the "+
				"task's conversation and parks the task there until somebody sets it. The value "+
				"goes into grain's secret store and never comes back to you. This ends your turn: "+
				"stop here and take no further actions.", secret)}
		},
	}
}

// validSecretName reports whether name is one secrets.Store.Resolve
// could take: "<secret>" or "<secret>/<key>", each part non-empty, not
// "." or "..", and carrying no whitespace or backslash. It is the same
// rule secrets.validComponent applies at the point of writing, checked
// here so a run learns it named something unstorable in the turn it made
// the call rather than by being parked on a request no box can be built
// for.
func validSecretName(name string) bool {
	secret, key, explicit := strings.Cut(name, "/")
	if !validSecretComponent(secret) {
		return false
	}
	return !explicit || validSecretComponent(key)
}

func validSecretComponent(s string) bool {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsSpace)
}

func commentOnIssueTool(sink *MockSink) Tool {
	return Tool{
		Name: "comment_on_issue",
		Description: "Leave a comment on this task. Despite the name there is no " +
			"GitHub issue behind it: when this run finishes, grain relays " +
			"the comment into the task's own conversation, in grain's UI, " +
			"where a human reads it and where the next attempt at this task " +
			"reads it back. Use this when the task only asked for an " +
			"answer, an investigation, or a recommendation -- not a code " +
			"change: if you never push a branch and never ask a question, " +
			"this comment becomes the task's closing note and completes " +
			"it. If you do push commits, a pull request is opened for them " +
			"regardless of whether you also call this -- calling it does " +
			"not by itself prevent a pull request from opening. Only the " +
			"first comment_on_issue call in a run is relayed, so say " +
			"everything you have to say in one call. After calling this, do " +
			"not take any further actions unless you still intend to push " +
			"commits or to end your turn with ask_question -- a comment and " +
			"a question are both relayed, in that order.",
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
			return Result{Text: "Recorded. When this run finishes, grain relays this comment into the " +
				"task's conversation; with no branch pushed and no question asked, it is the " +
				"task's closing note."}
		},
	}
}

func proposeTaskTool(sink *MockSink) Tool {
	return Tool{
		Name: "propose_task",
		Description: "Propose a new task for the task queue -- for example, " +
			"splitting follow-up work out of the task you were given, or " +
			"flagging work you noticed is needed but is out of scope for " +
			"this one. Each proposal is filed as a real task once this run " +
			"finishes, linked back to this one, and filed unapproved: it " +
			"sits in the queue as proposed, and a human has to approve it " +
			"in grain's UI before it is ever dispatched -- proposing a task " +
			"never starts it. Call this once per task you want to " +
			"propose. Give a short `id` if " +
			"a later propose_task call in this same run should list this " +
			"one in its own depends_on -- for example, to propose a small " +
			"chain of tasks where the second can't start until the first " +
			"is done. Say what the new task has to wait on in depends_on: " +
			"the task you are working on now, when the follow-up only " +
			"makes sense once this one's change has landed, and any " +
			"earlier proposal in this same run it builds on. A proposal " +
			"with no depends_on can be approved and dispatched alongside " +
			"yours, so leaving one off is what puts two agents in the same " +
			"code at once. A proposal that is a piece of the task you were " +
			"given lands the way the whole of it would have, auto-merge " +
			"included, so leave auto_merge alone for one of those; set it " +
			"to false on a proposal that is separate work and deserves a " +
			"review of its own.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"id": map[string]any{
					"type": "string",
					"description": "A short local name for this proposal, used only " +
						"so a later propose_task call in this same run can " +
						"name it in its own depends_on -- not shown to the " +
						"human and not the id the task is eventually filed " +
						"under.",
				},
				"title": map[string]any{"type": "string"},
				"body":  map[string]any{"type": "string"},
				"depends_on": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
					"description": "Existing task ids (the task you are working on " +
						"now included), and/or ids given to earlier " +
						"propose_task calls in this same run, that this task " +
						"must wait on before it can start. Each one is filed " +
						"as a real dependency: the proposal stays blocked " +
						"until everything it names is done, even after a " +
						"human approves it.",
				},
				"auto_merge": map[string]any{
					"type": "boolean",
					"description": "Whether this proposal's own pull request should " +
						"merge without human review. Defaults to whatever " +
						"the task you are working on now does, which is what " +
						"you want for a piece of that same task; pass false " +
						"for separate work that deserves its own review. " +
						"Passing true cannot grant more than your own task " +
						"has -- if your task is not an auto-merge job, " +
						"neither is anything you propose.",
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
			if autoMerge, ok := argBool(args, "auto_merge"); ok {
				task.AutoMerge = &autoMerge
			}

			sink.mu.Lock()
			sink.proposedTasks = append(sink.proposedTasks, task)
			n := len(sink.proposedTasks)
			sink.mu.Unlock()

			return Result{Text: fmt.Sprintf("Recorded (%d proposed task(s) so far this run). Each is "+
				"filed as a real task in grain's queue when this run finishes, unapproved until "+
				"a human approves it there.", n)}
		},
	}
}

// addReviewCommentTool is the one escape hatch with nothing downstream
// of it. Its calls are recovered from agent.Result.ToolCalls like the
// others', and then relayed nowhere: attaching a draft review needs a
// pull request in hand, which only a review dispatch would have, and
// grain does not dispatch reviews yet (orchestrator.ProcessResult's own
// doc comment). All a call leaves behind is its name in the run's
// tool-call census.
//
// So it says so, rather than promising a draft review a human will open
// (docs/agent-ergonomics.md, finding 2). The alternative that finding
// offers -- registering it only for a dispatch that is actually a
// review, behind a `grain mcpserver` flag the way -pr-repo/-pr-branch
// scope the CI tools -- is the better shape and is not this change:
// there is no review dispatch to set that flag, so the only thing it
// could do today is gate the tool off every run, and a run that is going
// to write review feedback anyway is better served by being told where
// it lands and what to use instead. The flag is worth adding the day
// something can set it.
func addReviewCommentTool(sink *MockSink) Tool {
	return Tool{
		Name: "add_review_comment",
		Description: "Record one piece of code-review feedback. Give both `path` " +
			"and `line` to attach the comment to a specific line of a " +
			"specific file, as shown in the diff against the base branch; " +
			"omit both for a general remark that isn't tied to one line. " +
			"Call this once per point of feedback. Be aware of where the " +
			"feedback goes: grain has no review dispatch to attach a draft " +
			"review to yet, so nothing relays these anywhere -- a call is " +
			"recorded in this run's own tool-call record and reaches no " +
			"human, no pull request and no task. If the feedback is meant " +
			"to be read, put it in a comment_on_issue call instead, which " +
			"is relayed into this task's conversation.",
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

			return Result{Text: fmt.Sprintf("Recorded (%d review comment(s) so far this run). Nothing "+
				"relays these: they land in this run's own record and nowhere else -- no draft "+
				"review, no pull request, no comment on the task. Use comment_on_issue for "+
				"feedback a human needs to read.", n)}
		},
	}
}
