// Package gemini implements agent.Framework by driving the Gemini API's
// function calling against an in-process v2/mcp server. Gemini itself never
// gets shell or filesystem access: the API only ever hands back structured
// FunctionCall requests, and this package's only way of turning one into an
// effect is a tools/call round trip through the MCP client, whose only
// route to anything is the registry it was constructed with -- confined to
// one RunConfig.SandboxRoot for the real tools, and touching no live
// GitHub API at all for the mocked ones. That is the whole isolation
// argument: not a permission check anywhere in this package, but the
// absence of any other path from "the model asked for X" to "X happened".
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
)

const (
	// DefaultModel is the current, non-retired Gemini model this package
	// was verified against; gemini-2.5-flash, the previous default, has
	// been retired to new callers.
	DefaultModel    = "gemini-3.6-flash"
	defaultMaxTurns = 20
)

// contentGenerator is the one method this package needs from
// *genai.Client.Models -- narrowed to an interface so a test can supply a
// fake and exercise the tool-call loop without a live API key.
type contentGenerator interface {
	GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
}

// Framework implements agent.Framework using the Gemini API.
type Framework struct {
	generator contentGenerator
	model     string
	maxTurns  int
}

// Option configures a Framework at construction time.
type Option func(*Framework)

func WithModel(model string) Option {
	return func(f *Framework) { f.model = model }
}

// WithMaxTurns overrides the default cap on model-response/tool-call round
// trips a single Run will make before giving up.
func WithMaxTurns(n int) Option {
	return func(f *Framework) { f.maxTurns = n }
}

// New builds a Framework backed by the real Gemini API at the given key.
func New(ctx context.Context, apiKey string, opts ...Option) (*Framework, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: creating client: %w", err)
	}
	return newFramework(client.Models, opts...), nil
}

func newFramework(generator contentGenerator, opts ...Option) *Framework {
	f := &Framework{generator: generator, model: DefaultModel, maxTurns: defaultMaxTurns}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Run implements agent.Framework: it starts an in-process MCP server
// scoped to cfg's sandbox tools -- cfg.Tools directly if the caller built
// its own set, otherwise mcp.NewSandboxTools(cfg.SandboxRoot) -- advertises
// them to Gemini as function declarations, and loops sending the
// conversation to the model and executing whatever function calls come
// back until a turn produces a final answer with no more calls, or
// MaxTurns is exhausted.
//
// Once the loop has started, an error is returned *with* the partial
// result rather than instead of it, and every caller must read both.
// Returning `nil, err` from here threw away the record of a run that had
// already done the work: an agent that edits files, commits and pushes
// and only then runs out of turns pushed a real branch, and the run it
// belongs to ended with a nil result, so the orchestrator skipped
// ProcessResult entirely and left that branch on GitHub with no pull
// request and no trace of the tool calls that made it (bwsalmon/agents
// task-1). The error still says the run failed; the result says what it
// managed to do first.
//
// When cfg.TranscriptPath is set, every line this func would otherwise
// only add to the in-memory transcript builder is also written straight
// through to that file as it is produced, the same live-mirror contract
// claude.Framework.Run gives its own subprocess's stdout (RunConfig.
// TranscriptPath's own doc comment, bwsalmon/agents#467) -- except here
// there is no subprocess to tee, so the mirror is this package's own
// fmt.Fprintf calls writing to two places instead of one. Unlike
// claude's raw --output-format stream-json capture, what lands in the
// file is already the finished human-readable narrative, one line at a
// time; a reader needs no parser, just the bytes so far (gemini.
// LiveTranscriptDir.Tail).
func (f *Framework) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	sandboxTools := cfg.Tools
	if sandboxTools == nil {
		if cfg.SandboxRoot == "" {
			return nil, fmt.Errorf("gemini: RunConfig.SandboxRoot or .Tools is required")
		}
		sandboxTools = mcp.NewSandboxTools(cfg.SandboxRoot)
	}
	maxTurns := f.maxTurns
	if cfg.MaxTurns > 0 {
		maxTurns = cfg.MaxTurns
	}

	registry := mcp.NewRegistry()
	registry.Register(sandboxTools...)
	registry.Register(mcp.NewMockTools(&mcp.MockSink{})...)
	client := mcp.NewInProcess(ctx, registry)
	defer client.Close()

	toolInfos, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("gemini: listing tools: %w", err)
	}
	tools := []*genai.Tool{{FunctionDeclarations: toFunctionDeclarations(toolInfos)}}

	history := []*genai.Content{genai.NewContentFromText(cfg.Prompt, genai.RoleUser)}
	result := &agent.Result{}
	var transcript strings.Builder
	var transcriptOut io.Writer = &transcript
	if cfg.TranscriptPath != "" {
		// O_TRUNC, not O_APPEND, for the same reason claude.Framework.Run's
		// own doc comment gives: a path is only ever reused across runs by
		// a caller passing the same run ID twice, which should never
		// happen, so starting clean is what makes a stale previous run's
		// bytes never a way to misread this one's.
		transcriptFile, err := os.OpenFile(cfg.TranscriptPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("gemini: opening transcript file: %w", err)
		}
		defer transcriptFile.Close()
		transcriptOut = io.MultiWriter(&transcript, transcriptFile)
	}
	// Result.Transcript is finalized from whatever accumulated no matter
	// which of Run's several return points is taken -- an error partway
	// through must not throw away the narrative of what already happened,
	// the same reasoning this func's own doc comment gives for returning
	// a non-nil result alongside an error at all.
	defer func() { result.Transcript = strings.TrimSpace(transcript.String()) }()

	for turn := 0; turn < maxTurns; turn++ {
		resp, err := f.generator.GenerateContent(ctx, f.model, history, &genai.GenerateContentConfig{Tools: tools})
		if err != nil {
			return result, fmt.Errorf("gemini: generate content: %w", err)
		}
		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			return result, fmt.Errorf("gemini: response had no candidate content")
		}
		candidate := resp.Candidates[0]
		history = append(history, candidate.Content)

		var funcCalls []*genai.FunctionCall
		var text strings.Builder
		for _, part := range candidate.Content.Parts {
			if part.FunctionCall != nil {
				funcCalls = append(funcCalls, part.FunctionCall)
			}
			if part.Text == "" {
				continue
			}
			// A thought part explains what the model is about to do
			// rather than answering the prompt -- it belongs in the
			// transcript alongside the tool calls it precedes, not in
			// FinalText, the same split claude/transcript.go draws
			// between a "thinking" block and a "text" one.
			if part.Thought {
				fmt.Fprintf(transcriptOut, "%s\n\n", part.Text)
				continue
			}
			text.WriteString(part.Text)
			fmt.Fprintf(transcriptOut, "%s\n\n", part.Text)
		}
		if len(funcCalls) == 0 {
			result.FinalText = text.String()
			return result, nil
		}

		responseParts := make([]*genai.Part, 0, len(funcCalls))
		for _, fc := range funcCalls {
			callResult, callErr := client.CallTool(ctx, fc.Name, fc.Args)
			var toolText string
			var isError bool
			if callErr != nil {
				toolText, isError = callErr.Error(), true
			} else {
				toolText, isError = callResult.Text(), callResult.IsError
			}
			result.ToolCalls = append(result.ToolCalls, agent.ToolCall{
				Name: fc.Name, Arguments: fc.Args, Text: toolText, IsError: isError,
			})
			fmt.Fprintf(transcriptOut, "> %s(%s)\n", fc.Name, inputSummary(fc.Args))
			if isError {
				fmt.Fprintf(transcriptOut, "! %s\n\n", toolText)
			} else {
				fmt.Fprintf(transcriptOut, "%s\n\n", toolText)
			}
			responseParts = append(responseParts, genai.NewPartFromFunctionResponse(fc.Name, map[string]any{
				"result": toolText, "isError": isError,
			}))
		}
		history = append(history, genai.NewContentFromParts(responseParts, genai.RoleUser))
	}

	return result, fmt.Errorf("gemini: exceeded max turns (%d) without a final answer", maxTurns)
}

// inputSummary renders a function call's own args as compact JSON for a
// transcript line -- best-effort, matching claude/transcript.go's own
// helper of the same name and purpose: a malformed value here should cost
// the transcript one unreadable line, never the whole run.
func inputSummary(args map[string]any) string {
	data, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(data)
}
