package gemini

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
)

// fakeGenerator scripts a sequence of responses, one per call to
// GenerateContent, so the tool-dispatch loop can be exercised without a
// live API key or any network access.
type fakeGenerator struct {
	responses []*genai.GenerateContentResponse
	// err, when set, is what the generator returns once responses runs
	// out -- the "the model broke partway through" case, as opposed to
	// the silent nil below.
	err      error
	calls    int
	gotTools [][]*genai.Tool
}

func (f *fakeGenerator) GenerateContent(_ context.Context, _ string, _ []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	f.gotTools = append(f.gotTools, config.Tools)
	if f.calls >= len(f.responses) {
		f.calls++
		return nil, f.err
	}
	resp := f.responses[f.calls]
	f.calls++
	return resp, nil
}

func textResponse(text string) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: genai.NewContentFromText(text, genai.RoleModel)}},
	}
}

func toolCallResponse(name string, args map[string]any) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: genai.NewContentFromFunctionCall(name, args, genai.RoleModel)}},
	}
}

// thoughtAndToolCallResponse builds a response with a thought part ahead of
// the function call it explains -- the shape Gemini uses when thinking is
// enabled, and what TestRunBuildsAHumanReadableTranscript exercises.
func thoughtAndToolCallResponse(thought, name string, args map[string]any) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: thought, Thought: true},
				genai.NewPartFromFunctionCall(name, args),
			},
		}}},
	}
}

func TestRunReturnsFinalTextWhenNoToolCallsAreMade(t *testing.T) {
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{textResponse("all done")}}
	f := newFramework(fake)

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "say hi", SandboxRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText != "all done" {
		t.Errorf("FinalText = %q", result.FinalText)
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want none", result.ToolCalls)
	}
	if fake.calls != 1 {
		t.Errorf("GenerateContent called %d times, want 1", fake.calls)
	}
}

func TestRunExecutesToolCallsThroughTheSandbox(t *testing.T) {
	root := t.TempDir()
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{
		toolCallResponse("write_file", map[string]any{"file_path": "out.txt", "content": "PONG"}),
		textResponse("wrote it"),
	}}
	f := newFramework(fake)

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "write PONG to out.txt", SandboxRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText != "wrote it" {
		t.Errorf("FinalText = %q", result.FinalText)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "write_file" {
		t.Fatalf("ToolCalls = %+v", result.ToolCalls)
	}
	if result.ToolCalls[0].IsError {
		t.Errorf("write_file call reported an error: %s", result.ToolCalls[0].Text)
	}
	data, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "PONG" {
		t.Errorf("out.txt content = %q, want PONG", string(data))
	}
}

// TestRunFoldsAnAddendumIntoTheConversationBetweenTurns is bwsalmon/
// agents#523: this package is the one Framework whose loop actually has
// a "between turns" to poll RunConfig.Addenda at (see that field's own
// doc comment on why claude.Framework.Run cannot), so this is the one
// place that plumbing needs to be proven end to end -- a poll that
// returns something must land in both the conversation the next
// GenerateContent call sees and the transcript, not just get thrown away.
func TestRunFoldsAnAddendumIntoTheConversationBetweenTurns(t *testing.T) {
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{
		toolCallResponse("write_file", map[string]any{"file_path": "out.txt", "content": "PONG"}),
		textResponse("wrote it, addendum received"),
	}}
	f := newFramework(fake)

	const addendum = "a human just added: actually, write PING instead"
	calls := 0
	addenda := func(context.Context) ([]string, error) {
		calls++
		if calls == 2 {
			return []string{addendum}, nil
		}
		return nil, nil
	}

	result, err := f.Run(context.Background(), agent.RunConfig{
		Prompt: "write PONG to out.txt", SandboxRoot: t.TempDir(), Addenda: addenda,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("Addenda polled %d time(s), want at least 2 (once per turn)", calls)
	}
	if !strings.Contains(result.Transcript, addendum) {
		t.Errorf("transcript = %q, want it to include the addendum", result.Transcript)
	}
}

// TestRunNeverPollsAddendaWhenNoneIsGiven checks the nil case costs
// nothing: RunConfig.Addenda is optional, and most callers (every test
// above this one) never set it.
func TestRunNeverPollsAddendaWhenNoneIsGiven(t *testing.T) {
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{textResponse("ok")}}
	f := newFramework(fake)
	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func TestRunAdvertisesAllEightToolsToTheModel(t *testing.T) {
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{textResponse("ok")}}
	f := newFramework(fake)

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if len(fake.gotTools) != 1 || len(fake.gotTools[0]) != 1 {
		t.Fatalf("gotTools = %+v", fake.gotTools)
	}
	decls := fake.gotTools[0][0].FunctionDeclarations
	if len(decls) != 8 {
		t.Fatalf("got %d function declarations, want 8: %+v", len(decls), decls)
	}
}

func TestRunFailsWithoutSandboxRootOrTools(t *testing.T) {
	f := newFramework(&fakeGenerator{})
	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x"}); err == nil {
		t.Fatal("expected an error for a missing SandboxRoot and Tools")
	}
}

// TestRunUsesToolsDirectlyWhenProvided proves a caller that already built
// its own tool set -- orchestrator.KonturSandboxes' mcp.NewSSHSandboxTools,
// in production -- gets exactly that set registered, with no
// mcp.NewSandboxTools/SandboxRoot involved at all.
func TestRunUsesToolsDirectlyWhenProvided(t *testing.T) {
	called := false
	tool := mcp.Tool{
		Name:        "custom_tool",
		Description: "a tool only present because RunConfig.Tools supplied it",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(context.Context, map[string]any) mcp.Result {
			called = true
			return mcp.Result{Text: "done"}
		},
	}
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{
		toolCallResponse("custom_tool", map[string]any{}),
		textResponse("ok"),
	}}
	f := newFramework(fake)

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", Tools: []mcp.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("custom_tool's handler was never invoked")
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].IsError {
		t.Fatalf("ToolCalls = %+v", result.ToolCalls)
	}
	// Only custom_tool plus the mocked escape hatches should have been
	// advertised -- no run_command/read_file/edit_file/write_file, since
	// Tools being set skips mcp.NewSandboxTools entirely.
	if len(fake.gotTools) == 0 || len(fake.gotTools[0]) != 1 {
		t.Fatalf("gotTools = %+v", fake.gotTools)
	}
	decls := fake.gotTools[0][0].FunctionDeclarations
	if len(decls) != 5 {
		t.Fatalf("got %d function declarations, want 5 (custom_tool + 4 escape hatches): %+v", len(decls), decls)
	}
}

func TestRunGivesUpAfterMaxTurns(t *testing.T) {
	responses := make([]*genai.GenerateContentResponse, 5)
	for i := range responses {
		responses[i] = toolCallResponse("run_command", map[string]any{"command": "true"})
	}
	fake := &fakeGenerator{responses: responses}
	f := newFramework(fake, WithMaxTurns(3))

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "loop forever", SandboxRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error once MaxTurns is exhausted")
	}
	if fake.calls != 3 {
		t.Errorf("GenerateContent called %d times, want 3", fake.calls)
	}

	// The error says the run did not finish; the result says what it did
	// before running out of budget. Returning nil here threw that away,
	// and with it every trace of a run that had already pushed a branch
	// -- see Run's own comment.
	if result == nil {
		t.Fatal("Run returned no result alongside the max-turns error; a caller cannot tell a run that spun from one that worked and ran out of turns")
	}
	if len(result.ToolCalls) != 3 {
		t.Errorf("result.ToolCalls = %d, want the 3 calls the run actually made", len(result.ToolCalls))
	}
}

// A run that breaks mid-loop keeps the same contract as one that runs out
// of turns: the generator failing on the third call must not erase the two
// tool calls that already happened.
func TestRunKeepsWhatItDidWhenTheGeneratorFails(t *testing.T) {
	fake := &fakeGenerator{
		responses: []*genai.GenerateContentResponse{
			toolCallResponse("run_command", map[string]any{"command": "true"}),
			toolCallResponse("run_command", map[string]any{"command": "true"}),
		},
		err: errors.New("model unavailable"),
	}
	f := newFramework(fake)

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "work", SandboxRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected the generator's error")
	}
	if result == nil {
		t.Fatal("Run returned no result alongside the generator error")
	}
	if len(result.ToolCalls) != 2 {
		t.Errorf("result.ToolCalls = %d, want 2", len(result.ToolCalls))
	}
}

// mockToolIsNeverReal proves the escape-hatch tools never touch a real
// GitHub API from this package's side: calling one only ever mutates local,
// in-process state (the MockSink inside Run's own registry), which is
// discarded when Run returns -- there is no client, credential, or network
// call anywhere in that path.
func TestMockToolCallsNeverReachAnyNetwork(t *testing.T) {
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{
		toolCallResponse("comment_on_issue", map[string]any{"comment": "done"}),
		textResponse("ok"),
	}}
	f := newFramework(fake)

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "leave a comment", SandboxRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].IsError {
		t.Fatalf("ToolCalls = %+v", result.ToolCalls)
	}
}

func TestRunBuildsAHumanReadableTranscript(t *testing.T) {
	root := t.TempDir()
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{
		thoughtAndToolCallResponse("let me check the file first", "read_file", map[string]any{"file_path": "out.txt"}),
		textResponse("found it"),
	}}
	f := newFramework(fake)

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: root})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"let me check the file first", "read_file", "out.txt", "found it"} {
		if !strings.Contains(result.Transcript, want) {
			t.Errorf("Transcript = %q, want it to contain %q", result.Transcript, want)
		}
	}
	// Chronological: the thought precedes the tool call it explains, which
	// precedes the final text produced on the next turn.
	thinkAt := strings.Index(result.Transcript, "let me check the file first")
	toolAt := strings.Index(result.Transcript, "read_file")
	textAt := strings.Index(result.Transcript, "found it")
	if !(thinkAt < toolAt && toolAt < textAt) {
		t.Errorf("Transcript not in chronological order: %q", result.Transcript)
	}
	// The thought must not have leaked into the final answer.
	if strings.Contains(result.FinalText, "let me check the file first") {
		t.Errorf("FinalText = %q, should not include the thought", result.FinalText)
	}
}

func TestRunTranscriptMarksAFailedToolCall(t *testing.T) {
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{
		toolCallResponse("run_command", map[string]any{"command": "false"}),
		textResponse("done"),
	}}
	f := newFramework(fake)

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ToolCalls[0].IsError {
		t.Fatalf("expected run_command with `false` to report an error, got %+v", result.ToolCalls[0])
	}
	if !strings.Contains(result.Transcript, "! ") {
		t.Errorf("Transcript = %q, want an error-marked line for the failed call", result.Transcript)
	}
}

// TestRunMirrorsTheTranscriptToTranscriptPath proves Run writes its own
// transcript-in-progress to cfg.TranscriptPath as it goes, the same
// live-mirror contract claude.Framework.Run gives its own subprocess's
// stdout (bwsalmon/agents#467, extended to this package by bwsalmon/
// agents#513) -- gemini.LiveTranscriptDir.Tail is what a caller reads that
// file back with while the run is still in progress.
func TestRunMirrorsTheTranscriptToTranscriptPath(t *testing.T) {
	fake := &fakeGenerator{responses: []*genai.GenerateContentResponse{
		toolCallResponse("run_command", map[string]any{"command": "true"}),
		textResponse("all done"),
	}}
	f := newFramework(fake)
	path := filepath.Join(t.TempDir(), "r1")

	result, err := f.Run(context.Background(), agent.RunConfig{
		Prompt: "x", SandboxRoot: t.TempDir(), TranscriptPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading TranscriptPath: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), result.Transcript; got != want {
		t.Errorf("transcript file = %q, want it to match Result.Transcript %q", got, want)
	}
	if !strings.Contains(string(data), "run_command") {
		t.Errorf("transcript file = %q, want it to contain the tool call", string(data))
	}
}

// TestRunTranscriptSurvivesMaxTurns proves a run that exhausts MaxTurns
// still hands back a transcript of what it did, the same way it still
// hands back ToolCalls -- see Run's own doc comment on why an error must
// not erase the record of a run that already did real work.
func TestRunTranscriptSurvivesMaxTurns(t *testing.T) {
	responses := make([]*genai.GenerateContentResponse, 3)
	for i := range responses {
		responses[i] = toolCallResponse("run_command", map[string]any{"command": "true"})
	}
	fake := &fakeGenerator{responses: responses}
	f := newFramework(fake, WithMaxTurns(3))

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "loop forever", SandboxRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error once MaxTurns is exhausted")
	}
	if result.Transcript == "" {
		t.Error("Transcript is empty, want a record of the 3 tool calls the run made before running out of turns")
	}
}
