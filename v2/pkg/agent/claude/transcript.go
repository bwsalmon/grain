package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/agent"
)

// rawEvent is one --output-format stream-json line -- the same event shape
// grain/automation/transcript.py parses (see its own docstring for the
// field-by-field justification: each line is one Claude Code event, tagged
// by its own "type" field), narrowed to only what this package needs to
// build an agent.Result rather than a full trajectory viewer.
type rawEvent struct {
	Type    string      `json:"type"`
	Subtype string      `json:"subtype"`
	IsError bool        `json:"is_error"`
	Message *rawMessage `json:"message"`
	Result  string      `json:"result"`
}

type rawMessage struct {
	Content json.RawMessage `json:"content"`
}

type rawBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Thinking carries a "thinking"-type block's own text -- Claude's
	// extended-thinking output, present only when that feature is on, and
	// otherwise the one part of a real transcript neither FinalText nor
	// ToolCalls ever captured before bwsalmon/agents#446 gave it
	// somewhere to go (agent.Result.Transcript).
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// parseTranscript turns a full --output-format stream-json capture into a
// Result: FinalText from the terminal "result" event, ToolCalls built by
// pairing each "tool_use" block in an assistant message with the
// "tool_result" block bearing the same tool_use_id wherever it later
// appears in a user message -- the same pairing transcript.py's TUI does to
// render a trajectory, done here to produce agent.ToolCall values instead
// of display strings. A malformed or unrecognized line is skipped rather
// than treated as fatal, matching transcript.py's own tolerance: Claude
// Code's event shape grows new fields and subtypes across versions, and
// this parser seeing one it doesn't know about should not make a whole run
// unreadable.
//
// The same pass also builds Result.Transcript: every "thinking" and
// "text" block's own text, and a line for each tool call and what it got
// back, in the order they appeared -- the human-readable narrative
// FinalText and ToolCalls alone do not give a reader (bwsalmon/agents#446).
func parseTranscript(stdout string) (*agent.Result, error) {
	p := parseEvents(stdout)
	if p.resultErr != nil {
		return nil, p.resultErr
	}
	if !p.sawResult {
		return nil, fmt.Errorf("claude: no result event found in output")
	}
	p.result.Transcript = strings.TrimSpace(p.transcript.String())
	return &p.result, nil
}

// PartialTranscript renders whatever of a still-in-progress
// --output-format stream-json capture is on disk so far into the same
// human-readable narrative parseTranscript builds once a run finishes --
// what LiveTranscriptDir reads back for a run with no FinishedAt yet
// (bwsalmon/agents#467). Unlike parseTranscript it never errors: a
// missing (or not-yet-written) terminal "result" event just means the run
// is still going, not that anything is wrong, and a truncated final line
// -- reading a file the whole time claude is still appending to it can
// always catch mid-write -- is handled the same way parseTranscript
// already tolerates any other malformed line, by skipping it.
func PartialTranscript(stdout string) string {
	return strings.TrimSpace(parseEvents(stdout).transcript.String())
}

// parsedEvents is one line-by-line pass over a stream-json capture,
// shared by parseTranscript (the whole thing, once a run has finished)
// and PartialTranscript (however much exists so far) so the two can never
// build the transcript text itself two different ways.
type parsedEvents struct {
	result     agent.Result
	pending    map[string]int
	sawResult  bool
	resultErr  error
	transcript strings.Builder
}

func parseEvents(stdout string) *parsedEvents {
	p := &parsedEvents{pending: map[string]int{}}

	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev rawEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "assistant", "user":
			if ev.Message == nil {
				continue
			}
			for _, b := range parseBlocks(ev.Message.Content) {
				switch b.Type {
				case "thinking":
					if b.Thinking != "" {
						fmt.Fprintf(&p.transcript, "%s\n\n", b.Thinking)
					}
				case "text":
					if b.Text != "" {
						fmt.Fprintf(&p.transcript, "%s\n\n", b.Text)
					}
				case "tool_use":
					p.result.ToolCalls = append(p.result.ToolCalls, agent.ToolCall{Name: b.Name, Arguments: b.Input})
					p.pending[b.ID] = len(p.result.ToolCalls) - 1
					fmt.Fprintf(&p.transcript, "> %s(%s)\n", b.Name, inputSummary(b.Input))
				case "tool_result":
					if idx, ok := p.pending[b.ToolUseID]; ok {
						text := toolResultText(b.Content)
						p.result.ToolCalls[idx].Text = text
						p.result.ToolCalls[idx].IsError = b.IsError
						if b.IsError {
							fmt.Fprintf(&p.transcript, "! %s\n\n", text)
						} else {
							fmt.Fprintf(&p.transcript, "%s\n\n", text)
						}
					}
				}
			}
		case "result":
			p.sawResult = true
			if ev.IsError {
				p.resultErr = fmt.Errorf("claude: run ended in error (subtype=%s): %s", ev.Subtype, ev.Result)
				continue
			}
			p.result.FinalText = ev.Result
		}
	}
	return p
}

// inputSummary renders a tool_use block's own input as compact JSON for a
// transcript line -- best-effort, since a malformed value here should
// cost the transcript one unreadable line, never the whole parse (see
// parseTranscript's own doc comment on the same tolerance for the stream
// itself).
func inputSummary(input map[string]any) string {
	data, err := json.Marshal(input)
	if err != nil {
		return fmt.Sprintf("%v", input)
	}
	return string(data)
}

// parseBlocks reads a message's content field, which is either a plain
// string or a list of typed blocks (text, tool_use, tool_result) -- both
// shapes appear in a real transcript, per transcript.py's own _text_of.
func parseBlocks(raw json.RawMessage) []rawBlock {
	var blocks []rawBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return []rawBlock{{Type: "text", Text: s}}
	}
	return nil
}

// toolResultText reads a tool_result block's own content field, which has
// the same string-or-blocks shape as a message's content -- transcript.py's
// _text_of joins multiple text blocks with a space; this does the same.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}
