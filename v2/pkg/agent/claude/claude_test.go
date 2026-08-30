package claude

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/agent"
)

// fakeRunner scripts a single canned stream-json transcript and records
// exactly how it was invoked, so Run's arg-building, mcp-config plumbing,
// and transcript parsing can all be exercised without a real claude binary
// or a live Claude Code credential.
type fakeRunner struct {
	stdout       string
	err          error
	gotArgs      []string
	gotStdin     string
	gotEnv       []string
	gotMCPConfig []byte
}

func (f *fakeRunner) Run(_ context.Context, args []string, stdin string, env []string, tee io.Writer) (string, error) {
	f.gotArgs = args
	f.gotStdin = stdin
	f.gotEnv = env
	if path := argValue(args, "--mcp-config"); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			f.gotMCPConfig = data
		}
	}
	if tee != nil {
		io.WriteString(tee, f.stdout)
	}
	return f.stdout, f.err
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func streamJSONLine(t *testing.T, v map[string]any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunReturnsFinalTextWhenNoToolCallsAreMade(t *testing.T) {
	fake := &fakeRunner{stdout: strings.Join([]string{
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "thinking"}},
			},
		}),
		streamJSONLine(t, map[string]any{"type": "result", "result": "all done"}),
	}, "\n")}
	f := newFramework(fake, "mcpserver-path")

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
}

func TestRunPairsToolUseWithItsToolResult(t *testing.T) {
	fake := &fakeRunner{stdout: strings.Join([]string{
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_use", "id": "call-1", "name": "write_file",
					"input": map[string]any{"file_path": "out.txt", "content": "PONG"},
				}},
			},
		}),
		streamJSONLine(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": "call-1", "content": "Wrote out.txt",
				}},
			},
		}),
		streamJSONLine(t, map[string]any{"type": "result", "result": "wrote it"}),
	}, "\n")}
	f := newFramework(fake, "mcpserver-path")

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "write PONG to out.txt", SandboxRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText != "wrote it" {
		t.Errorf("FinalText = %q", result.FinalText)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v", result.ToolCalls)
	}
	call := result.ToolCalls[0]
	if call.Name != "write_file" || call.Text != "Wrote out.txt" || call.IsError {
		t.Errorf("ToolCalls[0] = %+v", call)
	}
	if call.Arguments["file_path"] != "out.txt" {
		t.Errorf("ToolCalls[0].Arguments = %+v", call.Arguments)
	}
}

func TestRunReportsToolResultContentGivenAsBlocksNotAPlainString(t *testing.T) {
	fake := &fakeRunner{stdout: strings.Join([]string{
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_use", "id": "call-1", "name": "run_command",
					"input": map[string]any{"command": "false"},
				}},
			},
		}),
		streamJSONLine(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": "call-1", "is_error": true,
					"content": []map[string]any{{"type": "text", "text": "exit=1"}},
				}},
			},
		}),
		streamJSONLine(t, map[string]any{"type": "result", "result": "done"}),
	}, "\n")}
	f := newFramework(fake, "mcpserver-path")

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Text != "exit=1" || !result.ToolCalls[0].IsError {
		t.Fatalf("ToolCalls = %+v", result.ToolCalls)
	}
}

func TestRunBuildsAHumanReadableTranscript(t *testing.T) {
	fake := &fakeRunner{stdout: strings.Join([]string{
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{"type": "thinking", "thinking": "let me check the file first"}},
			},
		}),
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_use", "id": "call-1", "name": "read_file",
					"input": map[string]any{"file_path": "out.txt"},
				}},
			},
		}),
		streamJSONLine(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": "call-1", "content": "PONG",
				}},
			},
		}),
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "found it"}},
			},
		}),
		streamJSONLine(t, map[string]any{"type": "result", "result": "done"}),
	}, "\n")}
	f := newFramework(fake, "mcpserver-path")

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"let me check the file first", "read_file", "out.txt", "PONG", "found it"} {
		if !strings.Contains(result.Transcript, want) {
			t.Errorf("Transcript = %q, want it to contain %q", result.Transcript, want)
		}
	}
	// Chronological: the thinking block precedes the tool call it explains,
	// which precedes the result it read, which precedes the final text
	// that used it.
	thinkAt := strings.Index(result.Transcript, "let me check the file first")
	toolAt := strings.Index(result.Transcript, "read_file")
	resultAt := strings.Index(result.Transcript, "PONG")
	textAt := strings.Index(result.Transcript, "found it")
	if !(thinkAt < toolAt && toolAt < resultAt && resultAt < textAt) {
		t.Errorf("Transcript not in chronological order: %q", result.Transcript)
	}
}

func TestRunReportsAnErrorResultEvent(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{
		"type": "result", "subtype": "error_max_turns", "is_error": true, "result": "",
	})}
	f := newFramework(fake, "mcpserver-path")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err == nil {
		t.Fatal("expected an error for an error_max_turns result event")
	}
}

func TestRunFailsWithoutSandboxRoot(t *testing.T) {
	f := newFramework(&fakeRunner{}, "mcpserver-path")
	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x"}); err == nil {
		t.Fatal("expected an error for a missing SandboxRoot")
	}
}

func TestRunFailsWithoutMCPServerPath(t *testing.T) {
	f := newFramework(&fakeRunner{}, "")
	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err == nil {
		t.Fatal("expected an error for a missing grainBinaryPath")
	}
}

func TestRunPassesThePromptOverStdinNotArgv(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "mcpserver-path")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "untrusted issue body", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if fake.gotStdin != "untrusted issue body" {
		t.Errorf("stdin = %q, want the prompt", fake.gotStdin)
	}
	for _, a := range fake.gotArgs {
		if strings.Contains(a, "untrusted issue body") {
			t.Errorf("prompt leaked into argv: %v", fake.gotArgs)
		}
	}
}

func TestRunEmptiesTheNativeToolRoster(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "mcpserver-path")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if v := argValue(fake.gotArgs, "--tools"); v != "" {
		t.Errorf("--tools = %q, want empty", v)
	}
	found := false
	for _, a := range fake.gotArgs {
		if a == "--strict-mcp-config" {
			found = true
		}
	}
	if !found {
		t.Errorf("--strict-mcp-config missing from args: %v", fake.gotArgs)
	}
}

func TestRunCleansUpTheMCPConfigFile(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "mcpserver-path")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	configPath := argValue(fake.gotArgs, "--mcp-config")
	if configPath == "" {
		t.Fatal("--mcp-config not passed")
	}
	if _, err := os.Stat(configPath); err == nil {
		t.Errorf("mcp-config file %s was not cleaned up after Run", configPath)
	}
}

func TestRunWritesMCPConfigPointingAtTheServerBinaryAndSandboxRoot(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	root := t.TempDir()
	f := newFramework(fake, "/path/to/grain")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: root}); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(fake.gotMCPConfig, &cfg); err != nil {
		t.Fatalf("mcp-config was not valid JSON: %v (%s)", err, fake.gotMCPConfig)
	}
	server, ok := cfg.MCPServers["grain-sandbox"]
	if !ok {
		t.Fatalf("mcp-config missing grain-sandbox server: %+v", cfg)
	}
	if server.Command != "/path/to/grain" {
		t.Errorf("command = %q", server.Command)
	}
	// "mcpserver" comes first: claude forks this exact command, and the
	// grain binary itself dispatches on that leading argument (main.go)
	// to run as an MCP server rather than the task CLI.
	if len(server.Args) != 3 || server.Args[0] != "mcpserver" || server.Args[1] != "-sandbox-root" || server.Args[2] != root {
		t.Errorf("args = %v", server.Args)
	}
}

func TestAllowedToolsNamesAllEightGrainSandboxTools(t *testing.T) {
	names := allowedTools()
	if len(names) != 8 {
		t.Fatalf("allowedTools() = %v, want 8 entries", names)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "mcp__grain-sandbox__") {
			t.Errorf("tool name %q missing mcp__grain-sandbox__ prefix", n)
		}
	}
}

func TestWithMaxTurnsOverridesTheDefault(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "mcpserver-path", WithMaxTurns(5))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if got := argValue(fake.gotArgs, "--max-turns"); got != "5" {
		t.Errorf("--max-turns = %q, want 5", got)
	}
}

func TestRunConfigMaxTurnsOverridesTheFrameworkDefault(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "mcpserver-path", WithMaxTurns(5))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir(), MaxTurns: 2}); err != nil {
		t.Fatal(err)
	}
	if got := argValue(fake.gotArgs, "--max-turns"); got != "2" {
		t.Errorf("--max-turns = %q, want 2 (RunConfig should override WithMaxTurns)", got)
	}
}

func TestWithOAuthTokenSetsTheEnvironmentNotArgv(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "mcpserver-path", WithOAuthToken("secret-token"))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range fake.gotEnv {
		if e == "CLAUDE_CODE_OAUTH_TOKEN=secret-token" {
			found = true
		}
	}
	if !found {
		t.Errorf("gotEnv = %v, want CLAUDE_CODE_OAUTH_TOKEN=secret-token", fake.gotEnv)
	}
	for _, a := range fake.gotArgs {
		if strings.Contains(a, "secret-token") {
			t.Errorf("token leaked into argv: %v", fake.gotArgs)
		}
	}
}

func TestRunMirrorsStdoutToTranscriptPath(t *testing.T) {
	stdout := strings.Join([]string{
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "on it"}},
			},
		}),
		streamJSONLine(t, map[string]any{"type": "result", "result": "done"}),
	}, "\n")
	fake := &fakeRunner{stdout: stdout}
	f := newFramework(fake, "mcpserver-path")

	transcriptPath := filepath.Join(t.TempDir(), "r1")
	if _, err := f.Run(context.Background(), agent.RunConfig{
		Prompt: "x", SandboxRoot: t.TempDir(), TranscriptPath: transcriptPath,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("expected TranscriptPath to have been written: %v", err)
	}
	if string(got) != stdout {
		t.Errorf("transcript file = %q, want the raw stdout %q", got, stdout)
	}
	if partial := PartialTranscript(string(got)); !strings.Contains(partial, "on it") {
		t.Errorf("PartialTranscript(transcript file) = %q, want it to contain %q", partial, "on it")
	}
}

func TestRunWithNoTranscriptPathWritesNoFile(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "mcpserver-path")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	// Nothing to assert on directly beyond "this didn't panic or error" --
	// TranscriptPath's own zero value ("") is the only thing distinguishing
	// this from the test above, and Run must treat it as "no caller wants
	// this" rather than trying to open a file at an empty path.
}

// TestMockToolCallsNeverReachAnyNetwork mirrors agent/gemini's own test of
// the same name: proving the escape-hatch tools this transcript parser
// reports on are only ever mocked ones as far as this package is
// concerned -- it never talks to GitHub itself, only parses what
// cmd/mcpserver's own MockSink already recorded.
func TestMockToolCallsNeverReachAnyNetwork(t *testing.T) {
	fake := &fakeRunner{stdout: strings.Join([]string{
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_use", "id": "call-1", "name": "comment_on_issue",
					"input": map[string]any{"comment": "done"},
				}},
			},
		}),
		streamJSONLine(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": "call-1",
					"content": "Recorded (mocked -- no GitHub comment was posted).",
				}},
			},
		}),
		streamJSONLine(t, map[string]any{"type": "result", "result": "ok"}),
	}, "\n")}
	f := newFramework(fake, "mcpserver-path")

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "leave a comment", SandboxRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].IsError {
		t.Fatalf("ToolCalls = %+v", result.ToolCalls)
	}
}
