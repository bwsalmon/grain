package antigravity

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
)

// agy reports a quota refusal as a failed terminal status. Run must
// hand that back as an agent.UsageLimitError rather than as its own
// generic "run ended in status FAILURE", which is what
// orchestrator.RunDispatch reads to pause the deployment instead of
// retrying into the same refusal.
func TestRunReportsAQuotaRefusalAsAUsageLimit(t *testing.T) {
	r := &recordingRunner{stdout: stream(
		initLine,
		`{"event":"result","result":{"status":"FAILURE","error":"429 RESOURCE_EXHAUSTED: You exceeded your current quota. retryDelay: 56s"}}`,
	)}
	f := newFramework(r, "/usr/local/bin/grain")

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", SandboxRoot: t.TempDir()})
	limit, ok := agent.UsageLimit(err)
	if !ok {
		t.Fatalf("Run err = %v, want an agent.UsageLimitError", err)
	}
	if limit.Framework != "antigravity" {
		t.Errorf("Framework = %q, want antigravity", limit.Framework)
	}
	if limit.RetryAfter != 56*time.Second {
		t.Errorf("RetryAfter = %s, want 56s -- the delay the API named", limit.RetryAfter)
	}
	if !strings.Contains(limit.Message, "RESOURCE_EXHAUSTED") {
		t.Errorf("Message = %q, want the provider's own words", limit.Message)
	}
	// agent.Framework's contract: a run that pushed a branch before it
	// ran out of quota still comes back with what it did.
	if result == nil {
		t.Error("Run returned no Result at all, want the partial one alongside the limit")
	}
}

// An ordinary failure must stay an ordinary failure: pausing the whole
// deployment is the one response that is much worse than retrying when
// it is the wrong one.
func TestRunKeepsAnOrdinaryFailureOrdinary(t *testing.T) {
	r := &recordingRunner{stdout: stream(
		initLine,
		`{"event":"result","result":{"status":"FAILURE","error":"the model returned no candidates"}}`,
	)}
	f := newFramework(r, "/usr/local/bin/grain")

	_, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", SandboxRoot: t.TempDir()})
	if err == nil {
		t.Fatal("Run err = nil, want the failure")
	}
	if _, ok := agent.UsageLimit(err); ok {
		t.Errorf("Run err = %v, want it read as an ordinary failure", err)
	}
}

// The tool output of a successful run is not evidence about quota: agy
// reports a refusal in its terminal event, and a run that greps for the
// phrase must not put the deployment to sleep.
func TestRunIgnoresTheQuotaPhraseInASuccessfulRunsWork(t *testing.T) {
	r := &recordingRunner{stdout: stream(
		initLine,
		toolActive(0, "run_command", `{"command":"grep -r RESOURCE_EXHAUSTED ."}`),
		toolDone(0, "run_command", "usagelimit.go: resource_exhausted"),
		`{"event":"result","result":{"status":"SUCCESS","response":"found it"}}`,
	)}
	f := newFramework(r, "/usr/local/bin/grain")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "grep", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatalf("Run err = %v, want the run to pass", err)
	}
}

func TestRetryDelayReadsTheAPIsOwnSpellings(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want time.Duration
	}{
		{"json", `{"retryDelay":"56s"}`, 56 * time.Second},
		{"snake case", `retry_delay: "30s"`, 30 * time.Second},
		{"fractional", `"retryDelay":"1.5s"`, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := retryDelay(tc.text)
			if !ok {
				t.Fatalf("retryDelay(%q) found nothing", tc.text)
			}
			if got != tc.want {
				t.Errorf("retryDelay(%q) = %s, want %s", tc.text, got, tc.want)
			}
		})
	}
	if _, ok := retryDelay("no delay here"); ok {
		t.Error("retryDelay found a delay in text that names none")
	}
}
