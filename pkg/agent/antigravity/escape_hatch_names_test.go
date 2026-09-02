package antigravity

import (
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/mcp"
)

// TestEscapeHatchToolCallsAreRecordedUnderTheirBareNames is
// agent/claude's test of the same name, for agy's own step_update
// vocabulary: agy reports a tool it loaded from its MCP settings as
// "mcp__grain-sandbox__<tool>" (this package's own allowedTools writes
// that prefix), and orchestrator.ProcessResult matches ToolCall.Name
// against "ask_question", "comment_on_issue" and "propose_task" exactly.
// Recording the prefixed name matched none of them, so a run that asked
// the human a question was recorded no_action and the question was never
// relayed. Every other test here scripts the bare name, which is why
// nothing caught it.
func TestEscapeHatchToolCallsAreRecordedUnderTheirBareNames(t *testing.T) {
	for _, tool := range []string{"ask_question", "comment_on_issue", "propose_task"} {
		t.Run(tool, func(t *testing.T) {
			reported := mcp.QualifiedToolName(tool)
			got, err := parseTranscript(stream(
				initLine,
				toolActive(0, reported, `{"question":"which config file?"}`),
				toolDone(0, reported, "Recorded"),
				`{"event":"result","result":{"status":"SUCCESS","response":"waiting on a reply"}}`,
			))
			if err != nil {
				t.Fatalf("parseTranscript: %v", err)
			}
			if len(got.ToolCalls) != 1 {
				t.Fatalf("ToolCalls = %+v, want 1", got.ToolCalls)
			}
			if got.ToolCalls[0].Name != tool {
				t.Errorf("ToolCalls[0].Name = %q, want %q -- orchestrator.ProcessResult "+
					"matches the bare name exactly, so anything else relays nothing",
					got.ToolCalls[0].Name, tool)
			}
			if strings.Contains(got.Transcript, "mcp__") {
				t.Errorf("Transcript = %q, want no mcp__ prefix in it", got.Transcript)
			}
		})
	}
}

// TestToolStepWithNoActiveUpdateIsAlsoRecordedBarely covers applyStep's
// other route into ToolCalls -- a terminal update with no ACTIVE one
// before it, which PartialTranscript hits every time it reads a capture
// that starts mid-run. It names the call through the same rawStep.toolCall,
// so it must normalize the same way rather than only the common path.
func TestToolStepWithNoActiveUpdateIsAlsoRecordedBarely(t *testing.T) {
	got, err := parseTranscript(stream(
		initLine,
		toolDone(0, mcp.QualifiedToolName("comment_on_issue"), "Recorded"),
		`{"event":"result","result":{"status":"SUCCESS","response":"done"}}`,
	))
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "comment_on_issue" {
		t.Fatalf("ToolCalls = %+v, want one named %q", got.ToolCalls, "comment_on_issue")
	}
}
