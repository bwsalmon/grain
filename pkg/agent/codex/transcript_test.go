package codex

import (
	"strings"
	"testing"
)

// The two vocabularies transcript.go reads (rawEvent's own doc comment)
// have to produce the same Result from the same run, since which one a
// deployment gets is decided by the codex binary in its image. Every
// property below is asserted over both.
func TestParseTranscriptReadsBothEventVocabularies(t *testing.T) {
	itemForm := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"item.completed","item":{"id":"i0","item_type":"reasoning","text":"reading the repo"}}`,
		`{"type":"item.started","item":{"id":"i1","item_type":"mcp_tool_call","server":"grain-sandbox",` +
			`"tool":"run_command","arguments":{"command":"go test ./..."}}}`,
		`{"type":"item.completed","item":{"id":"i1","item_type":"mcp_tool_call","server":"grain-sandbox",` +
			`"tool":"run_command","status":"completed","result":{"content":[{"type":"text","text":"ok"}]}}}`,
		`{"type":"item.completed","item":{"id":"i2","item_type":"agent_message","text":"the tests pass"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1}}`,
	}, "\n")
	msgForm := strings.Join([]string{
		`{"id":"0","msg":{"type":"task_started"}}`,
		`{"id":"1","msg":{"type":"agent_reasoning","text":"reading the repo"}}`,
		`{"id":"2","msg":{"type":"mcp_tool_call_begin","call_id":"c1","invocation":` +
			`{"server":"grain-sandbox","tool":"run_command","arguments":{"command":"go test ./..."}}}}`,
		`{"id":"3","msg":{"type":"mcp_tool_call_end","call_id":"c1","invocation":` +
			`{"server":"grain-sandbox","tool":"run_command"},"result":{"Ok":{"content":[{"type":"text","text":"ok"}]}}}}`,
		`{"id":"4","msg":{"type":"agent_message","message":"the tests pass"}}`,
		`{"id":"5","msg":{"type":"task_complete","last_agent_message":"the tests pass"}}`,
	}, "\n")

	for name, stream := range map[string]string{"item": itemForm, "msg": msgForm} {
		t.Run(name, func(t *testing.T) {
			got, err := parseTranscript(stream)
			if err != nil {
				t.Fatalf("parseTranscript: %v", err)
			}
			if got.FinalText != "the tests pass" {
				t.Errorf("FinalText = %q", got.FinalText)
			}
			if len(got.ToolCalls) != 1 {
				t.Fatalf("ToolCalls = %+v, want one", got.ToolCalls)
			}
			call := got.ToolCalls[0]
			if call.Name != "run_command" || call.Text != "ok" || call.IsError {
				t.Errorf("ToolCalls[0] = %+v", call)
			}
			if call.Arguments["command"] != "go test ./..." {
				t.Errorf("ToolCalls[0].Arguments = %+v", call.Arguments)
			}
			for _, want := range []string{"reading the repo", "> run_command(", "ok", "the tests pass"} {
				if !strings.Contains(got.Transcript, want) {
					t.Errorf("Transcript = %q, want it to carry %q", got.Transcript, want)
				}
			}
		})
	}
}

// A capture with no terminal event at all is a run whose output was
// truncated -- codex died, or the file was read mid-stream -- and
// parseTranscript says so rather than reporting a run that ended with no
// answer.
func TestParseTranscriptRequiresATerminalEvent(t *testing.T) {
	_, err := parseTranscript(`{"type":"item.completed","item":{"id":"i1","item_type":"agent_message","text":"hi"}}`)
	if err == nil || !strings.Contains(err.Error(), "no terminal event") {
		t.Fatalf("err = %v, want the truncated-stream error", err)
	}
}

// A failed turn comes back as an error *and* a Result: the run may well
// have pushed a branch before the failure, and a caller handed only an
// error strands that work (agent.Framework's own contract).
func TestParseTranscriptReturnsWhatAFailedTurnDid(t *testing.T) {
	got, err := parseTranscript(strings.Join([]string{
		`{"type":"item.completed","item":{"id":"i1","item_type":"mcp_tool_call","server":"grain-sandbox",` +
			`"tool":"run_command","status":"completed","result":"pushed"}}`,
		`{"type":"turn.failed","error":{"message":"stream disconnected before completion"}}`,
	}, "\n"))
	if err == nil || !strings.Contains(err.Error(), "stream disconnected") {
		t.Fatalf("err = %v, want codex's own account of the failure", err)
	}
	if got == nil || len(got.ToolCalls) != 1 {
		t.Fatalf("Result = %+v, want the call the run made before failing", got)
	}
}

// A bare "error" event is the other way a run ends badly, and it is
// terminal too -- reading it as "still running" would leave every such
// run reported as a truncated stream instead of as what codex said.
func TestParseTranscriptTreatsAnErrorEventAsTerminal(t *testing.T) {
	_, err := parseTranscript(`{"type":"error","message":"model not found"}`)
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("err = %v, want the error event's own text", err)
	}
}

// A tool call that failed is recorded as one: orchestrator reads
// ToolCall.IsError, and a failure that reads as a success is a run whose
// story is wrong in the one place it matters.
func TestParseTranscriptMarksAFailedToolCall(t *testing.T) {
	got, err := parseTranscript(strings.Join([]string{
		`{"type":"item.completed","item":{"id":"i1","item_type":"mcp_tool_call","server":"grain-sandbox",` +
			`"tool":"read_file","status":"failed","error":"no such file"}}`,
		`{"type":"item.completed","item":{"id":"i2","item_type":"agent_message","text":"missing"}}`,
		`{"type":"turn.completed"}`,
	}, "\n"))
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(got.ToolCalls) != 1 || !got.ToolCalls[0].IsError || got.ToolCalls[0].Text != "no such file" {
		t.Fatalf("ToolCalls = %+v, want one failed call carrying its message", got.ToolCalls)
	}
	if !strings.Contains(got.Transcript, "! no such file") {
		t.Errorf("Transcript = %q, want the failure marked", got.Transcript)
	}
}

// codex's own shell is not one of grain's tools. It belongs in the
// narrative an operator reads and never in ToolCalls, which
// orchestrator.ProcessResult matches against the vocabulary pkg/mcp
// registers.
func TestParseTranscriptKeepsCodexsOwnShellOutOfToolCalls(t *testing.T) {
	got, err := parseTranscript(strings.Join([]string{
		`{"type":"item.started","item":{"id":"i1","item_type":"command_execution","command":"ls -la"}}`,
		`{"type":"item.completed","item":{"id":"i1","item_type":"command_execution","command":"ls -la",` +
			`"aggregated_output":"total 0","exit_code":0}}`,
		`{"type":"item.completed","item":{"id":"i2","item_type":"agent_message","text":"looked"}}`,
		`{"type":"turn.completed"}`,
	}, "\n"))
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(got.ToolCalls) != 0 {
		t.Fatalf("ToolCalls = %+v, want none: a native tool is not one grain published", got.ToolCalls)
	}
	if !strings.Contains(got.Transcript, "$ ls -la") || !strings.Contains(got.Transcript, "total 0") {
		t.Errorf("Transcript = %q, want the shell command and its output", got.Transcript)
	}
	// Announced once, not once per report of the same item.
	if strings.Count(got.Transcript, "$ ls -la") != 1 {
		t.Errorf("Transcript = %q, want the command announced exactly once", got.Transcript)
	}
}

// PartialTranscript reads a capture that is still being written, which
// routinely means one that starts mid-run and ends mid-line. Neither may
// cost it the part it can read.
func TestPartialTranscriptReadsAnUnfinishedCapture(t *testing.T) {
	got := PartialTranscript(strings.Join([]string{
		`{"type":"item.completed","item":{"id":"i1","item_type":"mcp_tool_call","server":"grain-sandbox",` +
			`"tool":"run_command","status":"completed","result":"ok"}}`,
		`{"type":"item.completed","item":{"id":"i2","item_type":"agent_m`,
	}, "\n"))
	if !strings.Contains(got, "> run_command(") || !strings.Contains(got, "ok") {
		t.Errorf("PartialTranscript = %q, want the complete lines rendered", got)
	}
	if got == "" {
		t.Error("PartialTranscript = \"\" for a capture with one complete line in it")
	}
}

// A completion with no start before it -- what PartialTranscript sees
// every time it reads a capture that begins mid-run -- still records the
// call rather than dropping what it returned.
func TestToolCallWithNoStartIsStillRecorded(t *testing.T) {
	got, err := parseTranscript(strings.Join([]string{
		`{"type":"item.completed","item":{"id":"i9","item_type":"mcp_tool_call","server":"grain-sandbox",` +
			`"tool":"comment_on_issue","status":"completed","result":"Recorded"}}`,
		`{"type":"turn.completed"}`,
	}, "\n"))
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "comment_on_issue" || got.ToolCalls[0].Text != "Recorded" {
		t.Fatalf("ToolCalls = %+v, want the call recorded from its completion alone", got.ToolCalls)
	}
}

// Arguments arrive as an object or as a string holding one, depending on
// the build and on whether codex parsed the model's payload before
// reporting it.
func TestArgumentsArriveAsAnObjectOrAsAStringHoldingOne(t *testing.T) {
	got, err := parseTranscript(strings.Join([]string{
		`{"type":"item.completed","item":{"id":"i1","item_type":"mcp_tool_call","server":"grain-sandbox",` +
			`"tool":"write_file","status":"completed","arguments":"{\"file_path\":\"out.txt\"}","result":"wrote"}}`,
		`{"type":"turn.completed"}`,
	}, "\n"))
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Arguments["file_path"] != "out.txt" {
		t.Fatalf("ToolCalls = %+v, want the string-encoded arguments decoded", got.ToolCalls)
	}
}
