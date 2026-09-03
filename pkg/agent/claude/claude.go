// Package claude implements agent.Framework by running the real `claude`
// CLI as a subprocess on the controller -- the same shape v1's
// grain/automation/dispatch.py already settled on (see that file's own
// docstring, "Why the agent doesn't run on the sandbox anymore"): the
// agent's OAuth credential must never enter the untrusted execution
// environment, so `claude -p` runs here on the controller with its native
// tool roster emptied out, reaching the sandbox only through the MCP tools
// grain's own "mcpserver" subcommand exposes (cmd/grain/mcpserver.go).
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
// cmd/grain/main.go's own doc comment on this package refers to.
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
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/procgroup"
)

const (
	// DefaultModel is the model claude is asked for when a caller names
	// none. Left empty deliberately would mean "whatever the binary
	// defaults to"; naming it here keeps a deployment's runs reproducible
	// across claude CLI upgrades the same way agent/antigravity's own
	// DefaultModel does.
	DefaultModel = "claude-sonnet-5"

	// defaultMaxTurns is the cap on model/tool round trips a run may take
	// before claude's own --max-turns cuts it off, and zero means no cap
	// at all: Run passes no --max-turns whatsoever, exactly as v1
	// (grain/automation/dispatch.py) always did.
	//
	// A turn here is one model/tool round trip, and the ordinary shape of
	// a grain task -- read the repo, edit, run the tests, read the
	// failures, edit again, commit, push -- spends dozens of them. Any
	// number picked here is a guess at how much work a task deserves,
	// made by a package that knows nothing about the task; guessing wrong
	// does not slow a run down, it fails one that was working. The 20
	// this started at did exactly that, and while raising it made that
	// rarer it left the same cliff a little further out.
	//
	// Turns were never the bound worth enforcing anyway. What a runaway
	// run actually costs is wall-clock time and tokens, and
	// orchestrator's Config.MaxRunRuntime (two hours by default) already
	// stops one on the first of those regardless of how fast its turns
	// go. A deployment that does want a turn ceiling still sets one --
	// `grain settings -max-agent-turns`, RunConfig.MaxTurns, or
	// WithMaxTurns -- and gets it passed through unchanged.
	defaultMaxTurns = 0

	// mcpServerName is the mcpServers key written into --mcp-config, and
	// therefore the prefix claude reports every tool under
	// ("mcp__<name>__<tool>"). It lives in pkg/mcp rather than here
	// because both halves of that convention -- writing the prefix onto
	// --allowedTools, and taking it back off a reported call before
	// recording it (transcript.go) -- have to agree, and agent/antigravity
	// needs the identical pair for its own CLI.
	mcpServerName = mcp.ToolNamespace
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
		// stdout is returned alongside the error rather than discarded --
		// the same contract agent/antigravity's own execRunner already
		// settled on, and for the same reason. claude exits non-zero for
		// every run whose terminal "result" event is an error, including
		// the routine "--max-turns ran out" one, and it reports that
		// error on stdout as stream-json rather than on stderr: a caller
		// handed only `exit status 1 (stderr: )` cannot tell a run that
		// edited, committed and pushed before it ran out of turns from
		// one that never started. Everything worth knowing about the
		// failure is in the bytes below.
		return stdout.String(), fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Framework implements agent.Framework by running `claude -p`.
type Framework struct {
	run             runner
	grainBinaryPath string
	oauthToken      func(context.Context) (string, error)
	model           string
	maxTurns        int
	konturSSHUser   string
	konturExecKey   string
	konturWorkspace string
	githubDataDir   string
	githubHost      string
	githubInsecure  bool
	grainServerURL  string
}

// Option configures a Framework at construction time.
type Option func(*Framework)

// WithModel overrides the model claude is asked for.
func WithModel(model string) Option {
	return func(f *Framework) { f.model = model }
}

// WithMaxTurns sets the cap on agentic turns a single Run allows before
// claude's own --max-turns cuts it off. Zero -- which is also the default
// -- means no cap: see defaultMaxTurns for why that is the right default
// and Config.MaxRunRuntime for what actually bounds a runaway run.
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

// WithGitHubAccess is what lets a run read CI for its own branch: the
// forked "mcpserver" subprocess is given -data-dir, -github-host and
// -github-insecure-http so it can build a GitHub REST client from this
// controller's own secrets/github ladder and answer pull_request_status
// (see cmd/grain/mcpserver.go, and pkg/mcp/pullrequest_tools.go for why
// this does not put GitHub inside the sandbox).
//
// dataDir is the deployment-wide half; which repo and branch that
// process may then ask about comes per-run off agent.RunConfig, never
// from here. A Framework built without this option passes none of the
// three, and the tool answers that it has nothing to report -- so a
// deployment with no GitHub credential still runs, it just cannot see
// its own CI.
func WithGitHubAccess(dataDir, host string, insecureHTTP bool) Option {
	return func(f *Framework) {
		f.githubDataDir, f.githubHost, f.githubInsecure = dataDir, host, insecureHTTP
	}
}

// WithGrainServer names the running "grain daemon"'s own UI/API base URL
// (e.g. "http://127.0.0.1:8420") for the forked "mcpserver" subprocess to
// reach it at -- which, together with a RunConfig.TaskID, is what gives a
// run the open_pull_request tool. That tool's effect is real and
// immediate, unlike the mocked escape hatches, and unlike
// WithGitHubAccess's read-only pull_request_status it happens by asking
// the daemon rather than by the forked process holding a GitHub
// credential of its own (cmd/grain/mcpserver.go's daemonPullRequests):
// opening a pull request is a write, and writes stay grain's.
//
// Unset -- a deployment serving no UI/API, or any caller that has no
// daemon to name -- leaves the tool unregistered and every run exactly as
// it was: a pushed branch still becomes a pull request when the run
// finishes.
func WithGrainServer(url string) Option {
	return func(f *Framework) { f.grainServerURL = url }
}

// Asserted here rather than left to the one call site that type-asserts
// for it (orchestrator.frameworkOpensPullRequests): a Framework that
// stopped implementing this would otherwise not fail to compile, it
// would quietly stop telling its runs about a tool they still have.
var _ agent.PullRequestFramework = (*Framework)(nil)

// CanOpenPullRequest implements agent.PullRequestFramework: a run driven
// by this Framework is offered open_pull_request exactly when
// WithGrainServer named a daemon for its forked mcpserver to ask (see
// grainServerArgs, which passes -server/-task together or not at all).
//
// It is what lets orchestrator.BuildPrompt tell a run about the tool
// only when the run really has it -- a tool description alone reaches
// nobody who never reads the roster, and a prompt that promised it
// unconditionally would be wrong on every deployment serving no UI/API.
func (f *Framework) CanOpenPullRequest() bool { return f.grainServerURL != "" }

// New builds a Framework that runs the real claude binary at claudePath
// (typically just "claude", resolved against $PATH) and points every
// run's --mcp-config at grainBinaryPath -- the same grain binary this
// process itself is (typically the result of os.Executable()) -- invoked
// as "grainBinaryPath mcpserver -sandbox-root <root>".
func New(claudePath, grainBinaryPath string, opts ...Option) *Framework {
	return newFramework(execRunner{claudePath: claudePath}, grainBinaryPath, opts...)
}

func newFramework(run runner, grainBinaryPath string, opts ...Option) *Framework {
	f := &Framework{run: run, grainBinaryPath: grainBinaryPath, model: DefaultModel, maxTurns: defaultMaxTurns}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// allowedTools names the exact tools NewSandboxTools, NewMockTools,
// NewPullRequestTools, NewOpenPullRequestTools and
// NewRecreateSandboxTools register, mcp__-prefixed
// the way claude reports them once loaded from --mcp-config -- computed
// from those constructors
// directly rather than hand-copied, so this can never drift from what
// the "mcpserver" subcommand actually advertises the way v1's
// hand-maintained _ALLOWED_TOOLS constant could (dispatch.py).
//
// pull_request_status is named unconditionally, not only for a run that
// passed pullRequestArgs. --strict-mcp-config admits a tool only if it
// is on this list *and* the server advertises it, and mcpserver
// registers it either way (with a nil reader when unconfigured), so
// listing it always is what keeps this list a property of the tool
// vocabulary rather than of one run's configuration.
func allowedTools() []string {
	var names []string
	for _, t := range mcp.NewSandboxTools("") {
		names = append(names, mcp.QualifiedToolName(t.Name))
	}
	for _, t := range mcp.NewMockTools(&mcp.MockSink{}) {
		names = append(names, mcp.QualifiedToolName(t.Name))
	}
	for _, t := range mcp.NewPullRequestTools(nil, mcp.PullRequestScope{}) {
		names = append(names, mcp.QualifiedToolName(t.Name))
	}
	// open_pull_request is named unconditionally too, even for a run
	// whose mcpserver will not register it (no -server/-task):
	// --allowedTools is a filter over what the server actually
	// advertises, so admitting a tool that is not there costs nothing,
	// while a run that *does* have it and is not admitted would be
	// refused by --strict-mcp-config. nil is a PullRequestOpener no run
	// ever gets -- this only wants the names.
	for _, t := range mcp.NewOpenPullRequestTools(nil) {
		names = append(names, mcp.QualifiedToolName(t.Name))
	}
	// recreate_sandbox comes from the same -server/-task pair
	// open_pull_request does, so it is named on the same terms and for
	// the same reason: unconditionally, since this list filters what the
	// server advertises rather than adding to it. nil is a
	// SandboxRecreator no run ever gets -- this only wants the name.
	for _, t := range mcp.NewRecreateSandboxTools(nil) {
		names = append(names, mcp.QualifiedToolName(t.Name))
	}
	return names
}

// mcpToolTimeoutVar is claude's own cap on how long it will let a single
// MCP tool call run before it abandons it, in milliseconds.
const mcpToolTimeoutVar = "MCP_TOOL_TIMEOUT"

// mcpToolTimeoutSlack is how far past the longest wait wait_for_checks
// can do the cap above is set. Enough for the round trips either side of
// the wait itself, and no more: the point is that the tool's own clock
// runs out first, not that claude stops capping tool calls.
const mcpToolTimeoutSlack = 2 * time.Minute

// mcpToolTimeout is the environment claude needs for wait_for_checks to
// be able to finish waiting.
//
// That tool blocks for as long as CI takes, up to an hour
// (mcp.MaxWaitForChecksTimeout), which is far past claude's own default
// cap on a tool call. Leaving the cap where it is would kill the call
// part-way through a wait the agent deliberately asked for and report it
// as a tool failure -- the one answer that is neither true nor useful,
// since the build it was waiting on is still running and the run now has
// no idea how it ended. Raising it to just past the tool's own maximum
// puts the deadline back where it belongs: on grain's side, where
// running out of time produces a report saying so.
//
// An MCP_TOOL_TIMEOUT already in the environment is left alone. An
// operator who set one deliberately outranks this default, and env
// entries here are appended after os.Environ() (execRunner.Run), so
// setting it unconditionally would silently override theirs.
func mcpToolTimeout() []string {
	if _, set := os.LookupEnv(mcpToolTimeoutVar); set {
		return nil
	}
	limit := mcp.MaxWaitForChecksTimeout + mcpToolTimeoutSlack
	return []string{fmt.Sprintf("%s=%d", mcpToolTimeoutVar, limit.Milliseconds())}
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
//
// pullRequestArgs and grainServerArgs are appended to either, since
// which repo's CI a run may read, and whether it may ask grain to open
// its pull request, are both independent of which backend its sandbox
// runs on.
//
// So is agent.RunDeadlineArgs, which is why ctx is here at all: the
// deadline on the ctx this run was given is what grain will cancel it
// at, and passing it on is what lets that server's tool results tell the
// run how long it has left (see that function, and
// mcp.Registry.AnnounceDeadline).
func (f *Framework) mcpServerArgs(ctx context.Context, cfg agent.RunConfig) ([]string, error) {
	var args []string
	switch {
	case cfg.SandboxRoot != "":
		args = []string{"mcpserver", "-sandbox-root", cfg.SandboxRoot}
	case cfg.KonturVM != "":
		if f.konturSSHUser == "" || f.konturWorkspace == "" {
			return nil, fmt.Errorf("claude: RunConfig.KonturVM is set but this Framework has no kontur SSH config (see WithKonturSSH)")
		}
		args = []string{
			"mcpserver", "-kontur-vm", cfg.KonturVM,
			"-ssh-user", f.konturSSHUser, "-workspace", f.konturWorkspace,
		}
		// Omitted rather than passed empty when unset, which is the
		// normal case: "kontur run" generates a keypair for each guest it
		// boots, so `kontur exec`'s own default path already holds a key
		// that guest authorizes. An explicit key is only for a custom
		// guest image that authorizes one of its own.
		if f.konturExecKey != "" {
			args = append(args, "-exec-key", f.konturExecKey)
		}
	default:
		return nil, fmt.Errorf("claude: RunConfig.SandboxRoot or .KonturVM is required")
	}
	args = append(args, f.pullRequestArgs(cfg)...)
	args = append(args, f.grainServerArgs(cfg)...)
	return append(args, agent.RunDeadlineArgs(ctx)...), nil
}

// grainServerArgs is the "-server/-task" pair that turns on the forked
// mcpserver's open_pull_request, or nothing at all when either half is
// missing -- a Framework built without WithGrainServer, or a caller with
// no task to name. One without the other names a question with no
// address to send it to, or an address with nothing to ask about, and
// mcpserver.go rejects either half on its own.
func (f *Framework) grainServerArgs(cfg agent.RunConfig) []string {
	if f.grainServerURL == "" || cfg.TaskID == "" {
		return nil
	}
	return []string{"-server", f.grainServerURL, "-task", cfg.TaskID}
}

// pullRequestArgs is the "-data-dir/-pr-repo/-pr-branch" triple that
// turns on the forked mcpserver's pull_request_status, or nothing at all
// when either half is missing -- a Framework built without
// WithGitHubAccess, or a run whose task has no repo attached. Passing
// half of it would be worse than passing none: mcpserver would warn on
// stderr about a misconfiguration that is really just a task with no
// target.
func (f *Framework) pullRequestArgs(cfg agent.RunConfig) []string {
	if f.githubDataDir == "" || cfg.Repo == "" || cfg.Branch == "" {
		return nil
	}
	args := []string{"-data-dir", f.githubDataDir, "-pr-repo", cfg.Repo, "-pr-branch", cfg.Branch}
	if f.githubHost != "" {
		args = append(args, "-github-host", f.githubHost)
	}
	if f.githubInsecure {
		args = append(args, "-github-insecure-http")
	}
	return args
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
	mcpArgs, err := f.mcpServerArgs(ctx, cfg)
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
	}
	// Omitted entirely rather than passed as a large number when no cap
	// is configured: "unlimited" is a thing the CLI already does by
	// default, and a big number is still a cliff (see defaultMaxTurns).
	if maxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(maxTurns))
	}
	if f.model != "" {
		args = append(args, "--model", f.model)
	}
	env := mcpToolTimeout()
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
	stdout, runErr := f.run.Run(ctx, args, cfg.Prompt, env, tee)
	result, parseErr := parseTranscript(stdout)
	switch {
	case runErr != nil:
		// partialResult, not nil: a run that pushed a branch before
		// claude exited non-zero has already changed the world, and
		// agent.Framework's own contract is that the caller gets both
		// halves. Returning nil here is what used to strand that work --
		// and, because orchestrator.RunDispatch only records a
		// transcript for a non-nil Result and then removes the live
		// mirror it had been rendering, what used to make a failed run's
		// transcript vanish from the UI the moment it failed.
		return partialResult(result, stdout), runFailure(stdout, maxTurns, runErr)
	case parseErr != nil:
		// A capture with no terminal result event: claude exited 0 but
		// its output was truncated mid-stream. Same reasoning as above --
		// whatever it did get done still comes back.
		return partialResult(result, stdout), parseErr
	}
	// A run claude ended by reporting the account's own usage limit --
	// exit 0, is_error unset, the refusal delivered as the run's final
	// answer like any other. Nothing above notices that: the parse
	// succeeded and the "result" is a sentence. It is an
	// agent.UsageLimitError all the same, so that the deployment pauses
	// rather than sending the next task at the same wall -- see
	// usagelimit.go for why this path matches strictly and runFailure's
	// does not.
	//
	// result travels back alongside it, never instead of it: a run that
	// worked for an hour and met the limit on its last turn has already
	// pushed its branch.
	if limit := usageLimitFromResult(parseEvents(stdout).resultText); limit != nil {
		return result, limit
	}
	return result, nil
}

// runFailure says why a claude subprocess that exited non-zero failed,
// preferring the stream's own account to the exit status. claude reports
// a failed run as a terminal "result" event with is_error set (subtype
// error_max_turns, error_during_execution, ...) written to stdout, then
// exits 1 with nothing at all on stderr -- so runErr on its own renders
// as the useless "exit status 1 (stderr: )". The stream knows better;
// runErr is only the fallback for a subprocess that died before saying
// anything (a missing binary, a signal, a cancelled context).
func runFailure(stdout string, maxTurns int, runErr error) error {
	p := parseEvents(stdout)
	limit := usageLimitFromFailure(p.resultText, runErr)
	switch {
	case p.resultSubtype == maxTurnsSubtype && maxTurns > 0:
		// Named in plain words rather than passed through as a subtype:
		// this is not a fault of claude or of the model but a configured
		// --max-turns budget running out, and the operator reading the
		// failed run needs to be told which number to raise. Worded by
		// agent.MaxTurnsExceeded, which every Framework shares: what
		// reads that sentence afterwards is model.EndingOf, not only a
		// human.
		return agent.MaxTurnsExceeded("claude", maxTurns)
	case p.resultSubtype == maxTurnsSubtype:
		// No cap was configured, so claude hit one of its own -- naming
		// a number grain never set would send an operator looking for a
		// setting that is already unlimited.
		return agent.MaxTurnsExceededByCLI("claude")
	case limit != nil:
		// Before p.resultErr below, which would otherwise render this as
		// a generic "run ended in error (subtype=...)" and lose the one
		// thing about it a deployment can act on: this run did not fail,
		// the account it ran as has no budget left until its window
		// resets. orchestrator.RunDispatch reads the type
		// (agent.UsageLimit) and pauses dispatch rather than retrying
		// into the same refusal.
		return limit
	case p.resultErr != nil:
		return p.resultErr
	}
	return fmt.Errorf("claude: running claude: %w", runErr)
}

// partialResult is what Run hands back alongside an error: whatever the
// run managed to do before it failed.
//
// A run that produced no events at all gets a nil Result, not an empty
// one -- agent.Framework's own contract draws exactly that line ("a nil
// Result with an error means the run never started"), and callers depend
// on it: orchestrator.RunDispatch treats a non-nil Result as a run whose
// tool calls it must process, so an empty one for a run that never
// reached its sandbox would report an agent that touched it.
func partialResult(parsed *agent.Result, stdout string) *agent.Result {
	if parsed != nil {
		return parsed
	}
	p := parseEvents(stdout)
	if len(p.result.ToolCalls) == 0 && p.transcript.Len() == 0 {
		return nil
	}
	partial := p.result
	partial.Transcript = strings.TrimSpace(p.transcript.String())
	return &partial
}
