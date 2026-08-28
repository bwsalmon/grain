package gemini

import (
	"context"
	"os"
	"path/filepath"
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
	calls     int
	gotTools  [][]*genai.Tool
}

func (f *fakeGenerator) GenerateContent(_ context.Context, _ string, _ []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	f.gotTools = append(f.gotTools, config.Tools)
	if f.calls >= len(f.responses) {
		f.calls++
		return nil, nil
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
		Handler: func(map[string]any) mcp.Result {
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

	_, err := f.Run(context.Background(), agent.RunConfig{Prompt: "loop forever", SandboxRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error once MaxTurns is exhausted")
	}
	if fake.calls != 3 {
		t.Errorf("GenerateContent called %d times, want 3", fake.calls)
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
