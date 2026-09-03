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
// file that `agy mcp add` edits, and its tool manifests are cached under
// ~/.gemini/antigravity-cli/mcp/<server>/. A per-user registration is
// exactly the wrong shape here: two runs dispatched concurrently against
// two different sandboxes would share one registration, and whichever
// wrote it last would decide where both runs' tools landed. So Run gives
// each run its own private HOME (a temp directory holding just the
// settings file agy reads) instead of registering anything globally --
// see writeMCPSettings. That keeps the per-run sandbox binding the
// dispatch model depends on, and leaves the controller's real ~/.gemini
// untouched.
//
// agy also has no --max-turns. The cap RunConfig.MaxTurns asks for is
// therefore enforced here rather than by the binary: Run counts completed
// agent_response steps as they stream past and cancels the subprocess
// once the cap is reached (turnCap). procgroup.Prepare is what makes that
// cancellation reach agy's own MCP child too, rather than orphaning it.
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
	"strings"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/procgroup"
)

const (
	// DefaultModel is the model agy is asked for when a caller names
	// none. Left empty deliberately would mean "whatever the binary
	// defaults to"; naming it here keeps a deployment's runs reproducible
	// across agy upgrades the same way the former gemini package's own
	// DefaultModel did.
	DefaultModel = "gemini-3.1-pro"

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
	// writeMCPSettings; a plain exec.CommandContext cancel only kills agy
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

// WithModel overrides the model agy is asked for.
func WithModel(model string) Option {
	return func(f *Framework) { f.model = model }
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
// private HOME writeMCPSettings hands agy workable: a run authenticating
// by env var needs nothing out of the real ~/.gemini it no longer sees.
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

// New builds a Framework that runs the real agy binary at agyPath
// (typically just "agy", resolved against $PATH) and points every run's
// MCP settings at grainBinaryPath -- the same grain binary this process
// itself is (typically the result of os.Executable()) -- invoked as
// "grainBinaryPath mcpserver -sandbox-root <root>".
func New(agyPath, grainBinaryPath string, opts ...Option) *Framework {
	return newFramework(execRunner{agyPath: agyPath}, grainBinaryPath, opts...)
}

func newFramework(run runner, grainBinaryPath string, opts ...Option) *Framework {
	f := &Framework{run: run, grainBinaryPath: grainBinaryPath, model: DefaultModel, maxTurns: defaultMaxTurns}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// allowedTools names the exact tools NewSandboxTools, NewMockTools,
// NewPullRequestTools and NewOpenPullRequestTools register, mcp__-prefixed
// the way agy reports them
// once loaded from its settings -- computed from those constructors
// directly rather than hand-copied, so this can never drift from what
// the "mcpserver" subcommand actually advertises the way v1's
// hand-maintained _ALLOWED_TOOLS constant could (dispatch.py).
//
// pull_request_status is named unconditionally rather than only for a
// run that passed pullRequestArgs, for the reason agent/claude's own
// allowedTools gives: this list is a property of the tool vocabulary,
// not of one run's configuration, and mcpserver registers that tool
// either way.
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
	// whose mcpserver will not register it (no -server/-task): this list
	// only ever filters what the server actually advertises, so naming a
	// tool that is not there costs nothing. nil is a PullRequestOpener no
	// run ever gets -- this only wants the names.
	for _, t := range mcp.NewOpenPullRequestTools(nil) {
		names = append(names, mcp.QualifiedToolName(t.Name))
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
// runs on.
func (f *Framework) mcpServerArgs(cfg agent.RunConfig) ([]string, error) {
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

// mcpSettingsJSON is the content of the settings file agy reads its MCP
// servers from: grainBinaryPath spawned with mcpArgs (built by
// mcpServerArgs above) -- the "mcpserver" argument selects the same
// subcommand mcpserver.go implements, so agy forking this exact command
// is what actually starts an MCP server, rather than needing a separately
// built binary on disk. The schema is Gemini CLI's own mcpServers map,
// which agy inherited along with the ~/.gemini config directory.
func mcpSettingsJSON(grainBinaryPath string, mcpArgs []string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			mcpServerName: map[string]any{
				"command": grainBinaryPath,
				"args":    mcpArgs,
			},
		},
	})
}

// settingsRelPath is where, inside the private HOME writeMCPSettings
// builds, agy looks for the settings file above. Named once, here,
// because it is the single piece of this package that depends on agy's
// own on-disk layout rather than on its command line or its output: if a
// future agy moves the file, this constant is the whole change.
var settingsRelPath = filepath.Join(".gemini", "settings.json")

// writeMCPSettings builds the private HOME one run gets: a fresh
// directory containing nothing but the settings file naming this run's
// own MCP server. It returns that directory and a cleanup func the caller
// defers.
//
// A private HOME rather than `agy mcp add` is the whole point -- see this
// package's own doc comment on why a per-user registration cannot express
// a per-run sandbox binding. It has the same effect claude's
// --strict-mcp-config has there: the only MCP server this run can see is
// the one written here, because there is no other settings file in the
// HOME it was given to find one in.
func writeMCPSettings(grainBinaryPath string, mcpArgs []string) (home string, cleanup func(), err error) {
	settings, err := mcpSettingsJSON(grainBinaryPath, mcpArgs)
	if err != nil {
		return "", nil, fmt.Errorf("antigravity: building mcp settings: %w", err)
	}
	home, err = os.MkdirTemp("", "grain-agy-home-*")
	if err != nil {
		return "", nil, fmt.Errorf("antigravity: creating agy home: %w", err)
	}
	cleanup = func() { os.RemoveAll(home) }
	path := filepath.Join(home, settingsRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("antigravity: creating agy config dir: %w", err)
	}
	if err := os.WriteFile(path, settings, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("antigravity: writing agy mcp settings: %w", err)
	}
	return home, cleanup, nil
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
// exposes no equivalent switch. What this does instead is give the
// subprocess a HOME with exactly one MCP server in it and a working
// directory that is the sandbox itself, so the tools it reaches are this
// run's, and verifyToolRoster reports -- as a transcript line, not a
// failure, since a roster this code does not recognize is not by itself
// evidence of a breach -- any tool agy's own init event advertises beyond
// the ones grain published. A deployment that needs a hard guarantee
// should run agy against a kontur sandbox (cfg.KonturVM), where the
// controller's filesystem is not reachable from the guest at all.
func (f *Framework) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	mcpArgs, err := f.mcpServerArgs(cfg)
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

	home, cleanup, err := writeMCPSettings(f.grainBinaryPath, mcpArgs)
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
		"--dangerously-skip-permissions",
		"--disable-slash-commands",
	}
	if f.model != "" {
		args = append(args, "--model", f.model)
	}
	// --add-dir names the one directory this run may touch. It is only
	// meaningful for a host-rooted sandbox; a kontur run reaches its
	// workspace through the forked mcpserver's SSH transport instead, and
	// there is no local path here to name.
	if cfg.SandboxRoot != "" {
		args = append(args, "--add-dir", cfg.SandboxRoot)
	}

	env := []string{"HOME=" + home}
	if f.apiKey != nil {
		apiKey, err := f.apiKey(ctx)
		if err != nil {
			return nil, fmt.Errorf("antigravity: reading the API key: %w", err)
		}
		if apiKey != "" {
			env = append(env, "GEMINI_API_KEY="+apiKey)
		}
	}

	stdin, err := userEvent(cfg.Prompt)
	if err != nil {
		return nil, err
	}

	stdout, runErr := f.run.Run(runCtx, args, stdin, env, cfg.SandboxRoot, io.MultiWriter(sinks...))
	result, parseErr := parseTranscript(stdout)
	switch {
	case capWatch.tripped():
		// The cap cancelled the subprocess, so runErr is that
		// cancellation rather than a fault of agy's -- report the cap.
		// result is whatever the run managed before it was stopped, and
		// is returned alongside the error, never instead of it.
		return partialResult(result, stdout),
			fmt.Errorf("antigravity: exceeded max turns (%d) without a final answer", maxTurns)
	case runErr != nil:
		return partialResult(result, stdout), fmt.Errorf("antigravity: running agy: %w", runErr)
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

// verifyToolRoster reports any tool agy's own init event advertised that
// grain's "mcpserver" subcommand never published -- the check that stands
// in for claude's --strict-mcp-config, which agy has no equivalent of.
// It returns the unexpected names, if any; it does not fail a run over
// them, since agy reporting a tool under a name this code does not
// recognize is not by itself evidence that a run reached anything it
// should not have.
func verifyToolRoster(stdout string) []string {
	expected := map[string]bool{}
	for _, name := range allowedTools() {
		expected[name] = true
	}
	var unexpected []string
	for _, name := range parseEvents(stdout).tools {
		if !expected[name] {
			unexpected = append(unexpected, name)
		}
	}
	return unexpected
}

// appendRosterNote records verifyToolRoster's finding in the transcript,
// where an operator reading a run will see it, rather than only in a log
// this deployment may not be collecting.
func appendRosterNote(transcript string, unexpected []string) string {
	if len(unexpected) == 0 {
		return transcript
	}
	note := fmt.Sprintf("! agy advertised %d tool(s) grain did not publish: %s",
		len(unexpected), strings.Join(unexpected, ", "))
	if transcript == "" {
		return note
	}
	return transcript + "\n\n" + note
}
