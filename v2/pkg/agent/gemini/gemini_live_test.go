package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
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

// TestLiveRunFoldsALiveAddendumFromARealAgent is bwsalmon/agents#523's own
// live confirmation, the same discipline live_issue514_test.go already
// gives that issue: a scripted agent.Framework (gemini_test.go's own
// TestRunFoldsAnAddendumIntoTheConversationBetweenTurns) already proves
// RunConfig.Addenda's plumbing in isolation; this drives the same
// "instructions changed mid-run" scenario through a real model deciding
// its own tool calls, standing in for RunDispatch's own addendaPoller
// (which hands a live run exactly this shape of func once a human posts
// a comment on its task) without needing a whole store and dispatch to
// set one up.
//
//	GEMINI_API_KEY=... go test ./pkg/agent/gemini/... -run TestLiveRunFoldsALiveAddendumFromARealAgent -v
func TestLiveRunFoldsALiveAddendumFromARealAgent(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live Gemini integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	f, err := New(ctx, apiKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	root := t.TempDir()
	// The first poll (before the model has done anything) has nothing to
	// say; the second -- reached only because Run polls again ahead of
	// every turn, not just the first -- delivers the correction, the
	// same shape addendumText (run.go) hands a live run once a human
	// posts a comment on its task mid-run.
	var polls int
	addenda := func(context.Context) ([]string, error) {
		polls++
		if polls == 2 {
			return []string{"a human just added this to the task while you're already working " +
				"on it -- read it and factor it in:\n\nActually, write PANG instead of PONG. " +
				"Overwrite out.txt if you already wrote something else to it."}, nil
		}
		return nil, nil
	}

	result, err := f.Run(ctx, agent.RunConfig{
		Prompt: "Use your write_file tool to write the text PONG (no extra whitespace or " +
			"punctuation) into a file named out.txt in your sandbox workspace. Once you have, " +
			"do not give your final answer yet -- wait a turn in case fresh instructions arrive " +
			"in this conversation, and if any do, follow them exactly (overwriting out.txt if " +
			"asked to) before you reply with a short final confirmation.",
		SandboxRoot: root,
		Addenda:     addenda,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if polls < 2 {
		t.Fatalf("Addenda polled %d time(s), want at least 2: this run ended before the "+
			"addendum could ever have reached it", polls)
	}

	data, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil {
		t.Fatalf("expected out.txt to have been written inside the sandbox root: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "PANG" {
		t.Fatalf("out.txt content = %q, want PANG: the live addendum should have overridden "+
			"the original PONG instruction", got)
	}
	t.Logf("final answer: %s", result.FinalText)
}
