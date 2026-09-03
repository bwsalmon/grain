// Package codex implements agent.Framework by running OpenAI's Codex CLI
// -- the `codex` binary -- as a subprocess on the controller. It is the
// third implementation of the shape agent/claude settled on and
// agent/antigravity followed: build the arguments, hand the CLI a
// prompt, parse the transcript it streams back.
//
// The isolation argument is the one both of those make, and the reason
// the agent runs here rather than on the sandbox: the agent's own
// credential must never enter the untrusted execution environment, so
// codex runs on the controller and reaches the sandbox only through the
// MCP tools grain's own "mcpserver" subcommand exposes
// (cmd/grain/mcpserver.go). Nothing in a tool call itself can change
// where that lands, mirroring v1's "only place it's named" discipline
// (dispatch.py's _mcp_config_json).
//
// # Where this differs from the other two, and why it matters
//
// codex has no --mcp-config flag. Its MCP servers live in a config file
// (config.toml) inside its own config directory, which is ~/.codex
// unless CODEX_HOME names another. A per-user registration is exactly
// the wrong shape here, for the reason agent/antigravity's doc comment
// gives about agy: two runs dispatched concurrently against two
// different sandboxes would share one registration, and whichever wrote
// it last would decide where both runs' tools landed. So Run gives each
// run its own private CODEX_HOME -- a temp directory holding nothing but
// the config file this run needs -- instead of registering anything
// globally (writeConfig). That keeps the per-run sandbox binding the
// dispatch model depends on, and leaves the controller's real ~/.codex
// untouched.
//
// codex also has no --max-turns. The cap RunConfig.MaxTurns asks for is
// therefore enforced here rather than by the binary, exactly as in
// agent/antigravity: Run counts completed assistant messages as they
// stream past and cancels the subprocess once the cap is reached
// (turnCap). procgroup.Prepare is what makes that cancellation reach
// codex's own MCP child too, rather than orphaning it.
//
// And codex has no --allowedTools to empty its native roster with. What
// this does instead is deny that roster anything worth having: the
// config file this run gets sets sandbox_mode = "read-only" and
// approval_policy = "never", so codex's own shell and patch tools cannot
// write to the controller and cannot ask a human for permission to,
// while every tool that can change anything is grain's own, reached over
// MCP against the sandbox. A deployment that wants a hard guarantee
// rather than a policy should run codex against a kontur sandbox
// (RunConfig.KonturVM), where the controller's filesystem is not
// reachable from the guest at all -- the same answer agent/antigravity
// gives for the same gap.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/procgroup"
)

const (
	// DefaultModel is the model codex is asked for when a caller names
	// none. Left empty deliberately would mean "whatever the binary
	// defaults to"; naming it here keeps a deployment's runs reproducible
	// across codex CLI upgrades the same way agent/claude's and
	// agent/antigravity's own DefaultModel do. A deployment that wants
	// another one sets it in Settings (model.Config.CodexModel) rather
	// than editing this.
	DefaultModel = "gpt-5.1-codex"

	// defaultMaxTurns is the cap on agentic turns a run may take before
	// turnCap stops it, and zero means no cap at all -- the same default
	// both other frameworks carry, for the reason agent/claude's own
	// defaultMaxTurns sets out at length: a turn is one model/tool round
	// trip, the ordinary shape of a grain task spends dozens of them,
	// and a number picked here by a package that knows nothing about the
	// task does not slow a run down when it guesses wrong, it fails one
	// that was working.
	//
	// orchestrator's Config.MaxRunRuntime (two hours by default) is what
	// actually bounds a runaway run. A deployment that does want a turn
	// ceiling still sets one -- `grain settings -max-agent-turns`,
	// RunConfig.MaxTurns, or WithMaxTurns -- and turnCap enforces it.
	defaultMaxTurns = 0

	// mcpServerName is the mcp_servers key written into config.toml, and
	// therefore the server name codex reports every tool under. It lives
	// in pkg/mcp rather than here because all three frameworks have to
	// agree on it: one writes the name, the other end takes it back off
	// a reported call before recording it (transcript.go).
	mcpServerName = mcp.ToolNamespace
)

// runner is the one seam this package needs to actually invoke codex --
// narrowed to an interface so a test can drive the parsing and wiring
// logic below without a real codex binary or a live OpenAI credential.
// tee, when non-nil, receives every byte of stdout as the subprocess
// produces it, alongside the buffer this still accumulates and returns
// whole once the process exits.
type runner interface {
	Run(ctx context.Context, args []string, stdin string, env []string, dir string, tee io.Writer) (stdout string, err error)
}

type execRunner struct {
	codexPath string
}

// Run execs codex and, when tee is non-nil, mirrors its stdout into tee
// via io.MultiWriter as the subprocess produces it -- not after cmd.Run
// returns, since exec.Cmd copies from the child's stdout pipe into
// whatever Writer it is given the moment there is anything to copy. That
// live mirror is what both RunConfig.TranscriptPath and the turn cap are
// built on: one needs a run's output on disk while it is still running,
// the other needs to count turns as they happen rather than after the
// fact.
func (r execRunner) Run(ctx context.Context, args []string, stdin string, env []string, dir string, tee io.Writer) (string, error) {
	cmd := exec.CommandContext(ctx, r.codexPath, args...)
	// codex forks its own MCP child once it loads the config written by
	// writeConfig; a plain exec.CommandContext cancel only kills codex
	// itself and leaves that child (and anything run_command's own
	// `bash -c` spawned under it) running as an orphan. procgroup.Prepare
	// makes cancelling ctx kill that whole tree instead -- which is also
	// what makes the turn cap actually stop a run rather than just stop
	// reading one.
	procgroup.Prepare(cmd)
	cmd.Dir = dir
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
		// stdout is returned alongside the error rather than discarded,
		// the contract both other execRunners already settled on: a run
		// cancelled by the turn cap, or one codex failed partway
		// through, has already streamed the turns it completed, and
		// Framework.Run still owes its caller that record (see
		// agent.Framework's own contract).
		return stdout.String(), fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Framework implements agent.Framework by running `codex exec`.
type Framework struct {
	run             runner
	grainBinaryPath string
	apiKey          func(context.Context) (string, error)
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

// WithModel overrides the model codex is asked for.
func WithModel(model string) Option {
	return func(f *Framework) { f.model = model }
}

// WithMaxTurns sets the cap on agentic turns a single Run allows. Like
// agent/antigravity's option of the same name and unlike agent/claude's,
// this is not passed to the binary -- codex has no --max-turns -- but
// enforced by Run itself; see this package's own doc comment. Zero --
// which is also the default -- means no cap; see defaultMaxTurns.
func WithMaxTurns(n int) Option {
	return func(f *Framework) { f.maxTurns = n }
}

// WithAPIKey sets the OPENAI_API_KEY passed to the codex subprocess's own
// environment -- never as a command-line argument, so it never lands in
// `ps` output (the same reasoning as claude.WithOAuthToken and
// antigravity.WithAPIKey, and v1's CONTROLLER_AGENT_TOKEN_PATH before
// them).
//
// An API key rather than a ChatGPT sign-in is the credential this
// framework takes, and the private CODEX_HOME is why: a run sees only
// the config directory writeConfig built for it, so the auth file a
// `codex login` wrote into the controller's real ~/.codex is not there
// to be read. An env var is a credential a deployment can rotate from
// the UI between one run and the next, which that file is not.
func WithAPIKey(key string) Option {
	return WithAPIKeyFunc(func(context.Context) (string, error) { return key, nil })
}

// WithAPIKeyFunc is WithAPIKey for a deployment whose key is not known
// once, at construction: fn is called on every Run instead, so a key set
// (or replaced) while the daemon is running takes effect on the next run
// rather than at the next restart. cmd/grain's daemon reads it out of the
// secrets database the UI writes to, which is what makes "paste the key
// into Settings" work at all (see that package's own agentCredential).
//
// An error from fn fails the run: a key that cannot be read is not the
// same as a deployment that deliberately configured none, which stays
// what an empty string means here (Run then passes no OPENAI_API_KEY at
// all and lets the subprocess authenticate from its own ambient
// environment).
func WithAPIKeyFunc(fn func(context.Context) (string, error)) Option {
	return func(f *Framework) { f.apiKey = fn }
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

// WithGitHubAccess is agent/claude's option of the same name: it gives
// the forked "mcpserver" subprocess the -data-dir/-github-host/
// -github-insecure-http it needs to answer pull_request_status from this
// controller's own secrets/github ladder. See that package's own doc
// comment, and pkg/mcp/pullrequest_tools.go for why reading CI here does
// not put GitHub inside the sandbox.
func WithGitHubAccess(dataDir, host string, insecureHTTP bool) Option {
	return func(f *Framework) {
		f.githubDataDir, f.githubHost, f.githubInsecure = dataDir, host, insecureHTTP
	}
}

// WithGrainServer names the running "grain daemon"'s own UI/API base URL
// (e.g. "http://127.0.0.1:8420") for the forked "mcpserver" subprocess to
// reach it at -- which, together with a RunConfig.TaskID, is what gives a
// run the open_pull_request tool: agent/claude's option of the same name,
// for the same reason, since all three frameworks fork the identical
// server.
//
// Unset leaves that tool unregistered and every run exactly as it was: a
// pushed branch still becomes a pull request when the run finishes.
func WithGrainServer(url string) Option {
	return func(f *Framework) { f.grainServerURL = url }
}

// Asserted here rather than left to the one call site that type-asserts
// for it (orchestrator.frameworkOpensPullRequests): a Framework that
// stopped implementing this would otherwise not fail to compile, it
// would quietly stop telling its runs about a tool they still have.
var _ agent.PullRequestFramework = (*Framework)(nil)

// CanOpenPullRequest implements agent.PullRequestFramework, exactly as
// the other two frameworks' method of the same name does and for the
// same reason: only a Framework built WithGrainServer passes its forked
// mcpserver the -server/-task pair open_pull_request is registered on,
// so only it can tell orchestrator.BuildPrompt that the tool will be
// there.
func (f *Framework) CanOpenPullRequest() bool { return f.grainServerURL != "" }

// New builds a Framework that runs the real codex binary at codexPath
// (typically just "codex", resolved against $PATH) and points every
// run's config at grainBinaryPath -- the same grain binary this process
// itself is (typically the result of os.Executable()) -- invoked as
// "grainBinaryPath mcpserver -sandbox-root <root>".
func New(codexPath, grainBinaryPath string, opts ...Option) *Framework {
	return newFramework(execRunner{codexPath: codexPath}, grainBinaryPath, opts...)
}

func newFramework(run runner, grainBinaryPath string, opts ...Option) *Framework {
	f := &Framework{run: run, grainBinaryPath: grainBinaryPath, model: DefaultModel, maxTurns: defaultMaxTurns}
	for _, opt := range opts {
		opt(f)
	}
	return f
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
func (f *Framework) mcpServerArgs(cfg agent.RunConfig) ([]string, error) {
	var args []string
	switch {
	case cfg.SandboxRoot != "":
		args = []string{"mcpserver", "-sandbox-root", cfg.SandboxRoot}
	case cfg.KonturVM != "":
		if f.konturSSHUser == "" || f.konturWorkspace == "" {
			return nil, fmt.Errorf("codex: RunConfig.KonturVM is set but this Framework has no kontur SSH config (see WithKonturSSH)")
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
		return nil, fmt.Errorf("codex: RunConfig.SandboxRoot or .KonturVM is required")
	}
	args = append(args, f.pullRequestArgs(cfg)...)
	return append(args, f.grainServerArgs(cfg)...), nil
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

// configFileName is the file codex reads its configuration from inside
// CODEX_HOME. Named once, here, because it and configTOML are the whole
// of this package's dependence on codex's own on-disk layout rather than
// on its command line or its output: if a future codex moves or renames
// the file, this constant and that function are the change.
const configFileName = "config.toml"

// configTOML is the content of that file: this run's own MCP server, and
// the two policies that keep codex's *native* tools from being a way
// around it.
//
// The MCP half is grainBinaryPath spawned with mcpArgs (built by
// mcpServerArgs above) -- the "mcpserver" argument selects the same
// subcommand mcpserver.go implements, so codex forking this exact
// command is what actually starts an MCP server, rather than needing a
// separately built binary on disk.
//
// The policy half is sandbox_mode = "read-only" and approval_policy =
// "never": codex's own shell and patch tools then cannot write anywhere
// on the controller, and cannot stop the run to ask a human for
// permission to (there is no human at a terminal here -- an unattended
// run that blocked on a prompt would burn its whole runtime waiting).
// Everything a run is meant to change is in the sandbox, and the only
// route there is the MCP server above.
//
// features.code_mode_host = false is the third line, and it is about
// the record rather than about permissions. codex's code mode has the
// model reach its MCP tools from code it writes and runs, rather than
// as tool calls of its own -- and mcp_tool_call events are exactly what
// transcript.go builds Result.ToolCalls from, which is what
// orchestrator.ProcessResult reads to find that a run asked a question
// or left a closing comment (agent.ToolCall.Name's own doc comment).
// The deployment image does not carry the separate host binary that
// mode needs, so it is already off; saying so here makes it a decision
// rather than a property of how the image was built, and codex reports
// it as "disabled" rather than "not installed" in the run's own first
// event.
//
// TOML rather than JSON is codex's own choice of format. The values
// written are all strings and lists of strings, whose TOML basic-string
// and array spellings are exactly JSON's, so encoding/json is what
// escapes them rather than a hand-rolled quoter this package would then
// own -- see tomlString/tomlStringArray.
func configTOML(grainBinaryPath string, mcpArgs []string) ([]byte, error) {
	command, err := tomlString(grainBinaryPath)
	if err != nil {
		return nil, err
	}
	args, err := tomlStringArray(mcpArgs)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("# Written by grain for one run; see pkg/agent/codex.\n")
	b.WriteString("approval_policy = \"never\"\n")
	b.WriteString("sandbox_mode = \"read-only\"\n\n")
	b.WriteString("[features]\n")
	b.WriteString("code_mode_host = false\n\n")
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", mcpServerName)
	fmt.Fprintf(&b, "command = %s\n", command)
	fmt.Fprintf(&b, "args = %s\n", args)
	return []byte(b.String()), nil
}

// tomlString renders one Go string as a TOML basic string. A TOML basic
// string is a JSON string -- same quotes, same backslash escapes, same
// \uXXXX form for anything encoding/json decides to escape (a control
// character, and also "<", ">" and "&", which it escapes by default and
// TOML reads back identically) -- so this is encoding/json rather than a
// quoter of this package's own, which would be one more thing here to
// get subtly wrong for a path with a quote or a backslash in it.
func tomlString(s string) (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("codex: encoding %q for config.toml: %w", s, err)
	}
	return string(data), nil
}

// tomlStringArray is tomlString for a list, TOML's inline array of basic
// strings being spelled exactly as JSON's array of strings. nil renders
// as "[]" rather than as JSON's "null", which TOML has no word for.
func tomlStringArray(items []string) (string, error) {
	if items == nil {
		items = []string{}
	}
	data, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("codex: encoding %v for config.toml: %w", items, err)
	}
	return string(data), nil
}

// writeConfig builds the private CODEX_HOME one run gets: a fresh
// directory containing nothing but the config file naming this run's own
// MCP server. It returns that directory and a cleanup func the caller
// defers.
//
// A private CODEX_HOME rather than `codex mcp add` is the whole point --
// see this package's own doc comment on why a per-user registration
// cannot express a per-run sandbox binding. It has the same effect
// claude's --strict-mcp-config has there: the only MCP server this run
// can see is the one written here, because there is no other
// configuration in the config directory it was given to find one in.
//
// A temp directory is where it lands, so codex opens every run with a
// warning on stderr that it will not create its PATH aliases under one.
// That is a convenience it declines rather than a failure -- it says
// "proceeding" and does -- and the alternative, a config directory
// somewhere permanent, would be the shared registration this exists to
// avoid.
func writeConfig(grainBinaryPath string, mcpArgs []string) (home string, cleanup func(), err error) {
	config, err := configTOML(grainBinaryPath, mcpArgs)
	if err != nil {
		return "", nil, err
	}
	home, err = os.MkdirTemp("", "grain-codex-home-*")
	if err != nil {
		return "", nil, fmt.Errorf("codex: creating codex home: %w", err)
	}
	cleanup = func() { os.RemoveAll(home) }
	if err := os.WriteFile(filepath.Join(home, configFileName), config, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("codex: writing codex config: %w", err)
	}
	return home, cleanup, nil
}

// Run implements agent.Framework: it writes this run's own config into a
// private CODEX_HOME pointing at grainBinaryPath and cfg.SandboxRoot or
// cfg.KonturVM (mcpServerArgs), runs `codex exec` with the prompt on
// stdin, and parses the resulting JSON event stream back into a Result.
//
// The prompt travels over stdin -- what the bare "-" argument asks codex
// for -- and never as argv: untrusted issue content must never become a
// shell- or ps-visible argument (dispatch.py's own docstring makes the
// identical point about its stdin-redirect path, and both other
// frameworks pass their prompt the same way).
//
// Run may return a non-nil Result together with a non-nil error, and a
// caller must read both. A run that edits files, commits, pushes and
// only then trips the turn cap has already changed the world, and a
// caller that treats the error as "no result" strands that work -- the
// failure that taught this is recorded in agent.Framework's own
// contract, and it is why a cancelled or failed subprocess's partial
// stdout is still parsed here rather than discarded.
//
// When cfg.TranscriptPath is set, the raw event stream is mirrored there
// live, one line per event, as codex emits them -- the file this
// package's own LiveTranscriptDir reads back with PartialTranscript to
// render a still-running run's transcript-in-progress. It is opened
// O_TRUNC, not O_APPEND: a path is only ever reused across runs by a
// caller passing the same run ID twice, which should never happen, and
// starting clean is what makes a stale previous run's bytes never a way
// to misread this one's. The file is left on disk once Run returns
// either way -- cleaning it up is the caller's job, the same way
// orchestrator.RunDispatch owns cfg.SandboxRoot's own lifecycle rather
// than this package.
//
// cfg.Addenda is not polled, for the reason RunConfig.Addenda's own doc
// comment gives about both other frameworks: this is one blocking
// subprocess call with the whole prompt written before the process
// starts, so there is no turn boundary here to poll at. A comment posted
// while a run is in flight waits for the task's next dispatch instead.
func (f *Framework) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	mcpArgs, err := f.mcpServerArgs(cfg)
	if err != nil {
		return nil, err
	}
	if f.grainBinaryPath == "" {
		return nil, fmt.Errorf("codex: grainBinaryPath is required")
	}
	maxTurns := f.maxTurns
	if cfg.MaxTurns > 0 {
		maxTurns = cfg.MaxTurns
	}

	home, cleanup, err := writeConfig(f.grainBinaryPath, mcpArgs)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var sinks []io.Writer
	if cfg.TranscriptPath != "" {
		transcriptFile, err := os.OpenFile(cfg.TranscriptPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("codex: opening transcript file: %w", err)
		}
		defer transcriptFile.Close()
		sinks = append(sinks, transcriptFile)
	}

	// The turn cap watches the same live stream the transcript mirror
	// does and cancels this run's own context once the cap is reached --
	// codex having no --max-turns to enforce it in the binary (see this
	// package's doc comment).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	capWatch := &turnCap{max: maxTurns, cancel: cancel}
	sinks = append(sinks, capWatch)

	args := []string{
		"exec",
		// The machine-readable event stream everything below parses.
		// Without it codex prints a human-formatted log, which carries
		// the same story in a shape no parser should be reading.
		"--json",
		// A sandbox's working tree is a git repository, but a kontur run
		// has no local directory at all and leaves codex started in
		// whatever this daemon's own working directory is. codex exec
		// otherwise refuses to run outside a repository -- a guard for
		// somebody running it by hand in their home directory, and not
		// one that means anything here, where what a run may touch is
		// decided by the MCP server and the sandbox policy rather than
		// by what is under the cwd.
		"--skip-git-repo-check",
	}
	if f.model != "" {
		args = append(args, "--model", f.model)
	}
	// "-" is codex's own "read the prompt from stdin". It goes last
	// because it is the positional prompt argument, not a flag.
	args = append(args, "-")

	env := []string{"CODEX_HOME=" + home}
	if f.apiKey != nil {
		apiKey, err := f.apiKey(ctx)
		if err != nil {
			return nil, fmt.Errorf("codex: reading the API key: %w", err)
		}
		if apiKey != "" {
			env = append(env, "OPENAI_API_KEY="+apiKey)
		}
	}

	stdout, runErr := f.run.Run(runCtx, args, cfg.Prompt, env, cfg.SandboxRoot, io.MultiWriter(sinks...))
	result, parseErr := parseTranscript(stdout)
	// Read once, from the stream's own account of the failure plus
	// whatever the subprocess itself reported, because a quota refusal
	// can arrive either way: codex usually reports it as a failed turn
	// (parseErr, below) but a hard enough refusal kills the process
	// first (runErr). See usagelimit.go.
	limit := usageLimitFailure(parseEvents(stdout).failureText(), runErr)
	switch {
	case capWatch.tripped():
		// The cap cancelled the subprocess, so runErr is that
		// cancellation rather than a fault of codex's -- report the cap.
		// result is whatever the run managed before it was stopped, and
		// is returned alongside the error, never instead of it.
		return partialResult(result, stdout),
			fmt.Errorf("codex: exceeded max turns (%d) without a final answer", maxTurns)
	case limit != nil:
		// Ahead of both branches below, which would otherwise render
		// this as "running codex: exit status 1" and lose the one thing
		// about it a deployment can act on: nothing is wrong with this
		// run, the credential it used has no budget left until its
		// window resets. orchestrator.RunDispatch reads the type
		// (agent.UsageLimit) and pauses dispatch rather than sending the
		// next task at the same refusal.
		//
		// result travels back alongside it, as everywhere else here: a
		// run that pushed a branch and then ran out of quota has already
		// changed the world.
		return partialResult(result, stdout), limit
	case runErr != nil:
		return partialResult(result, stdout), fmt.Errorf("codex: running codex: %w", runErr)
	case parseErr != nil:
		// A capture with no terminal event -- a codex that died without
		// runErr reaching us, or output truncated mid-stream.
		// partialResult, not result: parseTranscript hands back nothing
		// in that case, and a run that pushed a branch before the stream
		// stopped must still come back with the push.
		return partialResult(result, stdout), parseErr
	}
	return result, nil
}

// partialResult is what Run hands back alongside an error: whatever the
// run managed to do before it failed or was stopped.
//
// A run that produced no events at all gets a nil Result, not an empty
// one -- agent.Framework's own contract draws exactly that line ("a nil
// Result with an error means the run never started"), and callers depend
// on it: orchestrator.RunDispatch treats a non-nil Result as a run whose
// tool calls it must process, so an empty one for a run cancelled before
// codex ever spoke would report an agent that touched a sandbox it never
// reached.
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
