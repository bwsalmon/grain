// Package antigravity implements agent.Framework by running Google's
// Antigravity CLI -- the `agy` binary that replaced Gemini CLI -- as a
// subprocess on the controller. It is the replacement for this repo's own
// home-grown Gemini runtime (the former pkg/agent/gemini), which drove the
// Gemini API's function calling directly and looped tool calls in-process
// against its own pkg/mcp registry. Everything that runtime hand-rolled --
// the turn loop, the function-declaration translation, the tool dispatch --
// is agy's job now; what is left here is the same shape agent/claude
// already settled on for a real CLI: build the arguments, hand it a
// prompt, parse the transcript it streams back.
//
// The isolation argument is the same one agent/claude makes, and the
// reason the agent runs here rather than on the sandbox: the agent's own
// credential must never enter the untrusted execution environment, so agy
// runs on the controller and reaches the sandbox only through the MCP
// tools grain's own "mcpserver" subcommand exposes (cmd/grain/
// mcpserver.go). Nothing in a tool call itself can change where that
// lands, mirroring v1's "only place it's named" discipline (dispatch.py's
// _mcp_config_json).
//
// # Where this differs from agent/claude, and why it matters
//
// agy has no --mcp-config flag. Its MCP servers live in a per-user config
// file that `agy mcp add` edits (~/.gemini/config/mcp_config.json), and
// its tool manifests are cached under
// ~/.gemini/antigravity-cli/mcp/<server>/. A per-user registration is
// exactly the wrong shape here: two runs dispatched concurrently against
// two different sandboxes would share one registration, and whichever
// wrote it last would decide where both runs' tools landed. So Run gives
// each run its own private HOME (a temp directory holding just the two
// files agy reads out of one) instead of registering anything globally --
// see writeAgyHome. That keeps the per-run sandbox binding the dispatch
// model depends on, and leaves the controller's real ~/.gemini untouched.
//
// A private HOME also decides how a run authenticates. agy's own
// credential is an OAuth session it stores under the HOME it was started
// with, which a run given a fresh directory by definition does not have;
// what it does have is the deployment's Gemini API key. agy reads that
// key from GEMINI_API_KEY, but only for a session whose settings file
// asks for it -- with no "modelProvider": "gemini" in
// ~/.gemini/antigravity-cli/settings.json it ignores the variable
// entirely, falls through to its interactive browser login, and (with a
// prompt on stdin rather than a terminal) exits 1 with "authentication
// required. Run 'agy' to log in, then retry." So writeAgyHome writes that
// setting alongside the MCP config whenever grain has a key to pass.
//
// A private HOME is also where this package tells agy how to put grain's
// tools in front of the model, which it does not do by default. agy loads
// an MCP server's tools lazily: they appear in no roster, and the only
// way to one is a native dispatcher, call_mcp_tool. A run configured that
// way reaches for agy's own 57 native tools instead -- which execute
// wherever agy does, on the controller -- and every call grain does see
// comes back named after the dispatcher rather than the tool. So the MCP
// config asks for every published tool with "eager": true, which
// registers each as a tool of agy's own under the name
// mcp.AgyQualifiedToolName spells (see eagerToolsConfig, and
// transcript.go for the two names a call can arrive under).
//
// agy caps a whole print-mode run at five minutes unless told otherwise,
// which ends a grain-shaped run long before it can push anything. Run
// passes --print-timeout derived from the deadline on its own context, so
// that grain's cancellation is always what stops a run; see printTimeout.
//
// agy also has no --max-turns. The cap RunConfig.MaxTurns asks for is
// therefore enforced here rather than by the binary: Run counts completed
// agent_response steps as they stream past and cancels the subprocess
// once the cap is reached (turnCap). procgroup.Prepare is what makes that
// cancellation reach agy's own MCP child too, rather than orphaning it.
//
// agy does cap a single MCP tool call, and there is no environment
// variable for it the way MCP_TOOL_TIMEOUT is for claude: the cap is a
// per-server "timeoutSeconds" key in the MCP config above, which is why
// mcpToolTimeoutSeconds writes one rather than Run adding to the
// subprocess environment. See that function for the measured default and
// why wait_for_checks makes leaving it alone the wrong answer.
package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/capability/bootstrap"
	"github.com/bwsalmon/grain/pkg/capability/selfdebug"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/procgroup"
)

const (
	// DefaultModel is the model agy is asked for when a caller names
	// none. Left empty deliberately would mean "whatever the binary
	// defaults to"; naming it here keeps a deployment's runs reproducible
	// across agy upgrades the same way the former gemini package's own
	// DefaultModel did.
	//
	// It is the bare family name, with DefaultEffort below carrying the
	// reasoning effort as agy's own --effort. A model selection is two
	// values to agy, not one, and it accepts them written either way --
	// measured against agy 1.1.26 with a live key, every line of this
	// checked by running it:
	//
	//	--model gemini-3.1-pro                 refused, "--model
	//	                                       gemini-3.1-pro requires
	//	                                       --effort (available: low,
	//	                                       high)"
	//	--model gemini-3.1-pro --effort high   runs
	//	--model gemini-3.1-pro-high            runs
	//	--model gemini-3.1-pro-high
	//	                       --effort high   runs
	//	--model gemini-3.1-pro-high
	//	                       --effort low    refused, "--model
	//	                                       gemini-3.1-pro-high
	//	                                       conflicts with
	//	                                       --effort=low"
	//	--model gemini-3.1-pro --effort medium refused, "gemini-3.1-pro
	//	                                       has no \"medium\" effort"
	//
	// So the two forms name the same run, and the pair is the one that
	// lets a deployment change the effort without touching the model.
	// `agy models` still lists the suffixed spelling
	// (gemini-3.1-pro-high, gemini-3.8-flash-medium), which is what a
	// deployment that named a model before this existed has stored, so
	// Run drops --effort for a model whose name already carries one --
	// see modelNamesItsEffort.
	DefaultModel = "gemini-3.1-pro"

	// DefaultEffort is the reasoning effort agy is asked for when a
	// caller names none: the depth half of the model selection described
	// above, passed as --effort.
	//
	// agy takes low, medium or high (`agy --help`), but which of them a
	// given model has is the model's own business -- Gemini 3.1 Pro
	// offers low and high and refuses medium, the Flash models offer all
	// three -- so a deployment changing one of these two settings
	// (`grain settings -gemini-effort`, Settings -> Agent frameworks)
	// wants to know what the other one is. high, rather than agy's own
	// default, for the reason DefaultModel is named at all: a run's depth
	// should not change under a deployment because the binary was
	// upgraded.
	DefaultEffort = "high"

	// defaultMaxTurns is the cap on agentic turns a run may take before
	// turnCap stops it, and zero means no cap at all -- the same default
	// agent/claude carries, and for the same reason: a turn is one
	// model/tool round trip, the ordinary shape of a grain task spends
	// dozens of them, and a number picked here by a package that knows
	// nothing about the task does not slow a run down when it guesses
	// wrong, it fails one that was working.
	//
	// orchestrator's Config.MaxRunRuntime (two hours by default) is what
	// actually bounds a runaway run. A deployment that does want a turn
	// ceiling still sets one -- `grain settings -max-agent-turns`,
	// RunConfig.MaxTurns, or WithMaxTurns -- and turnCap enforces it
	// exactly as before; it already reads a non-positive max as "no cap"
	// (turnCap.Write), so nothing else here changes.
	defaultMaxTurns = 0

	// mcpServerName is the key written into agy's MCP settings, and
	// therefore the prefix agy reports every tool under
	// ("mcp__<name>__<tool>"). It lives in pkg/mcp rather than here
	// because both halves of that convention -- writing the prefix onto
	// the allowed-tools list, and taking it back off a reported call
	// before recording it (transcript.go) -- have to agree, and
	// agent/claude needs the identical pair for its own CLI.
	mcpServerName = mcp.ToolNamespace
)

// runner is the one seam this package needs to actually invoke agy --
// narrowed to an interface so a test can drive the parsing and wiring
// logic below without a real agy binary or a live credential (see
// testing.go, whose fake speaks the same stream-json protocol and runs
// the scripted tool calls for real). tee, when non-nil, receives every
// byte of stdout as the subprocess produces it, alongside the buffer this
// still accumulates and returns whole once the process exits.
type runner interface {
	Run(ctx context.Context, args []string, stdin string, env []string, dir string, tee io.Writer) (stdout string, err error)
}

type execRunner struct {
	agyPath string
}

// Run execs agy and, when tee is non-nil, mirrors its stdout into tee via
// io.MultiWriter as the subprocess produces it -- not after cmd.Run
// returns, since exec.Cmd copies from the child's stdout pipe into
// whatever Writer it is given the moment there is anything to copy. That
// live mirror is what both RunConfig.TranscriptPath and the turn cap are
// built on: one needs a run's output on disk while it is still running,
// the other needs to count steps as they happen rather than after the
// fact.
func (r execRunner) Run(ctx context.Context, args []string, stdin string, env []string, dir string, tee io.Writer) (string, error) {
	cmd := exec.CommandContext(ctx, r.agyPath, args...)
	// agy forks its own MCP child once it loads the settings written by
	// writeAgyHome; a plain exec.CommandContext cancel only kills agy
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
		// stdout is returned alongside the error rather than discarded:
		// a run cancelled by the turn cap, or one agy failed partway
		// through, has already streamed the steps it completed, and
		// Framework.Run still owes its caller that record (see
		// agent.Framework's own contract).
		return stdout.String(), fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Framework implements agent.Framework by running `agy`.
type Framework struct {
	run             runner
	grainBinaryPath string
	apiKey          func(context.Context) (string, error)
	model           string
	effort          string
	maxTurns        int
	konturSSHUser   string
	konturExecKey   string
	konturWorkspace string
	githubDataDir   string
	githubHost      string
	githubInsecure  bool
	grainServerURL  string

	// Set only by WithoutPermissionOverrideForTest, and only by the live
	// probe that measures a run in agy's own permission mode. Every other
	// Framework leaves it false, which is Run passing
	// --dangerously-skip-permissions exactly as it always has.
	withoutPermissionOverride bool
}

// Option configures a Framework at construction time.
type Option func(*Framework)

// WithModel overrides the model agy is asked for.
func WithModel(model string) Option {
	return func(f *Framework) { f.model = model }
}

// WithEffort overrides the reasoning effort agy is asked for -- the
// other half of a model selection (DefaultModel and DefaultEffort have
// the two forms agy accepts and how they were measured). Empty passes no
// --effort at all, leaving whichever effort the model name itself names,
// and a model name that already names one wins over this either way,
// since agy refuses the pair when they disagree.
func WithEffort(effort string) Option {
	return func(f *Framework) { f.effort = effort }
}

// WithMaxTurns sets the cap on agentic turns a single Run allows. Unlike
// agent/claude's option of the same name, this is not passed to the
// binary -- agy has no --max-turns -- but enforced by Run itself; see
// this package's own doc comment. Zero -- which is also the default --
// means no cap; see defaultMaxTurns.
func WithMaxTurns(n int) Option {
	return func(f *Framework) { f.maxTurns = n }
}

// WithAPIKey sets the GEMINI_API_KEY passed to the agy subprocess's own
// environment -- never as a command-line argument, so it never lands in
// `ps` output (the same reasoning as claude.WithOAuthToken, and v1's
// CONTROLLER_AGENT_TOKEN_PATH before it). This is also what makes the
// private HOME writeAgyHome hands agy workable: a run authenticating by
// env var needs nothing out of the real ~/.gemini it no longer sees --
// but only together with the settings file writeAgyHome writes for it,
// since agy ignores the variable unless it is asked to use it.
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
// what an empty string means here (Run then passes no GEMINI_API_KEY at
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
// for the same reason, since both frameworks fork the identical server.
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
// agent/claude's method of the same name does and for the same reason:
// only a Framework built WithGrainServer passes its forked mcpserver the
// -server/-task pair open_pull_request is registered on, so only it can
// tell orchestrator.BuildPrompt that the tool will be there.
func (f *Framework) CanOpenPullRequest() bool { return f.grainServerURL != "" }

// New builds a Framework that runs the real agy binary at agyPath
// (typically just "agy", resolved against $PATH) and points every run's
// MCP settings at grainBinaryPath -- the same grain binary this process
// itself is (typically the result of os.Executable()) -- invoked as
// "grainBinaryPath mcpserver -sandbox-root <root>".
func New(agyPath, grainBinaryPath string, opts ...Option) *Framework {
	return newFramework(execRunner{agyPath: agyPath}, grainBinaryPath, opts...)
}

func newFramework(run runner, grainBinaryPath string, opts ...Option) *Framework {
	f := &Framework{
		run:             run,
		grainBinaryPath: grainBinaryPath,
		model:           DefaultModel,
		effort:          DefaultEffort,
		maxTurns:        defaultMaxTurns,
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Efforts is the reasoning efforts agy's --effort accepts, in the order
// its own help text lists them ("Reasoning effort for the current CLI
// session (low|medium|high)"). Whether a given model offers all three is
// the model's own business and only agy knows it -- Gemini 3.1 Pro
// refuses medium -- so this is what can be checked without the binary,
// and it is what ui.UpdateSettings validates a saved effort against
// rather than letting a typo reach the run that fails on it.
func Efforts() []string { return []string{"low", "medium", "high"} }

// modelNamesItsEffort reports whether model is one of the catalog names
// that spells its reasoning effort into the model name itself
// (gemini-3.1-pro-high, gemini-3.8-flash-medium -- the only spelling
// `agy models` prints). Run passes no --effort alongside one of those;
// DefaultModel has why.
func modelNamesItsEffort(model string) bool {
	for _, effort := range Efforts() {
		if strings.HasSuffix(model, "-"+effort) {
			return true
		}
	}
	return false
}

// publishedTools names the exact tools NewSandboxTools, NewMockTools,
// NewPullRequestTools, NewOpenPullRequestTools, NewRecreateSandboxTools
// and NewStatusTools register, plus selfdebug.SourceTools' and
// bootstrap.PlaybookTools' -- bare, as the "mcpserver" subcommand
// registers them. Computed from those
// constructors directly rather than hand-copied, so this can never drift
// from what that subcommand actually advertises the way v1's
// hand-maintained _ALLOWED_TOOLS constant could (dispatch.py).
//
// It is what eagerToolsConfig asks agy to register eagerly, so the list
// has to name every tool a run might have: a tool left out of it is not
// merely unlisted, it is one the model can only reach through the
// call_mcp_tool fallback.
//
// pull_request_status is named unconditionally rather than only for a
// run that passed pullRequestArgs, for the reason agent/claude's own
// allowedTools gives: this list is a property of the tool vocabulary,
// not of one run's configuration, and mcpserver registers that tool
// either way. Naming a tool this run's own mcpserver will not register
// costs nothing -- agy only ever registers the intersection of this list
// with what the server actually offers.
func publishedTools() []string {
	var names []string
	for _, t := range mcp.NewSandboxTools("") {
		names = append(names, t.Name)
	}
	for _, t := range mcp.NewMockTools(&mcp.MockSink{}) {
		names = append(names, t.Name)
	}
	for _, t := range mcp.NewPullRequestTools(nil, mcp.PullRequestScope{}) {
		names = append(names, t.Name)
	}
	// open_pull_request is named unconditionally too, even for a run
	// whose mcpserver will not register it (no -server/-task). nil is a
	// PullRequestOpener no run ever gets -- this only wants the names.
	for _, t := range mcp.NewOpenPullRequestTools(nil) {
		names = append(names, t.Name)
	}
	// recreate_sandbox is registered by the same -server/-task pair
	// open_pull_request is, so it is named here on the same terms. nil
	// is a SandboxRecreator no run ever gets -- this only wants the name.
	for _, t := range mcp.NewRecreateSandboxTools(nil) {
		names = append(names, t.Name)
	}
	// update_status comes from that same pair, and is named here on those
	// same terms. nil is a StatusReporter no run ever gets -- this only
	// wants the name.
	for _, t := range mcp.NewStatusTools(nil) {
		names = append(names, t.Name)
	}
	// The capability grants' own tools, named on the same terms again:
	// mcpserver registers each set only for a run whose task holds that
	// grant (-grant). "" is a source directory no run ever gets -- this
	// only wants the names.
	for _, t := range selfdebug.SourceTools("") {
		names = append(names, t.Name)
	}
	for _, t := range bootstrap.PlaybookTools() {
		names = append(names, t.Name)
	}
	return names
}

// eagerToolNames is publishedTools spelled the way agy reports a call to
// one of them once it has registered them eagerly -- what a transcript
// reader and the roster check below both have to recognize.
func eagerToolNames() []string {
	names := publishedTools()
	for i, name := range names {
		names[i] = mcp.AgyQualifiedToolName(name)
	}
	return names
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
// runs on. So is agent.GrantArgs, which passes on the tool-granting
// capabilities this run's task holds -- and so whether that server
// serves the read-only tools for grain's own source (self-debug) or for
// grain's own bootstrap playbooks (bootstrap-playbooks).
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
			return nil, fmt.Errorf("antigravity: RunConfig.KonturVM is set but this Framework has no kontur SSH config (see WithKonturSSH)")
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
		return nil, fmt.Errorf("antigravity: RunConfig.SandboxRoot or .KonturVM is required")
	}
	args = append(args, f.pullRequestArgs(cfg)...)
	args = append(args, f.grainServerArgs(cfg)...)
	args = append(args, agent.GrantArgs(cfg)...)
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

// mcpToolTimeoutSlack is how far past the longest wait wait_for_checks
// can do the per-server cap below is set. Enough for the round trips
// either side of the wait itself, and no more: the point is that the
// tool's own clock runs out first, not that agy stops capping tool calls.
const mcpToolTimeoutSlack = 2 * time.Minute

// mcpToolTimeoutSeconds is the "timeoutSeconds" this run's MCP server
// entry carries, and it is agent/claude's mcpToolTimeout in the one shape
// agy offers: a key in the config file rather than an environment
// variable, since agy reads no MCP_TOOL_TIMEOUT of its own.
//
// agy caps a single MCP tool call at three minutes when the key is
// absent, takes a positive value as that many seconds, and treats a
// negative one as no cap at all (measured against agy 1.1.25's own
// mcpcore.(*McpClientSessionInstance).callToolTimeout, which is also
// where "MCP tool call to server %q timed out after %s" comes from).
// Three minutes is far short of wait_for_checks, which blocks for as long
// as CI takes, up to an hour (mcp.MaxWaitForChecksTimeout). Leaving the
// default in place would kill the call part-way through a wait the agent
// deliberately asked for and report it as a tool failure -- the one
// answer that is neither true nor useful, since the build it was waiting
// on is still running and the run now has no idea how it ended. Raising
// it to just past the tool's own maximum puts the deadline back where it
// belongs: on grain's side, where running out of time produces a report
// saying so.
//
// A finite value rather than the negative "never time out" agy also
// accepts, for the reason the slack above gives: an MCP server that has
// genuinely wedged should still end the call eventually.
func mcpToolTimeoutSeconds() int {
	return int((mcp.MaxWaitForChecksTimeout + mcpToolTimeoutSlack).Seconds())
}

// printTimeoutSlack is how far past grain's own deadline for this run the
// --print-timeout below is set: enough that agy is never the thing that
// ends a run, and grain's own cancellation always gets there first.
const printTimeoutSlack = 10 * time.Minute

// defaultPrintTimeout is the cap for a run whose context carries no
// deadline at all -- a test, or a caller that means a run to take as long
// as it takes. Long rather than absent because agy's flag has no "never"
// value, and a day is past any run grain would leave in flight.
const defaultPrintTimeout = 24 * time.Hour

// printTimeout is the value of agy's --print-timeout for this run, and
// passing it at all is a bug fix rather than a tuning knob.
//
// agy's print mode caps a whole non-interactive run at five minutes by
// default ("--print-timeout ... (default 5m0s)"). It is a wall-clock cap
// on the run, not on a single model call: reaching it kills the run
// mid-tool-call and emits a terminal result event of
//
//	{"status":"ERROR","error":"timeout waiting for response"}
//
// with exit status 1. grain never passed the flag, so every dispatched
// antigravity run died five minutes in -- long enough to clone, read
// around and start editing, and far short of pushing anything -- and
// surfaced as this package's generic "run ended in status ERROR". The
// ordinary shape of a grain task is dozens of turns over tens of minutes;
// orchestrator.Config.MaxRunRuntime (two hours by default) is what is
// meant to bound it.
//
// So the cap is put back where the rest of this package already puts a
// deadline: on grain's side. The value tracks the deadline on the run's
// own context -- the moment grain will cancel it -- plus enough slack
// that agy's clock never runs out first. A run stopped by grain reports
// what stopped it; a run stopped by agy reports only that something timed
// out waiting.
func printTimeout(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return defaultPrintTimeout
	}
	// A deadline already past (or nearly) still gets the slack rather
	// than a zero or negative duration, which agy would reject: the run
	// is about to be cancelled either way, and by grain.
	return time.Until(deadline) + printTimeoutSlack
}

// eagerToolsConfig is the per-server "tools" map that decides how agy
// puts grain's tools in front of the model, and it is not an
// optimization: without it a run effectively has none of them.
//
// agy loads an MCP server's tools *lazily* by default. Lazily loaded
// tools are not tools as far as the model is concerned -- they are
// manifest files under ~/.gemini/antigravity-cli/mcp/<server>/, reachable
// only by way of one native dispatcher, call_mcp_tool, taking
// {"ServerName", "ToolName", "Arguments"}. Measured against agy 1.1.25
// that has two consequences, and both of them break a grain run:
//
//   - The model prefers agy's own 57 native tools, which it can see. Told
//     to run a command it calls agy's *native* run_command, which executes
//     on the controller rather than through grain's mcpserver -- for a
//     kontur-VM run, a machine the run was never meant to touch, and one
//     where the work it does lands nowhere.
//   - What grain records is the dispatcher. Every call comes back named
//     "call_mcp_tool", so orchestrator.ProcessResult -- which matches
//     ToolCall.Name against "ask_question", "comment_on_issue" and
//     "propose_task" exactly -- sees none of them, and a run that asked a
//     question or left a closing note is recorded as having done nothing.
//     (transcript.go unwraps that shape as well, since a model may still
//     take the dispatcher route, but the whole point here is that it
//     should not have to.)
//
// "eager": true per tool registers each one as a native tool of agy's
// own, named mcp_<server>_<tool> (mcp.AgyQualifiedToolName). The model
// then sees grain's tools beside agy's, and calls them by name.
func eagerToolsConfig() map[string]any {
	tools := map[string]any{}
	for _, name := range publishedTools() {
		tools[name] = map[string]any{"eager": true}
	}
	return tools
}

// mcpConfigJSON is the content of the file agy reads its MCP servers
// from: grainBinaryPath spawned with mcpArgs (built by mcpServerArgs
// above) -- the "mcpserver" argument selects the same subcommand
// mcpserver.go implements, so agy forking this exact command is what
// actually starts an MCP server, rather than needing a separately built
// binary on disk. The schema is Gemini CLI's own mcpServers map, which
// agy inherited along with the ~/.gemini config directory, plus the
// per-server timeoutSeconds and tools keys agy added to it.
//
// Those two keys have to be the right JSON *type*, and getting one wrong
// is not a partial load: agy drops the whole server entry and reports "No
// MCP servers configured", with no error, no warning and nothing in the
// file to say which key did it. Measured in docs/agy-surface.md, one key
// at a time -- `tools` as a list rather than the object eagerToolsConfig
// builds drops the server, `timeoutSeconds` as a string drops the server,
// and a key agy has never heard of is ignored without harm. So the
// asymmetry to hold on to is that an unknown key here is safe and a known
// key of the wrong type costs a run every tool it has.
func mcpConfigJSON(grainBinaryPath string, mcpArgs []string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			mcpServerName: map[string]any{
				"command":        grainBinaryPath,
				"args":           mcpArgs,
				"timeoutSeconds": mcpToolTimeoutSeconds(),
				"tools":          eagerToolsConfig(),
			},
		},
	})
}

// settingsJSON is the content of agy's own settings file: the permission
// rules that tell agy which tools this run may use, plus -- for a run
// that authenticates with a Gemini API key -- the one setting that makes
// agy read GEMINI_API_KEY at all rather than expecting the OAuth session
// a private HOME cannot have (see this package's doc comment).
//
// Nothing else is written into it. agy merges what it finds with its own
// defaults, so naming only the settings this package actually depends on
// leaves every other CLI behaviour at whatever the installed binary
// chose.
func settingsJSON(apiKeyAuth bool) ([]byte, error) {
	settings := map[string]any{"permissions": permissionRules()}
	if apiKeyAuth {
		settings["modelProvider"] = apiKeyModelProvider
	}
	return json.Marshal(settings)
}

// permissionRules is the "permissions" block of that settings file: agy's
// own tools denied by name, grain's allowed by name.
//
// The rules are real and they load. Measured against agy 1.1.26 by
// writing this block into a private HOME and asking the binary what it
// read back (`agy -p /permissions`, which print mode answers without an
// agent turn), the reply is one tab-separated record per rule --
// "global<TAB>deny<TAB>run_command" -- so the key is parsed, kept, and
// carried at global scope, and a malformed value (`"deny": 12345`) is
// dropped in silence with no rule and no complaint. What they do not do
// is stop a tool, and that is now settled rather than left open:
//
//   - Nothing here changes the roster. The same probe read the `init`
//     event of a real (if unauthenticated) stream-json session with and
//     without this block, and both advertise the identical 57 native
//     tools -- as do excludeTools, permissionPreset, agentPermissions,
//     toolConfirmation and the --sandbox flag, each measured in
//     docs/agy-surface.md. A denied tool is one the model still sees and
//     still calls; the denial, if it lands at all, lands on the call.
//   - The override wins, and that is now measured rather than suspected.
//     Run passes --dangerously-skip-permissions, which puts the session in
//     agy's "always-proceed" permission mode (that same `init` event
//     reports permission_mode "always-proceed" with the flag and
//     "request-review" without it), and agy's own interactive prompt calls
//     an always-deny decision "overridden by dangerously-skip-permissions".
//     A live run holding this exact block, asked to list a directory, ran
//     agy's own list_dir -- one of the names denied below -- to completion.
//     So these rules stop nothing as grain runs agy today.
//   - Whether they could is being measured rather than argued about.
//     Dropping that flag leaves a run in agy's own request-review mode,
//     where a headless run soft-denies anything needing a confirmation --
//     so a run whose allow rules are not matched in the spelling they are
//     written in is a dispatch that can do nothing at all, grain's tools
//     being the only ones that reach a sandbox. That takes a live model to
//     settle, and tests/e2e's TestLiveRunWithoutThePermissionOverride
//     settles it nightly, driving this framework minus the flag (see
//     WithoutPermissionOverrideForTest). Until it comes back clean, Run
//     keeps the override.
//
// So hookConfigJSON is the half with teeth -- it is the one that has been
// watched stopping a live model -- and what contains a native tool that
// gets past it is still a kontur sandbox. This block is written anyway
// because it costs a run nothing, because it is the documented place to
// say what a session may do, and because a deny rule that starts being
// enforced is a deny rule already in place.
//
// The names are spelled bare (`run_command`, not `run_command(*)`), which
// is how agy's own allow rules read for a whole tool rather than one
// resource of it; both spellings survive the round trip above. Grain's
// tools are allowed by their exact eagerly registered names rather than
// by a `mcp_grain-sandbox_*` glob, because agy matches an approval rule
// strictly rather than as a pattern unless it is prefixed `regex:`, and a
// glob that matches nothing would allow nothing.
func permissionRules() map[string]any {
	var allow []string
	for _, name := range publishedTools() {
		allow = append(allow, mcp.AgyQualifiedToolName(name))
	}
	return map[string]any{
		"allow": allow,
		"deny":  append([]string(nil), withheldNativeTools...),
	}
}

// The three paths, inside the private HOME writeAgyHome builds, that agy
// reads its MCP servers, its own settings and its lifecycle hooks from.
// Named once, here, because they are the whole of what this package
// depends on in agy's on-disk layout rather than in its command line or
// its output: if a future agy moves one of them, these three vars are the
// change.
//
// They are not all in one directory, and that is not an oversight: agy
// keeps MCP servers in ~/.gemini/config/mcp_config.json (what `agy mcp
// add` edits and `agy mcp list` reads) and CLI settings in
// ~/.gemini/antigravity-cli/settings.json. Written anywhere else --
// ~/.gemini/settings.json, say, which is where Gemini CLI kept both --
// they are silently ignored: `agy mcp list` reports "No MCP servers
// configured", and a run authenticates as though no API key had been
// configured at all. hooks.json sits beside mcp_config.json because both
// are "customizations" in agy's own vocabulary, and ~/.gemini/config is
// the global customization root its shipped guide names (the binary
// unpacks that guide into a fresh HOME under
// antigravity-cli/builtin/skills/agy-customizations/, which is where the
// hook contract quoted in hookConfigJSON comes from).
//
// Settings and MCP servers are read from exactly one path each, but hooks
// are not, and the difference is worth knowing about: 1.1.26 loads
// hooks.json from ~/.gemini/config/, ~/.gemini/antigravity-cli/ and
// ~/.gemini/ alike, and a file at the antigravity-cli path *replaces* the
// one here rather than merging with it (measured both ways in
// docs/agy-surface.md, which is also where "planted together" and
// "planted one at a time" first disagreed). Nothing bites today, because
// writeAgyHome builds the whole HOME per run and agy unpacks no hooks.json
// of its own into a fresh one. It would bite the first time a run is given
// a HOME it does not own outright -- a reused HOME, a mounted one, a
// future agy that ships a default -- and it would bite silently, with the
// run's own hook simply absent. `agy -p /hooks` is the check, and
// TestLiveAgyLoadsGrainsHookConfig is where it is already asked.
var (
	mcpConfigRelPath   = filepath.Join(".gemini", "config", "mcp_config.json")
	cliSettingsRelPath = filepath.Join(".gemini", "antigravity-cli", "settings.json")
	hooksConfigRelPath = filepath.Join(".gemini", "config", "hooks.json")
)

// hookConfigJSON is the content of agy's hooks.json: one PreToolUse hook,
// matching every tool, whose command is grain forking itself back into
// HookDecision (cmd/grain's "agy-tool-hook" subcommand, the same
// fork-myself trick mcpConfigJSON plays for the MCP server).
//
// This is the one denial agy documents as a denial, and the one that has
// been watched working: a live agy 1.1.26 driving gemini-3.1-pro-high,
// told in as many words to run `echo ... > /tmp/agyprobe.txt` as a shell
// command, called its own run_command and then its own write_to_file and
// find_by_name, had all three blocked by this hook, and gave up with the
// file never created. Its own shipped guide
// (builtin/skills/agy-customizations/docs/hooks.md, unpacked into any
// fresh HOME) gives PreToolUse the contract "use to gate, block, or audit
// tool executions": the hook is handed {"toolCall": {"name": ..., "args":
// ...}} on stdin before the tool runs, and a reply of {"decision":
// "deny"} is spelled out as "hard block the execution immediately" --
// which is a different mechanism from the permission prompt that
// --dangerously-skip-permissions auto-approves, and the reason this is
// here as well as permissionRules.
//
// The matcher is "*" rather than a list of tool names because the names
// it matches on are agy's step types, and the name grain must decide on
// is the one inside the payload; HookDecision reads that instead. See
// there for why the decision is a deny list rather than an allow list,
// and what a run does when this hook does not work at all.
//
// The schema is agy's own: a map from hook name to its per-event
// configuration, each event a list of {matcher, hooks} groups. It is not
// validated on load -- a hooks.json agy cannot make sense of leaves a run
// with no hooks and no complaint -- so what proves this file is the shape
// agy reads is `agy -p /hooks`, which prints the loaded hooks the way
// -p /permissions prints the loaded rules.
func hookConfigJSON(grainBinaryPath string) ([]byte, error) {
	return json.Marshal(map[string]any{
		HookName: map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "*",
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": shellQuote(grainBinaryPath) + " " + HookSubcommand,
					// Seconds. A fork of grain that only reads stdin and
					// writes a decision does not take one of these, but a
					// loaded controller can be slow to start a process,
					// and agy's own default is 30.
					"timeout": 30,
				}},
			}},
		},
	})
}

// HookName is what this run's hook is called in hooks.json. agy merges
// hooks from every source it finds by name, so a name of grain's own
// keeps this one distinct from anything a future agy ships built in.
//
// Exported because it is also how a real agy reports the hook back
// (`agy -p /hooks` prints this name in the first field of the record it
// loaded), and the live test that reads that output should match on this
// constant rather than on a second copy of the string.
const HookName = "grain-native-tool-denial"

// HookSubcommand is the argv[1] cmd/grain matches to run HookDecision.
// Named here rather than there because this package is what writes it
// into agy's config, and the two must agree.
const HookSubcommand = "agy-tool-hook"

// shellQuote wraps a path for the `sh -c` agy runs a hook command
// through. Deployments put grain in /usr/local/bin, but a path is not
// grain's to assume.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HookDecision answers one PreToolUse payload: it returns the JSON agy
// reads back on stdout, and it is the whole of what cmd/grain's
// "agy-tool-hook" subcommand does.
//
// It denies by name, from withheldNativeTools, rather than allowing by
// name or by grain's prefix, and the asymmetry is the safety property.
// This hook stands between the model and every tool call a run makes,
// including grain's own; a rule of "deny anything not named
// mcp_grain-sandbox_*" would turn any surprise in the payload -- a name
// spelled differently than expected, an agy version that reports MCP
// calls under a step type instead of a tool name -- into a run that can
// do nothing at all, and a run's own transcript is a slow way to find
// that out. A deny list can only ever fail the other way: a payload this
// does not recognise gets no decision, and the call proceeds exactly as
// it does today.
//
// Saying "no decision" is the part that has to be got exactly right, and
// the first version of this got it exactly wrong. See noOpinion.
func HookDecision(payload []byte) []byte {
	var in struct {
		ToolCall struct {
			Name string `json:"name"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return noOpinion
	}
	// The raw name, deliberately not mcp.BareToolName's: that would strip
	// grain's own prefix and leave mcp_grain-sandbox_run_command matching
	// the deny list as "run_command", which is precisely the name
	// collision toolPreamble spends a paragraph on -- here it would deny
	// the run the one tool that does reach its sandbox.
	if !IsWithheldNativeTool(in.ToolCall.Name) {
		return noOpinion
	}
	reply, err := json.Marshal(map[string]any{
		"decision": "deny",
		// Read by the model, so it says what to do instead rather than
		// only what it may not do -- the same argument toolPreamble makes
		// at greater length.
		//
		// The alternatives are named from the constructor rather than
		// derived from the denied tool's own name. Prefixing that name
		// was the obvious thing and it is wrong: agy's roster and
		// grain's are not the same roster, so write_to_file became
		// "use mcp_grain-sandbox_write_to_file", a tool that does not
		// exist, and a denial whose advice is a dead end is a denial
		// the model works around instead of following.
		"reason": in.ToolCall.Name + " runs on the machine hosting this session rather than in your " +
			"sandbox, so grain neither sees the call nor keeps what it does. Use " +
			joinNames(sandboxToolNames()) + " instead, or another " +
			mcp.AgyQualifiedToolName("") + "* tool.",
	})
	if err != nil {
		return noOpinion
	}
	return reply
}

// noOpinion is the reply that leaves a tool call to whatever agy would
// have done with it: nothing at all, written on a stdout the hook closes
// empty. It is what HookDecision returns for every name it was not asked
// to deny, and for every payload it could not read.
//
// It used to be `{}`, on the reading that an object carrying no decision
// carries no opinion. Against a real agy 1.1.26 driving a real model that
// is precisely backwards, and it denied every tool call a run made --
// grain's own included, so a run so configured could do nothing at all
// except say why. The whole measured contract, since agy's own guide
// documents only the four decision values and not what it does with
// anything else:
//
//   - no output at all, exit 0: the call proceeds. This is "no opinion",
//     and it is the only spelling of it that is not also something else.
//   - {"decision":"allow"}: proceeds, skipping the permission prompt.
//   - {"decision":"ask"}: proceeds, the prompt being auto-approved by
//     Run's --dangerously-skip-permissions.
//   - {"decision":"deny","reason":r}: "tool call denied by pre-tool hook:
//     r", and the tool does not run. This is the one grain wants.
//   - {} or any object with no "decision": denied, with an empty reason.
//     An absent decision is not an abstention; it is a deny nobody
//     explained.
//   - output that is not JSON: the call fails with "failed to unmarshal
//     result from hook ... via protojson".
//   - a hook command that exits non-zero, or is not there at all: the
//     call fails with "JSON hook ... failed: command failed: exit status
//     N".
//
// The last two are why cmd/grain's subcommand exits 0 whatever happens
// and writes only this or a decision: a hook that errors does not fail
// open, it fails the tool call, so a broken grain binary would take the
// run's tools with it.
var noOpinion []byte

// apiKeyModelProvider is the "modelProvider" value that turns
// GEMINI_API_KEY into a working credential. agy's other providers are
// its own hosted backends, reached with an OAuth session instead.
const apiKeyModelProvider = "gemini"

// writeAgyHome builds the private HOME one run gets: a fresh directory
// containing the config naming this run's own MCP server, the settings
// file carrying this run's permission rules (and, when this run has an
// API key to authenticate with, the setting that makes agy use it), and
// the hooks.json that puts grain in front of every tool call. It returns
// that directory and a cleanup func the caller defers.
//
// A private HOME rather than `agy mcp add` is the whole point -- see this
// package's own doc comment on why a per-user registration cannot express
// a per-run sandbox binding. It has the same effect claude's
// --strict-mcp-config has there: the only MCP server this run can see is
// the one written here, because there is no other config file in the
// HOME it was given to find one in.
func writeAgyHome(grainBinaryPath string, mcpArgs []string, apiKeyAuth bool) (home string, cleanup func(), err error) {
	mcpConfig, err := mcpConfigJSON(grainBinaryPath, mcpArgs)
	if err != nil {
		return "", nil, fmt.Errorf("antigravity: building agy's mcp config: %w", err)
	}
	settings, err := settingsJSON(apiKeyAuth)
	if err != nil {
		return "", nil, fmt.Errorf("antigravity: building agy's settings: %w", err)
	}
	hooks, err := hookConfigJSON(grainBinaryPath)
	if err != nil {
		return "", nil, fmt.Errorf("antigravity: building agy's hook config: %w", err)
	}
	files := map[string][]byte{
		mcpConfigRelPath:   mcpConfig,
		cliSettingsRelPath: settings,
		hooksConfigRelPath: hooks,
	}

	home, err = os.MkdirTemp("", "grain-agy-home-*")
	if err != nil {
		return "", nil, fmt.Errorf("antigravity: creating agy home: %w", err)
	}
	cleanup = func() { os.RemoveAll(home) }
	if err := os.MkdirAll(agyWorkspaceDir(home), 0o700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("antigravity: creating agy workspace: %w", err)
	}
	for rel, content := range files {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("antigravity: creating agy config dir: %w", err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("antigravity: writing %s: %w", rel, err)
		}
	}
	return home, cleanup, nil
}

// agyWorkspaceDir is the empty directory inside a run's private HOME that
// workDir hands a kontur run. See workDir.
func agyWorkspaceDir(home string) string { return filepath.Join(home, "workspace") }

// workDir is the directory agy is started in.
//
// For a host-rooted sandbox that is the sandbox itself, as it has always
// been. For a kontur run there is no local path to name -- the sandbox is
// a VM the forked mcpserver reaches over SSH -- and cfg.SandboxRoot is
// empty, which used to leave the subprocess in whatever directory the
// daemon itself was started in. That matters because agy brings its own
// native file and command tools, which run wherever it runs: a model
// reaching for run_command or write_to_file on a kontur run would act on
// the controller, and in the daemon's own working directory at that. An
// empty scratch directory inside the run's private HOME is somewhere
// those tools can do no harm, and it disappears with the HOME.
//
// It is not a substitute for the guarantee: agy has no way to withhold
// its own tools. Measured against 1.1.26 by reading the shipped binary
// (see the README): no flag names a tool at all, enabledTools and
// disabledTools are keys on an MCP *server's* entry in mcp_config.json,
// a custom agent definition can replace the system prompt but not the
// tool roster, and settings.json holds the permission system --
// machinery for approving a call, which Run's
// --dangerously-skip-permissions switches off -- rather than a roster.
// So what steers a run to grain's tools is eager registration plus
// toolPreamble, and what actually contains it is a kontur sandbox.
func workDir(cfg agent.RunConfig, home string) string {
	if cfg.SandboxRoot != "" {
		return cfg.SandboxRoot
	}
	return agyWorkspaceDir(home)
}

// withheldNativeTools names the agy built-in tools a run is told to treat
// as unavailable, and it is the list toolPreamble spells out by name.
//
// These are the ones that overlap what grain's own tools do -- run a
// command, read a file, write one, search the tree -- which is exactly
// why a model reaches for them: they are the obvious tool for the job it
// was given, and they are the wrong one here because they execute
// wherever agy does. They were observed on agy 1.1.25's own init roster
// (tests/e2e/live_test.go logs the whole of it), which is where a name
// added to this list should come from too.
//
// It is deliberately not the whole roster. agy advertises 57 native tools
// (counted off 1.1.26's own init event in docs/agy-surface.md, which is
// also where the 55 this comment used to claim was corrected) and pins
// none of them, so a
// hand-copied catalogue would drift silently and read as authoritative
// while doing so; what the prompt says after this list -- anything whose
// name does not carry grain's prefix -- is the rule, and these are the
// examples that make it concrete.
//
// It is the whole of the roster that reads, writes or executes, though,
// because this list is no longer only prose: permissionRules denies these
// names in agy's settings and HookDecision blocks a call to one, so a
// name missing from here is a tool nothing stops. Everything else agy
// carries -- its browser, its knowledge store -- is left alone
// deliberately: those do not touch the controller's filesystem, and a
// denial nobody has watched work is safest kept to the tools that
// motivate it.
//
// The two subagent tools are the exception to that last rule, and they
// are here for what they delegate to rather than for what they do
// themselves. define_subagent takes an enable_write_tools flag and
// invoke_subagent runs the result, so "spawn a subagent and have it write
// the file" would be a route to the controller's filesystem that never
// calls a tool named above -- and whether a subagent's own calls are put
// in front of this run's PreToolUse hook is a question the configuration
// grain runs agy in cannot answer, because it has no subagent to ask.
// Measured against 1.1.26 by capturing the function declarations agy
// actually sends the model (agy honours HTTPS_PROXY, so a logging proxy
// reads them; see the README): a run's model is offered 13 of agy's
// native tools, and neither subagent tool is among them -- a live model
// told in as many words to call define_subagent got agy's own `unknown
// tool: "define_subagent"` back, and grain's hook was never asked about
// it, agy refusing an unknown tool before any hook runs. So denying them
// here changes nothing about a run today. It is written because what
// prunes that roster is partly a server-side feature flag rather than
// anything in this binary or this repository, so the tools can appear on
// a day nobody changed either, and a deny list already carrying the name
// is what makes that day uneventful. HookDecision's reason names grain's
// file tools as the alternative, which is the wrong advice for a
// delegation tool and the right refusal; a run that needs the work done
// does it with grain's tools itself.
var withheldNativeTools = []string{
	"run_command", "command_status", "send_command_input",
	"view_file", "write_to_file", "replace_file_content",
	"multi_replace_file_content", "sed_file", "list_dir",
	"grep_search", "find_by_name", "notebook_edit", "notebook_execution",
	"define_subagent", "invoke_subagent",
}

// IsWithheldNativeTool reports whether name is one of agy's own tools
// this package refuses -- spelled as agy spells it in a hook payload,
// which is bare rather than prefixed (see HookDecision on why the raw
// name is what matters).
//
// Exported for the live test that watches a real agy honour the refusal
// (tests/e2e/live_native_tools_test.go). That test has to tell a native
// call apart from grain's own in agy's own vocabulary, and a second copy
// of this list written out there would go stale the first time a name is
// added here.
func IsWithheldNativeTool(name string) bool {
	return slices.Contains(withheldNativeTools, name)
}

// sandboxToolNames is the handful of grain tools that actually act on the
// sandbox -- run_command, read_file, write_file, edit_file -- spelled the
// way agy names them once eagerly registered.
//
// Taken from the constructor rather than written out, for the reason
// publishedTools is: a tool added to (or renamed in) mcp.NewSandboxTools
// should change what the prompt tells a run it has, and nobody remembers
// to edit prose. The rest of the published tools are described by
// category instead of by name, because which of them a given run actually
// holds depends on its configuration (pull_request_status needs a repo,
// open_pull_request and recreate_sandbox a -server/-task pair, the
// selfdebug tools a grant), and a prompt that named a tool this run does
// not have would send it looking for one.
func sandboxToolNames() []string {
	var names []string
	for _, t := range mcp.NewSandboxTools("") {
		names = append(names, mcp.AgyQualifiedToolName(t.Name))
	}
	return names
}

// toolPreamble is the short note prepended to a run's prompt naming which
// tools reach its sandbox and which ones it must not use, and it exists
// because agy cannot be told to withhold its own.
//
// Every other framework grain drives can be handed an empty native tool
// roster (claude's --tools ” plus --strict-mcp-config), or one that can
// do no damage (codex's read-only sandbox_mode). agy offers neither, so a
// run always sees agy's own run_command, view_file, write_to_file,
// replace_file_content and the rest alongside grain's -- and those
// execute wherever agy itself is running, which is the controller. On a
// host-rooted sandbox that lands in the sandbox directory by accident,
// because it is also agy's working directory; on a kontur run it does not
// land in the sandbox at all. Either way the call bypasses everything
// grain's own tools carry with them: the result caps, the timeout
// reporting, the remaining-runtime announcements, and the record of the
// call that orchestrator reads a run's outcome from.
//
// Saying so in the prompt is the only lever there is, so it says three
// things rather than one: which tools do the work here (by name, from the
// constructor), that the built-in ones are to be treated as unavailable
// (by name, so the model is not left to work out which of its own tools
// the rule covers), and that the two rosters share names -- agy's own
// run_command and grain's mcp_<server>_run_command are different tools on
// different machines, and a model that has both in front of it and
// notices neither the prefix nor the collision picks the wrong one while
// believing it picked the right one.
//
// Each is stated as what the tools do rather than as a rule for its own
// sake, because a model that understands why its native tools are the
// wrong ones here keeps choosing correctly in situations this text did
// not anticipate.
func toolPreamble(cfg agent.RunConfig) string {
	where := "your sandbox"
	if cfg.KonturVM != "" {
		where = "your sandbox, which is a separate virtual machine"
	}
	prefix := mcp.AgyQualifiedToolName("")
	return "Use the " + prefix + "* tools for everything you do to " + where + ". " +
		joinNames(sandboxToolNames()) + " are the ones that reach " + where + "; " +
		"the rest of the " + prefix + "* tools are how you report back, ask a question, " +
		"read CI, or ask grain to open your pull request.\n" +
		"Treat every one of your own built-in tools as unavailable -- " +
		strings.Join(withheldNativeTools, ", ") + ", and anything else whose name does not begin " +
		prefix + ". They do not run in " + where + ": they run on the machine hosting this " +
		"session, which is not the sandbox and is not where your work belongs, and grain does " +
		"not see the calls, so anything you do with them is both in the wrong place and " +
		"unrecorded. The names collide, which is the trap -- your own run_command and " +
		mcp.AgyQualifiedToolName("run_command") + " are different tools on different machines, " +
		"and only the prefixed one does what you were asked to do.\n\n"
}

// joinNames renders a tool list as prose: "a, b and c". A plain
// strings.Join would read as a fragment of JSON in the middle of a
// sentence the model is meant to take as an instruction.
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// userEvent is the one stdin line a `--input-format stream-json` run
// needs: the prompt, as a user turn. The prompt travels over stdin rather
// than as the argument to --print because untrusted issue content must
// never become a shell- or ps-visible argument (dispatch.py's own
// docstring makes the identical point about its stdin-redirect path, and
// agent/claude passes its prompt the same way). agy's other print mode,
// `--print <prompt>`, puts it in argv instead, which is why this package
// does not use it.
func userEvent(prompt string) (string, error) {
	line, err := json.Marshal(map[string]any{
		"event":   "user",
		"message": map[string]any{"role": "user", "content": prompt},
	})
	if err != nil {
		return "", fmt.Errorf("antigravity: encoding prompt: %w", err)
	}
	return string(line) + "\n", nil
}

// Run implements agent.Framework: it writes this run's own MCP settings
// into a private HOME pointing at grainBinaryPath and cfg.SandboxRoot or
// cfg.KonturVM (mcpServerArgs), runs agy in stream-json mode with the
// prompt on stdin, and parses the resulting stream back into a Result.
//
// Run may return a non-nil Result together with a non-nil error, and a
// caller must read both. A run that edits files, commits, pushes and only
// then trips the turn cap has already changed the world, and a caller
// that treats the error as "no result" strands that work -- the failure
// that taught this is recorded in agent.Framework's own contract, and it
// is why a cancelled or failed subprocess's partial stdout is still
// parsed here rather than discarded.
//
// When cfg.TranscriptPath is set, the raw stream-json is mirrored there
// live, one line per event, as agy emits them -- the file this package's
// own LiveTranscriptDir reads back with PartialTranscript to render a
// still-running run's transcript-in-progress. It is opened O_TRUNC, not
// O_APPEND: a path is only ever reused across runs by a caller passing
// the same run ID twice, which should never happen, and starting clean is
// what makes a stale previous run's bytes never a way to misread this
// one's. The file is left on disk once Run returns either way -- cleaning
// it up is the caller's job, the same way orchestrator.RunDispatch owns
// cfg.SandboxRoot's own lifecycle rather than this package.
//
// cfg.Addenda is not polled. The former in-process Gemini runtime could
// fold a comment posted mid-run into its next turn because it owned the
// turn loop; agy owns it now, and a `--print` run is one blocking
// subprocess call with its prompt written before the process starts. A
// comment posted while a run is in flight waits for the task's next
// dispatch instead, exactly as it already does under agent/claude -- see
// RunConfig.Addenda's own doc comment.
//
// # The native tool roster
//
// claude empties its own tool roster with --tools ” and admits only the
// grain-sandbox tools with --strict-mcp-config plus --allowedTools. agy
// exposes no equivalent switch: no --tools, no --strict-mcp-config, and
// its disabledTools setting applies to an MCP server's tools rather than
// its own. Every run therefore sees agy's native run_command, view_file,
// write_to_file and the rest alongside grain's, and those execute
// wherever agy does -- on the controller.
//
// Three things stand in for the switch that does not exist. The private
// HOME holds exactly one MCP server, so grain's tools are the only MCP
// tools there are. Those tools are registered eagerly, so the model can
// see them rather than having to go looking. And toolPreamble opens the
// prompt by naming both rosters: the grain tools that reach the sandbox,
// and the agy tools to treat as unavailable (withheldNativeTools),
// including the names the two rosters share. verifyToolRoster then
// reports -- as a transcript line, not a failure -- a roster with no
// route to grain's server at all.
//
// None of that is a guarantee, and it is not offered as one. A deployment
// that needs one should run agy against a kontur sandbox (cfg.KonturVM),
// where the controller's filesystem is not reachable from the guest at
// all; workDir gives such a run an empty scratch directory to start in,
// so a native tool reached for by mistake writes somewhere harmless.
func (f *Framework) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	mcpArgs, err := f.mcpServerArgs(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if f.grainBinaryPath == "" {
		return nil, fmt.Errorf("antigravity: grainBinaryPath is required")
	}
	maxTurns := f.maxTurns
	if cfg.MaxTurns > 0 {
		maxTurns = cfg.MaxTurns
	}

	// Read before the HOME is built rather than just before the exec:
	// whether this run has a key of its own decides what goes into that
	// HOME, since a key agy is never told to look for is a key it does
	// not use (see writeAgyHome).
	apiKey, err := f.resolveAPIKey(ctx)
	if err != nil {
		return nil, err
	}
	home, cleanup, err := writeAgyHome(f.grainBinaryPath, mcpArgs, apiKey != "")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var sinks []io.Writer
	if cfg.TranscriptPath != "" {
		transcriptFile, err := os.OpenFile(cfg.TranscriptPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("antigravity: opening transcript file: %w", err)
		}
		defer transcriptFile.Close()
		sinks = append(sinks, transcriptFile)
	}

	// The turn cap watches the same live stream the transcript mirror
	// does and cancels this run's own context once the cap is reached --
	// agy having no --max-turns to enforce it in the binary (see this
	// package's doc comment).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	capWatch := &turnCap{max: maxTurns, cancel: cancel}
	sinks = append(sinks, capWatch)

	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
	}
	// Every dispatch passes this. It is dropped only by the live probe
	// that measures what a run does without it -- see
	// WithoutPermissionOverrideForTest, and permissionRules for what the
	// override overrides.
	if !f.withoutPermissionOverride {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args,
		"--disable-slash-commands",
		// Without this a run is over in five minutes; see printTimeout.
		"--print-timeout", printTimeout(ctx).String(),
	)
	if f.model != "" {
		args = append(args, "--model", f.model)
	}
	// The other half of the selection, and dropped for a model whose own
	// name already carries an effort: agy refuses that pair outright
	// unless the two happen to agree ("--model gemini-3.1-pro-high
	// conflicts with --effort=low"), and a deployment that stored a
	// suffixed catalog name -- the only spelling `agy models` prints --
	// has said which effort it wants in the model it named. See
	// DefaultModel for the whole measured table.
	if f.effort != "" && !modelNamesItsEffort(f.model) {
		args = append(args, "--effort", f.effort)
	}
	// --add-dir names the one directory this run may touch. It is only
	// meaningful for a host-rooted sandbox; a kontur run reaches its
	// workspace through the forked mcpserver's SSH transport instead, and
	// there is no local path here to name.
	if cfg.SandboxRoot != "" {
		args = append(args, "--add-dir", cfg.SandboxRoot)
	}

	env := []string{"HOME=" + home}
	if apiKey != "" {
		// GOOGLE_API_KEY is cleared alongside, rather than left as the
		// controller's own environment has it: agy reads both and prefers
		// GOOGLE_API_KEY when the two are set ("Warning: Both
		// GOOGLE_API_KEY and GEMINI_API_KEY are set. Using
		// GOOGLE_API_KEY."), so an unrelated key exported on the
		// controller would quietly become the credential every run bills
		// against, instead of the one this deployment configured.
		env = append(env, "GEMINI_API_KEY="+apiKey, "GOOGLE_API_KEY=")
	}

	stdin, err := userEvent(toolPreamble(cfg) + cfg.Prompt)
	if err != nil {
		return nil, err
	}

	stdout, runErr := f.run.Run(runCtx, args, stdin, env, workDir(cfg, home), io.MultiWriter(sinks...))
	result, parseErr := parseTranscript(stdout)
	// Parsed once here rather than at each branch below, since two of
	// them read the same pass: what agy said about how the run ended is
	// both the evidence a quota refusal leaves and the sentence that
	// explains an ordinary failure.
	events := parseEvents(stdout)
	// Read from the terminal event's own text plus whatever the
	// subprocess itself reported, because a quota refusal can arrive
	// either way: agy usually reports it as a failed terminal status
	// (parseErr, below) but a hard enough refusal kills the process
	// first (runErr). See usagelimit.go.
	limit := usageLimitFailure(events.resultText, runErr)
	switch {
	case capWatch.tripped():
		// The cap cancelled the subprocess, so runErr is that
		// cancellation rather than a fault of agy's -- report the cap.
		// result is whatever the run managed before it was stopped, and
		// is returned alongside the error, never instead of it.
		return partialResult(result, stdout), agent.MaxTurnsExceeded("antigravity", maxTurns)
	case limit != nil:
		// Ahead of both branches below, which would otherwise render
		// this as "running agy: exit status 1" or as agy's own generic
		// "run ended in status FAILURE", and lose the one thing about it
		// a deployment can act on: nothing is wrong with this run, the
		// credential it used has no quota left until its window resets.
		// orchestrator.RunDispatch reads the type (agent.UsageLimit) and
		// pauses dispatch rather than sending the next task at the same
		// refusal.
		//
		// result travels back alongside it, as everywhere else here: a
		// run that pushed a branch and then ran out of quota has already
		// changed the world.
		return partialResult(result, stdout), limit
	case runErr != nil:
		// Both halves, not just the exit status. agy reports a run it
		// could not finish as a terminal result event with a status of
		// ERROR or FAILURE -- "There was a network issue connecting to
		// the server, please try again." -- and then exits 1 with
		// nothing at all on stderr, so this arm used to render the one
		// failure a reader can act on as "running agy: exit status 1
		// (stderr: )". events.resultErr is that sentence; the stream is
		// removed once the run finishes (orchestrator's own cleanup), so
		// what this returns is the last place it exists.
		return partialResult(result, stdout), agent.RunFailure("antigravity", "agy", events.resultErr, runErr)
	case parseErr != nil:
		// A capture with no terminal result event -- an agy that died
		// without runErr reaching us, or output truncated mid-stream.
		// partialResult, not result: parseTranscript hands back nothing
		// in that case, and a run that pushed a branch before the stream
		// stopped must still come back with the push.
		return partialResult(result, stdout), parseErr
	}
	result.Transcript = appendRosterNote(result.Transcript, verifyToolRoster(stdout))
	return result, nil
}

// resolveAPIKey reads this run's Gemini API key, or "" for a deployment
// that configured none and means agy to authenticate from its own
// ambient environment (see WithAPIKeyFunc, whose contract this keeps: an
// error is a key that could not be read, which is not the same thing).
func (f *Framework) resolveAPIKey(ctx context.Context) (string, error) {
	if f.apiKey == nil {
		return "", nil
	}
	key, err := f.apiKey(ctx)
	if err != nil {
		return "", fmt.Errorf("antigravity: reading the API key: %w", err)
	}
	return key, nil
}

// partialResult is what Run hands back alongside an error: whatever the
// run managed to do before it failed or was stopped.
//
// A run that produced no events at all gets a nil Result, not an empty
// one -- agent.Framework's own contract draws exactly that line ("a nil
// Result with an error means the run never started"), and callers depend
// on it: orchestrator.RunDispatch treats a non-nil Result as a run whose
// tool calls it must process, so an empty one for a run cancelled before
// agy ever spoke would report an agent that touched a sandbox it never
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

// verifyToolRoster reports whether agy's opening init event shows any way
// for this run to reach grain's tools, and says so when it does not.
//
// This used to ask the opposite question -- which advertised tools grain
// had not published -- on the theory that the roster was the run's whole
// tool vocabulary and so a stand-in for claude's --strict-mcp-config.
// Against the real binary that theory is simply wrong twice over. The
// roster is agy's own native tools and nothing else: MCP tools never
// appear in it, eagerly registered or not (measured on agy 1.1.25, which
// advertises 57 native tools and no grain tool under either spelling). So
// the old check both flagged all 57 of them on every single run, and
// missed the one thing worth noticing -- a run whose MCP server failed to
// load, which is invisible in that roster either way.
//
// What is visible is the bridge. call_mcp_tool is agy's dispatcher for
// lazily loaded MCP tools; an eagerly registered one appears under its
// own mcp_<server>_<tool> name (mcp.AgyQualifiedToolName). A roster with
// neither is a run that cannot reach grain at all, which is worth a line
// in the transcript -- as a note, not a failure, since the run may still
// have said something useful about why.
//
// An empty roster -- no init event, a capture that starts mid-stream --
// reports nothing: there is no roster to draw a conclusion from.
func verifyToolRoster(stdout string) string {
	roster := parseEvents(stdout).tools
	if len(roster) == 0 {
		return ""
	}
	eager := map[string]bool{}
	for _, name := range eagerToolNames() {
		eager[name] = true
	}
	for _, name := range roster {
		if name == mcpDispatcherTool || eager[name] {
			return ""
		}
	}
	return fmt.Sprintf("! agy advertised %d tool(s), none of them grain's and no %s to reach them "+
		"through: this run had no way to touch its sandbox or to report back",
		len(roster), mcpDispatcherTool)
}

// appendRosterNote records verifyToolRoster's finding in the transcript,
// where an operator reading a run will see it, rather than only in a log
// this deployment may not be collecting.
func appendRosterNote(transcript, note string) string {
	switch {
	case note == "":
		return transcript
	case transcript == "":
		return note
	}
	return transcript + "\n\n" + note
}
