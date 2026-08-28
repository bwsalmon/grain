// Package claude implements agent.Framework by running the real `claude`
// CLI as a subprocess on the controller -- the same shape v1's
// grain/automation/dispatch.py already settled on (see that file's own
// docstring, "Why the agent doesn't run on the sandbox anymore"): the
// agent's OAuth credential must never enter the untrusted execution
// environment, so `claude -p` runs here on the controller with its native
// tool roster emptied out, reaching the sandbox only through the MCP tools
// grain's own "mcpserver" subcommand exposes (v2/cmd/grain/mcpserver.go).
//
// Unlike agent/gemini, there is no lightweight API this package can drive
// in-process -- claude -p is the actual product bwsalmon/agents#255 asked
// to add as an option, so Run spawns it directly over os/exec and lets it
// manage its own MCP connection, rather than this package looping turns
// itself the way agent/gemini's Run does. --mcp-config points that
// connection at grainBinaryPath (this same grain binary -- bwsalmon/
// agents#313 combined what used to be a standalone cmd/mcpserver build
// into a subcommand of the one binary everything else here runs as too)
// with "mcpserver" and -sandbox-root set to RunConfig.SandboxRoot as its
// args -- claude forks that command itself once it loads --mcp-config,
// the "forking off processes" v2/cmd/grain/main.go's own doc comment on
// this package refers to. Nothing in a tool call itself can ever change
// where that lands, mirroring v1's own "only place it's named"
// discipline (dispatch.py's _mcp_config_json).
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
)

const (
	defaultMaxTurns = 20

	// mcpServerName is the mcpServers key written into --mcp-config, and
	// therefore the prefix claude reports every tool under
	// ("mcp__<name>__<tool>") -- matching v1's own "grain-sandbox" naming
	// (dispatch.py's _mcp_config_json) purely for continuity; claude does
	// not care what this string is.
	mcpServerName = "grain-sandbox"
)

// runner is the one seam this package needs to actually invoke the claude
// binary -- narrowed to an interface so a test can supply a canned
// transcript and exercise the parsing logic below without a real claude
// binary or a live Claude Code credential.
type runner interface {
	Run(ctx context.Context, args []string, stdin string, env []string) (stdout string, err error)
}

type execRunner struct {
	claudePath string
}

func (r execRunner) Run(ctx context.Context, args []string, stdin string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, r.claudePath, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Framework implements agent.Framework by running `claude -p`.
type Framework struct {
	run             runner
	grainBinaryPath string
	oauthToken      string
	maxTurns        int
}

// Option configures a Framework at construction time.
type Option func(*Framework)

// WithMaxTurns overrides the default cap on agentic turns a single Run
// allows before claude's own --max-turns cuts it off.
func WithMaxTurns(n int) Option {
	return func(f *Framework) { f.maxTurns = n }
}

// WithOAuthToken sets the CLAUDE_CODE_OAUTH_TOKEN passed to the claude
// subprocess's own environment -- never as a command-line argument, so it
// never lands in `ps` output (the same reasoning as v1's
// CONTROLLER_AGENT_TOKEN_PATH; see dispatch.py's start_unit call site).
func WithOAuthToken(token string) Option {
	return func(f *Framework) { f.oauthToken = token }
}

// New builds a Framework that runs the real claude binary at claudePath
// (typically just "claude", resolved against $PATH) and points every
// run's --mcp-config at grainBinaryPath -- the same grain binary this
// process itself is (typically the result of os.Executable()) -- invoked
// as "grainBinaryPath mcpserver -sandbox-root <root>".
func New(claudePath, grainBinaryPath string, opts ...Option) *Framework {
	return newFramework(execRunner{claudePath: claudePath}, grainBinaryPath, opts...)
}

func newFramework(run runner, grainBinaryPath string, opts ...Option) *Framework {
	f := &Framework{run: run, grainBinaryPath: grainBinaryPath, maxTurns: defaultMaxTurns}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// allowedTools names the exact tools NewSandboxTools and NewMockTools
// register, mcp__-prefixed the way claude reports them once loaded from
// --mcp-config -- computed from those constructors directly rather than
// hand-copied, so this can never drift from what the "mcpserver"
// subcommand actually advertises the way v1's hand-maintained
// _ALLOWED_TOOLS constant could (dispatch.py).
func allowedTools() []string {
	var names []string
	for _, t := range mcp.NewSandboxTools("") {
		names = append(names, fmt.Sprintf("mcp__%s__%s", mcpServerName, t.Name))
	}
	for _, t := range mcp.NewMockTools(&mcp.MockSink{}) {
		names = append(names, fmt.Sprintf("mcp__%s__%s", mcpServerName, t.Name))
	}
	return names
}

// mcpConfigJSON is the --mcp-config file content: grainBinaryPath spawned
// with "mcpserver -sandbox-root sandboxRoot" -- the "mcpserver" argument
// selects the same subcommand mcpserver.go implements, so claude forking
// this exact command is what actually starts an MCP server, rather than
// needing a separately built binary on disk.
func mcpConfigJSON(grainBinaryPath, sandboxRoot string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			mcpServerName: map[string]any{
				"command": grainBinaryPath,
				"args":    []string{"mcpserver", "-sandbox-root", sandboxRoot},
			},
		},
	})
}

// Run implements agent.Framework: it writes an --mcp-config file pointing
// at grainBinaryPath and cfg.SandboxRoot, runs claude -p with its native
// tool roster emptied out and only the grain-sandbox MCP tools admitted,
// and parses the resulting --output-format stream-json transcript into a
// Result.
func (f *Framework) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	if cfg.SandboxRoot == "" {
		return nil, fmt.Errorf("claude: RunConfig.SandboxRoot is required")
	}
	if f.grainBinaryPath == "" {
		return nil, fmt.Errorf("claude: grainBinaryPath is required")
	}
	maxTurns := f.maxTurns
	if cfg.MaxTurns > 0 {
		maxTurns = cfg.MaxTurns
	}

	configJSON, err := mcpConfigJSON(f.grainBinaryPath, cfg.SandboxRoot)
	if err != nil {
		return nil, fmt.Errorf("claude: building mcp config: %w", err)
	}
	configFile, err := os.CreateTemp("", "grain-claude-mcp-config-*.json")
	if err != nil {
		return nil, fmt.Errorf("claude: creating mcp config file: %w", err)
	}
	defer os.Remove(configFile.Name())
	_, writeErr := configFile.Write(configJSON)
	closeErr := configFile.Close()
	if writeErr != nil {
		return nil, fmt.Errorf("claude: writing mcp config file: %w", writeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("claude: writing mcp config file: %w", closeErr)
	}

	// --tools '' empties claude's native tool roster entirely (confirmed
	// live in v1 -- dispatch.py's own docstring: "--tools '' --mcp-config
	// <file> --allowedTools <names> genuinely empties the roster down to
	// exactly the MCP tools"), and --strict-mcp-config plus --allowedTools
	// admit only the grain-sandbox tools named above -- unlike v1, nothing
	// here re-admits a native "Task" tool for subagents, since v2 has no
	// self-debug/self-repair story yet for one to inherit.
	args := []string{
		"-p",
		"--tools", "",
		"--mcp-config", configFile.Name(),
		"--strict-mcp-config",
		"--allowedTools", strings.Join(allowedTools(), ","),
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", strconv.Itoa(maxTurns),
	}
	var env []string
	if f.oauthToken != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+f.oauthToken)
	}

	// The prompt travels over stdin, never argv -- untrusted issue content
	// must never become a shell- or ps-visible argument (dispatch.py's own
	// docstring makes the identical point about its `dd`/stdin-redirect
	// path).
	stdout, err := f.run.Run(ctx, args, cfg.Prompt, env)
	if err != nil {
		return nil, fmt.Errorf("claude: running claude: %w", err)
	}
	return parseTranscript(stdout)
}
