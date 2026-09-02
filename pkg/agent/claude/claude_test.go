package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
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
		t.Fatal("expected an error for a missing SandboxRoot and KonturVM")
	}
}

func TestRunFailsWithKonturVMButNoKonturSSHConfig(t *testing.T) {
	f := newFramework(&fakeRunner{}, "mcpserver-path")
	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", KonturVM: "g-1-1"}); err == nil {
		t.Fatal("expected an error for a KonturVM run with no WithKonturSSH config")
	}
}

func TestRunWritesMCPConfigPointingAtTheKonturVM(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "/path/to/grain", WithKonturSSH("root", "/images/key", "/workspace"))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", KonturVM: "g-1-1"}); err != nil {
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
	want := []string{"mcpserver", "-kontur-vm", "g-1-1", "-ssh-user", "root", "-workspace", "/workspace", "-exec-key", "/images/key"}
	if len(server.Args) != len(want) {
		t.Fatalf("args = %v, want %v", server.Args, want)
	}
	for i, w := range want {
		if server.Args[i] != w {
			t.Errorf("args[%d] = %q, want %q (full args %v)", i, server.Args[i], w, server.Args)
		}
	}
}

func TestRunPrefersSandboxRootOverKonturVMWhenBothAreSet(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	root := t.TempDir()
	f := newFramework(fake, "mcpserver-path", WithKonturSSH("root", "/images/key", "/workspace"))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: root, KonturVM: "g-1-1"}); err != nil {
		t.Fatal(err)
	}
	if v := argValue(fake.gotArgs, "--mcp-config"); v == "" {
		t.Fatal("--mcp-config not passed")
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

func TestRunPassesTheDefaultModelWhenNoneIsGiven(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "mcpserver-path")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if got := argValue(fake.gotArgs, "--model"); got != DefaultModel {
		t.Errorf("--model = %q, want %q (DefaultModel)", got, DefaultModel)
	}
}

func TestWithModelOverridesTheDefault(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "mcpserver-path", WithModel("claude-opus-5"))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if got := argValue(fake.gotArgs, "--model"); got != "claude-opus-5" {
		t.Errorf("--model = %q, want %q", got, "claude-opus-5")
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

// TestMockToolCallsNeverReachAnyNetwork mirrors agent/antigravity's own test of
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

// The normal case now: no exec key configured, because `kontur run`
// generates one per guest and `kontur exec`'s own default path already
// holds it. Passing "-exec-key" with an empty value instead of omitting
// it would have mcpserver hand KONTUR_EXEC_KEY="" to kontur exec, which
// reads an empty path as a key it cannot open rather than as "unset".
func TestRunOmitsExecKeyWhenUnset(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "ok"})}
	f := newFramework(fake, "/path/to/grain", WithKonturSSH("root", "", "/workspace"))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", KonturVM: "g-1-1"}); err != nil {
		t.Fatalf("a kontur run with no exec key should work, got: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(fake.gotMCPConfig, &cfg); err != nil {
		t.Fatalf("mcp-config was not valid JSON: %v (%s)", err, fake.gotMCPConfig)
	}
	for _, arg := range cfg.MCPServers["grain-sandbox"].Args {
		if arg == "-exec-key" {
			t.Errorf("args = %v, want no -exec-key when none is configured", cfg.MCPServers["grain-sandbox"].Args)
		}
	}
}

// maxTurnsStdout is the shape a real `claude -p --output-format
// stream-json` capture has when its --max-turns budget runs out: a run
// that did real work -- streamed text, called a tool, got a result back
// -- and only then hit the cap. Captured against claude 2.1.258, which
// exits 1 with *nothing on stderr* and reports the failure only here,
// as the terminal result event.
func maxTurnsStdout(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "pushing the branch"}},
			},
		}),
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_use", "id": "call-1", "name": "run_command",
					"input": map[string]any{"command": "git push"},
				}},
			},
		}),
		streamJSONLine(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": "call-1", "content": "branch pushed",
				}},
			},
		}),
		// "result": null, exactly as claude sends it for this subtype --
		// there is no final answer to report, which is the whole point.
		streamJSONLine(t, map[string]any{
			"type": "result", "subtype": "error_max_turns", "is_error": true, "result": nil,
		}),
	}, "\n")
}

// A claude that exits non-zero has still already done whatever it did
// before it failed, and Run owes its caller that record: returning nil
// here is what stranded a pushed branch, and -- because
// orchestrator.RunDispatch only records a transcript for a non-nil
// Result, then removes the live mirror the UI had been rendering -- what
// made a failed run's transcript vanish from the UI the instant it
// failed.
func TestRunReturnsTheWorkDoneBeforeANonZeroExit(t *testing.T) {
	fake := &fakeRunner{stdout: maxTurnsStdout(t), err: errors.New("exit status 1 (stderr: )")}
	f := newFramework(fake, "mcpserver-path")

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error for a claude that exited non-zero")
	}
	if result == nil {
		t.Fatal("Result = nil; a failed run's completed work must still come back")
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "run_command" {
		t.Errorf("ToolCalls = %+v, want the run_command the run made before it failed", result.ToolCalls)
	}
	if !strings.Contains(result.Transcript, "pushing the branch") {
		t.Errorf("Transcript = %q, want the text streamed before the failure", result.Transcript)
	}
}

// "exit status 1 (stderr: )" is what claude gives a caller that reads
// only the exit status, and it says nothing at all. The stream's own
// terminal result event knows why, and that is what an operator reading
// the failed run in the UI has to be shown.
func TestRunNamesTheTurnCapRatherThanTheExitStatus(t *testing.T) {
	fake := &fakeRunner{stdout: maxTurnsStdout(t), err: errors.New("exit status 1 (stderr: )")}
	f := newFramework(fake, "mcpserver-path", WithMaxTurns(7))

	_, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got, want := err.Error(), "claude: exceeded max turns (7) without a final answer"; got != want {
		t.Errorf("err = %q, want %q", got, want)
	}
}

// A subprocess that died before saying anything -- no binary, a signal,
// a cancelled context -- has no result event to explain itself, so the
// exit error is all there is and must not be swallowed.
func TestRunFallsBackToTheExitErrorWhenTheStreamSaysNothing(t *testing.T) {
	fake := &fakeRunner{err: errors.New("exec: \"claude\": executable file not found in $PATH")}
	f := newFramework(fake, "mcpserver-path")

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("err = %q, want the underlying exec error", err)
	}
	// nil, not an empty Result: agent.Framework's contract is that a nil
	// Result with an error means the run never started, and
	// orchestrator.RunDispatch reads it that way.
	if result != nil {
		t.Errorf("Result = %+v, want nil for a run that never started", result)
	}
}

// No cap by default: v1 passed claude no --max-turns at all, and any
// number this package picks is a guess at how much work a task deserves
// that fails a working run when it guesses low. Config.MaxRunRuntime is
// what actually bounds a runaway run.
func TestRunPassesNoMaxTurnsByDefault(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "done"})}
	f := newFramework(fake, "mcpserver-path")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	for _, a := range fake.gotArgs {
		if a == "--max-turns" {
			t.Fatalf("args = %v, want no --max-turns at all when none is configured", fake.gotArgs)
		}
	}
}

// A deployment that does want a ceiling still gets one, passed through
// unchanged -- "unlimited" is the default, not the only option.
func TestRunPassesAnExplicitMaxTurnsThrough(t *testing.T) {
	fake := &fakeRunner{stdout: streamJSONLine(t, map[string]any{"type": "result", "result": "done"})}
	f := newFramework(fake, "mcpserver-path")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir(), MaxTurns: 12}); err != nil {
		t.Fatal(err)
	}
	if got := argValue(fake.gotArgs, "--max-turns"); got != "12" {
		t.Errorf("--max-turns = %q, want 12", got)
	}
}

// With no cap configured there is no number to tell an operator to raise,
// so the error must not invent one -- it would send them looking for a
// setting that is already unlimited.
func TestRunNamesNoTurnBudgetWhenNoneWasConfigured(t *testing.T) {
	fake := &fakeRunner{stdout: maxTurnsStdout(t), err: errors.New("exit status 1 (stderr: )")}
	f := newFramework(fake, "mcpserver-path")

	_, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", SandboxRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "(0)") {
		t.Errorf("err = %q, want no fabricated turn budget in the message", err)
	}
	if !strings.Contains(err.Error(), "turn limit") {
		t.Errorf("err = %q, want it to still say the run was stopped at a turn limit", err)
	}
}
