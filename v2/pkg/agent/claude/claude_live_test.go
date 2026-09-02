package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
)

// TestLiveRunEndToEnd exercises the real claude binary and a real grain
// binary's "mcpserver" subcommand, gating on a credential and skipping
// otherwise -- this
// runs in CI (where CLAUDE_CODE_OAUTH_TOKEN is unset, and no claude binary
// is installed) without failing, but can be run for real wherever both are
// available:
//
//	CLAUDE_CODE_OAUTH_TOKEN=... go test ./agent/claude/... -run TestLiveRunEndToEnd -v
func TestLiveRunEndToEnd(t *testing.T) {
	token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if token == "" {
		t.Skip("CLAUDE_CODE_OAUTH_TOKEN not set; skipping live claude integration test")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude binary not found on PATH; skipping live claude integration test")
	}

	grainBinaryPath := buildGrainBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	f := New(claudePath, grainBinaryPath, WithOAuthToken(token))

	root := t.TempDir()
	result, err := f.Run(ctx, agent.RunConfig{
		Prompt: "Use your write_file tool to create a file named ok.txt in your sandbox " +
			"workspace containing exactly the text PONG (no extra whitespace or " +
			"punctuation). Then use read_file to confirm it, and reply with a short " +
			"confirmation once you have done both.",
		SandboxRoot: root,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "ok.txt"))
	if err != nil {
		t.Fatalf("expected ok.txt to have been written inside the sandbox root: %v", err)
	}
	if strings.TrimSpace(string(data)) != "PONG" {
		t.Errorf("ok.txt content = %q, want PONG", string(data))
	}

	var sawWrite bool
	for _, call := range result.ToolCalls {
		if call.Name == "write_file" {
			sawWrite = true
		}
		t.Logf("tool call: %s(%v) -> error=%v text=%q", call.Name, call.Arguments, call.IsError, call.Text)
	}
	if !sawWrite {
		t.Errorf("expected the model to have called write_file at least once; got calls: %+v", result.ToolCalls)
	}
	if result.FinalText == "" {
		t.Errorf("expected a non-empty final answer")
	}
}

// buildGrainBinary compiles v2/cmd/grain into t.TempDir() so the live
// test above can point a real claude --mcp-config at its "mcpserver"
// subcommand, the same binary a real deployment would build once and
// reuse across every dispatch (as the daemon, the UI, or the CLI too).
func buildGrainBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "grain")
	cmd := exec.Command("go", "build", "-o", out, "github.com/bwsalmon/grain/v2/cmd/grain")
	cmd.Dir = repoRoot(t)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building cmd/grain: %v\n%s", err, output)
	}
	return out
}

// repoRoot walks up from the current package directory to v2/, the module
// root -- so `go build`'s package path resolves the same way regardless of
// which directory `go test` happened to be invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find v2/go.mod above the current test directory")
		}
		dir = parent
	}
}
