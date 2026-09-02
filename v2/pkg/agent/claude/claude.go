// Package claude implements agent.Framework by running the real `claude`
// CLI as a subprocess on the controller -- the same shape v1's
// grain/automation/dispatch.py already settled on (see that file's own
// docstring, "Why the agent doesn't run on the sandbox anymore"): the
// agent's OAuth credential must never enter the untrusted execution
// environment, so `claude -p` runs here on the controller with its native
// tool roster emptied out, reaching the sandbox only through the MCP tools
// grain's own "mcpserver" subcommand exposes (v2/cmd/grain/mcpserver.go).
//
// claude -p is the actual product bwsalmon/agents#255 asked to add as an
// option, so Run spawns it directly over os/exec and lets it manage its
// own MCP connection rather than looping turns itself. --mcp-config points that
// connection at grainBinaryPath (this same grain binary -- bwsalmon/
// agents#313 combined what used to be a standalone cmd/mcpserver build
// into a subcommand of the one binary everything else here runs as too)
// with "mcpserver" and either -sandbox-root (RunConfig.SandboxRoot, a
// local directory) or -kontur-vm (RunConfig.KonturVM, a named
// orchestrator.KonturSandboxes VM, plus WithKonturSSH's own
// -ssh-user/-exec-key/-workspace) as its args -- claude forks that
// command itself once it loads --mcp-config, the "forking off processes"
// v2/cmd/grain/main.go's own doc comment on this package refers to.
// Nothing in a tool call itself can ever change where that lands,
// mirroring v1's own "only place it's named" discipline (dispatch.py's
// _mcp_config_json).
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/procgroup"
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
// binary or a live Claude Code credential. tee, when non-nil, receives
// every byte of stdout as the subprocess produces it, alongside the
// buffer this still accumulates and returns whole once the process exits
// -- see execRunner.Run's own doc comment on why a live copy of the exact
// same bytes is worth keeping.
type runner interface {
	Run(ctx context.Context, args []string, stdin string, env []string, tee io.Writer) (stdout string, err error)
}

type execRunner struct {
	claudePath string
}

// Run execs claude and, when tee is non-nil, mirrors its stdout into tee
// via io.MultiWriter as the subprocess produces it -- not after cmd.Run
// returns, since exec.Cmd itself copies from the child's stdout pipe into
// whatever Writer it's given the moment there is anything to copy. That
// live mirror is what RunConfig.TranscriptPath is for: a caller with
// filesystem access to that path can read a run's own stream-json output
// while claude is still running, rather than only once this whole
// function returns (bwsalmon/agents#467).
func (r execRunner) Run(ctx context.Context, args []string, stdin string, env []string, tee io.Writer) (string, error) {
	cmd := exec.CommandContext(ctx, r.claudePath, args...)
	// claude forks its own mcpserver child once it loads --mcp-config (see
	// this file's own doc comment); a plain exec.CommandContext cancel
	// only kills claude itself and leaves that child (and anything
	// run_command's own `bash -c` spawned under it) running as an orphan.
	// procgroup.Prepare makes cancelling ctx kill that whole tree instead.
	procgroup.Prepare(cmd)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	if tee != nil {
		cmd.Stdout = io.MultiWriter(&stdout, tee)
	} else {
		cmd.Stdout = &stdout
	}
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
	oauthToken      func(context.Context) (string, error)
	maxTurns        int
	konturSSHUser   string
	konturExecKey   string
	konturWorkspace string
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
	return WithOAuthTokenFunc(func(context.Context) (string, error) { return token, nil })
}

// WithOAuthTokenFunc is WithOAuthToken for a deployment whose token is
// not known once, at construction: fn is called on every Run instead, so
// a token set (or replaced) while the daemon is running takes effect on
// the next run rather than at the next restart. cmd/grain's daemon reads
// it out of the secrets database the UI writes to, which is what makes
// "paste the token into Settings" work at all (see that package's own
// agentCredential).
//
// An error from fn fails the run: a token that cannot be read is not the
// same as a deployment that deliberately configured none, which stays
// what an empty string means here (Run then passes no
// CLAUDE_CODE_OAUTH_TOKEN at all and lets the subprocess authenticate
// from its own ambient environment, as it always could).
func WithOAuthTokenFunc(fn func(context.Context) (string, error)) Option {
	return func(f *Framework) { f.oauthToken = fn }
}

// WithKonturSSH gives a Framework what it needs to reach a
// RunConfig.KonturVM instead of a RunConfig.SandboxRoot: the same
// deployment-wide sshUser/execKey/workspace triple
// orchestrator.KonturConfig already carries, passed straight through as
// the forked "mcpserver -kontur-vm"'s own -ssh-user/-exec-key/-workspace
// (mirroring mcpserver.go's mustKonturSandboxTools). A deployment that
// never sets -kontur-sandboxes has no use for this; Run rejects a
// RunConfig.KonturVM without it.
func WithKonturSSH(sshUser, execKey, workspace string) Option {
	return func(f *Framework) {
		f.konturSSHUser, f.konturExecKey, f.konturWorkspace = sshUser, execKey, workspace
	}
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
// with mcpArgs (built by mcpServerArgs below) -- the "mcpserver" argument
// selects the same subcommand mcpserver.go implements, so claude forking
// this exact command is what actually starts an MCP server, rather than
// needing a separately built binary on disk.
func mcpConfigJSON(grainBinaryPath string, mcpArgs []string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			mcpServerName: map[string]any{
				"command": grainBinaryPath,
				"args":    mcpArgs,
			},
		},
	})
}

// mcpServerArgs builds the arguments the forked "mcpserver" subcommand
// needs to reach cfg's sandbox: "-sandbox-root <dir>" for a local
// directory, or "-kontur-vm <name> -ssh-user ... -exec-key ... -workspace
// ..." (mirroring mcpserver.go's own mustKonturSandboxTools) for a named
// kontur VM, whichever RunConfig set. SandboxRoot wins if somehow both
// are -- RunDispatch never sets both at once in practice (a sandbox is
// either host-rooted or kontur-named, never both), but a Framework this
// simple is not the place to enforce that.
func (f *Framework) mcpServerArgs(cfg agent.RunConfig) ([]string, error) {
	switch {
	case cfg.SandboxRoot != "":
		return []string{"mcpserver", "-sandbox-root", cfg.SandboxRoot}, nil
	case cfg.KonturVM != "":
		if f.konturSSHUser == "" || f.konturExecKey == "" || f.konturWorkspace == "" {
			return nil, fmt.Errorf("claude: RunConfig.KonturVM is set but this Framework has no kontur SSH config (see WithKonturSSH)")
		}
		return []string{
			"mcpserver", "-kontur-vm", cfg.KonturVM,
			"-ssh-user", f.konturSSHUser, "-exec-key", f.konturExecKey, "-workspace", f.konturWorkspace,
		}, nil
	default:
		return nil, fmt.Errorf("claude: RunConfig.SandboxRoot or .KonturVM is required")
	}
}

// Run implements agent.Framework: it writes an --mcp-config file pointing
// at grainBinaryPath and cfg.SandboxRoot or cfg.KonturVM (mcpServerArgs),
// runs claude -p with its native tool roster emptied out and only the
// grain-sandbox MCP tools admitted, and parses the resulting
// --output-format stream-json transcript into a Result.
//
// When cfg.TranscriptPath is set, the raw stream-json this produces is
// also mirrored there live, one line per event, as claude itself emits
// them (execRunner.Run's own doc comment) -- the file this package's own
// LiveTranscriptDir reads back with PartialTranscript to render a
// still-running run's transcript-in-progress. It is opened O_TRUNC, not
// O_APPEND: a path is only ever reused across runs by a caller passing
// the same run ID twice, which should never happen, and starting clean
// is what makes a stale previous run's bytes never a way to misread this
// one's. The file is left on disk once Run returns either way -- cleaning
// it up once its contents no longer matter (the run has finished and
// Result.Transcript already carries the same story) is the caller's job,
// the same way orchestrator.RunDispatch owns cfg.SandboxRoot's own
// lifecycle rather than this package.
func (f *Framework) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	mcpArgs, err := f.mcpServerArgs(cfg)
	if err != nil {
		return nil, err
	}
	if f.grainBinaryPath == "" {
		return nil, fmt.Errorf("claude: grainBinaryPath is required")
	}
	maxTurns := f.maxTurns
	if cfg.MaxTurns > 0 {
		maxTurns = cfg.MaxTurns
	}

	var tee io.Writer
	if cfg.TranscriptPath != "" {
		transcriptFile, err := os.OpenFile(cfg.TranscriptPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("claude: opening transcript file: %w", err)
		}
		defer transcriptFile.Close()
		tee = transcriptFile
	}

	configJSON, err := mcpConfigJSON(f.grainBinaryPath, mcpArgs)
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
	if f.oauthToken != nil {
		token, err := f.oauthToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("claude: reading the Claude Code OAuth token: %w", err)
		}
		if token != "" {
			env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+token)
		}
	}

	// The prompt travels over stdin, never argv -- untrusted issue content
	// must never become a shell- or ps-visible argument (dispatch.py's own
	// docstring makes the identical point about its `dd`/stdin-redirect
	// path).
	stdout, err := f.run.Run(ctx, args, cfg.Prompt, env, tee)
	if err != nil {
		return nil, fmt.Errorf("claude: running claude: %w", err)
	}
	return parseTranscript(stdout)
}
