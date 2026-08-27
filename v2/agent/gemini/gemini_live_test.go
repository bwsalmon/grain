package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/agent"
)

// TestLiveRunEndToEnd exercises the real Gemini API, the same way v1's
// tests/test_gcp_live.go equivalents (test_gcp_live.py, test_dispatch_live_gce.py)
// gate a live-credential test on an environment variable and skip
// otherwise, so this runs in CI (where GEMINI_API_KEY is unset) without
// failing, but can be run for real wherever a key is available:
//
//	GEMINI_API_KEY=... go test ./agent/gemini/... -run TestLiveRunEndToEnd -v
func TestLiveRunEndToEnd(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live Gemini integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	f, err := New(ctx, apiKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

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
		// None of the escape-hatch tools should ever have been the only
		// way this prompt could be satisfied -- if the model called one,
		// it stayed local (see TestMockToolCallsNeverReachAnyNetwork);
		// this loop is just recording what happened for the log.
		t.Logf("tool call: %s(%v) -> error=%v text=%q", call.Name, call.Arguments, call.IsError, call.Text)
	}
	if !sawWrite {
		t.Errorf("expected the model to have called write_file at least once; got calls: %+v", result.ToolCalls)
	}
	if result.FinalText == "" {
		t.Errorf("expected a non-empty final answer")
	}
}
