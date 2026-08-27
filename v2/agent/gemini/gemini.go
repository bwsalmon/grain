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
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/agent"
	"github.com/bwsalmon/grain/v2/mcp"
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

// Run implements agent.Framework: it starts an in-process MCP server scoped
// to cfg.SandboxRoot, advertises its tools to Gemini as function
// declarations, and loops sending the conversation to the model and
// executing whatever function calls come back until a turn produces a
// final answer with no more calls, or MaxTurns is exhausted.
func (f *Framework) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	if cfg.SandboxRoot == "" {
		return nil, fmt.Errorf("gemini: RunConfig.SandboxRoot is required")
	}
	maxTurns := f.maxTurns
	if cfg.MaxTurns > 0 {
		maxTurns = cfg.MaxTurns
	}

	registry := mcp.NewRegistry()
	registry.Register(mcp.NewSandboxTools(cfg.SandboxRoot)...)
	registry.Register(mcp.NewMockTools(&mcp.MockSink{})...)
	client := mcp.NewInProcess(registry)
	defer client.Close()

	toolInfos, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("gemini: listing tools: %w", err)
	}
	tools := []*genai.Tool{{FunctionDeclarations: toFunctionDeclarations(toolInfos)}}

	history := []*genai.Content{genai.NewContentFromText(cfg.Prompt, genai.RoleUser)}
	result := &agent.Result{}

	for turn := 0; turn < maxTurns; turn++ {
		resp, err := f.generator.GenerateContent(ctx, f.model, history, &genai.GenerateContentConfig{Tools: tools})
		if err != nil {
			return nil, fmt.Errorf("gemini: generate content: %w", err)
		}
		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			return nil, fmt.Errorf("gemini: response had no candidate content")
		}
		candidate := resp.Candidates[0]
		history = append(history, candidate.Content)

		var funcCalls []*genai.FunctionCall
		var text strings.Builder
		for _, part := range candidate.Content.Parts {
			if part.FunctionCall != nil {
				funcCalls = append(funcCalls, part.FunctionCall)
			}
			if part.Text != "" {
				text.WriteString(part.Text)
			}
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
			responseParts = append(responseParts, genai.NewPartFromFunctionResponse(fc.Name, map[string]any{
				"result": toolText, "isError": isError,
			}))
		}
		history = append(history, genai.NewContentFromParts(responseParts, genai.RoleUser))
	}

	return nil, fmt.Errorf("gemini: exceeded max turns (%d) without a final answer", maxTurns)
}
