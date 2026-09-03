package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
)

// The shape that matters most: claude reports an OAuth account's own
// limit as the run's terminal result, exits 0, and nothing else about
// the capture says anything is wrong. Run must still hand back a
// usage limit, or the deployment sends the next task at the same wall.
func TestRunReportsAUsageLimitDeliveredAsAFinalAnswer(t *testing.T) {
	fake := &fakeRunner{stdout: strings.Join([]string{
		streamJSONLine(t, map[string]any{
			"type": "result", "result": "Claude AI usage limit reached|1772370000",
		}),
	}, "\n")}
	f := newFramework(fake, "mcpserver-path")

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", SandboxRoot: t.TempDir()})
	limit, ok := agent.UsageLimit(err)
	if !ok {
		t.Fatalf("Run err = %v, want an agent.UsageLimitError", err)
	}
	if limit.Framework != "claude" {
		t.Errorf("Framework = %q, want claude", limit.Framework)
	}
	if want := time.Unix(1772370000, 0).UTC(); !limit.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %s, want %s (the epoch the CLI appended)", limit.ResetAt, want)
	}
	// agent.Framework's contract: whatever the run did still comes back
	// alongside the error.
	if result == nil {
		t.Error("Run returned no Result, want the parsed one alongside the limit")
	}
}

// The failing shape: a non-zero exit with the API's own refusal in the
// stream. runFailure must prefer the limit to its generic rendering of
// the result event.
func TestRunReportsAUsageLimitFromAFailedRun(t *testing.T) {
	fake := &fakeRunner{
		stdout: streamJSONLine(t, map[string]any{
			"type": "result", "subtype": "error_during_execution", "is_error": true,
			"result": `API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`,
		}),
		err: errors.New("exit status 1"),
	}
	f := newFramework(fake, "mcpserver-path")

	_, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", SandboxRoot: t.TempDir()})
	limit, ok := agent.UsageLimit(err)
	if !ok {
		t.Fatalf("Run err = %v, want an agent.UsageLimitError", err)
	}
	if !strings.Contains(limit.Message, "rate_limit_error") {
		t.Errorf("Message = %q, want the provider's own words", limit.Message)
	}
	if !limit.ResetAt.IsZero() {
		t.Errorf("ResetAt = %s, want none: a bare 429 names no reset", limit.ResetAt)
	}
}

// A run that ran out of turns is not a run that ran out of budget, and
// the two must not be confused: one is a setting to raise, the other is
// a deployment-wide wait.
func TestRunKeepsMaxTurnsDistinctFromAUsageLimit(t *testing.T) {
	fake := &fakeRunner{
		stdout: streamJSONLine(t, map[string]any{
			"type": "result", "subtype": "error_max_turns", "is_error": true, "result": "",
		}),
		err: errors.New("exit status 1"),
	}
	f := newFramework(fake, "mcpserver-path", WithMaxTurns(3))

	_, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", SandboxRoot: t.TempDir()})
	if _, ok := agent.UsageLimit(err); ok {
		t.Fatalf("Run err = %v, want an ordinary turn-cap failure", err)
	}
	if !strings.Contains(err.Error(), "max turns") {
		t.Errorf("err = %q, want it to still name the turn cap", err)
	}
}

// The false positive that would matter: an agent whose own work quotes
// the phrase -- reading this very file, grepping a log -- must not put
// the whole deployment to sleep. A successful run is only ever read
// under the strict "|<epoch>" form, and only from the terminal result
// event, never from tool output.
func TestRunIgnoresTheLimitPhraseInASuccessfulRunsWork(t *testing.T) {
	fake := &fakeRunner{stdout: strings.Join([]string{
		streamJSONLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_use", "id": "call-1", "name": "run_command",
					"input": map[string]any{"command": "grep -r 'usage limit reached' ."},
				}},
			},
		}),
		streamJSONLine(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": "call-1",
					"content": "usagelimit.go: usage limit reached|1772370000",
				}},
			},
		}),
		streamJSONLine(t, map[string]any{"type": "result", "result": "found the phrase in one file"}),
	}, "\n")}
	f := newFramework(fake, "mcpserver-path")

	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "grep", SandboxRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Run err = %v, want the run to pass: nothing here is a refusal from the CLI", err)
	}
	if result.FinalText != "found the phrase in one file" {
		t.Errorf("FinalText = %q", result.FinalText)
	}
}

func TestResetFromEpochReadsBothUnits(t *testing.T) {
	seconds := resetFromEpoch("1772370000")
	if want := time.Unix(1772370000, 0).UTC(); !seconds.Equal(want) {
		t.Errorf("seconds epoch = %s, want %s", seconds, want)
	}
	millis := resetFromEpoch("1772370000000")
	if want := time.UnixMilli(1772370000000).UTC(); !millis.Equal(want) {
		t.Errorf("millisecond epoch = %s, want %s", millis, want)
	}
	// Both spellings must name the same moment, which is the whole point
	// of telling them apart by magnitude.
	if !seconds.Equal(millis) {
		t.Errorf("the same instant read back as %s and %s", seconds, millis)
	}
}

func TestUsageLimitFromResultRequiresTheStrictForm(t *testing.T) {
	if limit := usageLimitFromResult("I hit a rate limit exceeded error while testing"); limit != nil {
		t.Errorf("usageLimitFromResult reported %v for a sentence with no epoch", limit)
	}
	if limit := usageLimitFromResult("Claude AI usage limit reached|1772370000"); limit == nil {
		t.Error("usageLimitFromResult reported nothing for the CLI's own report")
	}
}
