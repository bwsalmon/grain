// mcpserver.go implements `grain mcpserver`, formerly its own
// cmd/mcpserver binary before bwsalmon/agents#313 combined every mode
// into one: it serves the sandbox tools and the escape hatches
// (ask_question, comment_on_issue, propose_task, add_review_comment --
// recorded here, relayed by orchestrator.ProcessResult once the run
// finishes) over its own stdin/stdout, so anything that can spawn a
// subprocess and speak MCP -- not just an in-process
// client -- can drive it. Real MCP clients (an actual `claude` or
// `gemini` CLI's --mcp-config, for instance) can point at this same
// grain binary, "mcpserver" and its own flags as the configured args, as
// pkg/agent/claude does.
//
// Its sandbox tools run against one of two backends: a local directory
// (-sandbox-root, unchanged since before bwsalmon/agents#256 -- what
// pkg/mcp's own tests and tests/e2e/ still use, since neither wants a
// real VM), or a real kontur-managed VM (-kontur-vm, see package kontur
// and pkg/mcp's NewSSHSandboxTools) -- the wiring bwsalmon/agents#256
// asked for, so a run_command/read_file/edit_file/write_file call
// actually lands inside one of the sandbox VMs kontur is running
// instead of the local-directory stand-in pkg/mcp/sandbox_tools.go's
// own doc comment describes. -kontur-backend selects which of kontur's
// two ways of running that VM -kontur-vm was created with -- "docker"
// (the default, bwsalmon/agents#353) needs just a local docker daemon;
// "static-pod" needs a standalone kubelet
// (deploy/static-kubelet/README.md) and is resolved via crictl instead.
//
// -pr-repo/-pr-branch add pkg/mcp's pull_request_status and
// wait_for_checks to that roster, so a run can read CI's verdict on the
// commits it pushes -- or block until there is one, rather than spending
// a turn per poll -- and repair a red build inside its own turn budget
// instead of leaving it for the merge queue's separate fix task. Those
// reads happen *here*, in this
// process, which runs on the controller: the GitHub credential comes off
// the controller's own -data-dir (the same secrets/github ladder
// `grain daemon` loads) and never crosses into the sandbox, so
// docs/design.md's split surface -- "Sandboxes: git transport only" --
// is untouched. See pkg/mcp/pullrequest_tools.go's own doc comment.
//
// -server and -task add two further tools. open_pull_request: a run that
// has pushed its branch can have grain open its pull request there and
// then, and read back what the repo's own CI makes of it, rather than
// exiting blind and leaving the pull request to the finish path. And
// recreate_sandbox: a run whose sandbox has become unusable -- a wedged
// guest, a full disk, a filesystem an interrupted install left in a
// state no command can untangle -- can have grain destroy it and build a
// clean one, and carry on in the turns it has left instead of failing
// every remaining call in a sandbox it cannot repair from inside.
//
// -self-debug adds the self-debug capability's own read-only tools, for
// a run whose task holds that grant: read_grain_source and
// list_grain_source, reading the checkout -grain-src-dir names
// (pkg/capability/selfdebug). Until this flag existed they were
// assembled by orchestrator.Config.GrantTools and consumed by nobody:
// every framework that remains forks a CLI and ignores
// agent.RunConfig.Tools, so the capability granted tools no running
// agent could call.
//
// This flag used to turn on a second half as well -- list_grain_tasks,
// read_grain_task, read_grain_task_prompt and read_grain_task_transcript,
// reading grain's *other* tasks over -server. Nothing serves those any
// more: the state repository holds every one of those rows as a file
// (pkg/staterepo, and the README's "The store is a git repository
// again"), so a task that needs to read what this deployment did is
// given read access to that repository and reads tables/task.json,
// tables/task_comment.json and tables/task_run.json with the file tools
// it already has.
//
// -run-deadline is the moment grain will cancel the run this process
// serves. It adds no tool: it makes every tool result carry how much
// wall-clock time is left, once there is little enough of it to change
// what a run should be doing (pkg/mcp's run_deadline.go). The prompt
// states the same budget up front, and is read once, hours earlier --
// this is the half that reaches a run at turn 200.
//
// The flags name the daemon to ask and the task to ask about. Unlike
// pull_request_status above, both of these are *writes*, and writes stay
// grain's: this process asks the daemon over its REST API rather than
// acting with a credential -- or, for the sandbox, an authority -- of
// its own. See daemonPullRequests and daemonSandbox below.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/bwsalmon/grain/pkg/capability/selfdebug"
	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/kontur"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

// daemonPullRequests implements mcp.PullRequestOpener by asking a running
// "grain daemon" to open the pull request for one task's branch (pkg/ui's
// POST /api/tasks/{id}/pull-request) -- the same REST API the grain CLI
// speaks, reached the same way, since this process is on the controller
// alongside the daemon that forked it.
//
// The hop exists because of what this call is, not because this process
// cannot reach GitHub at all: pullRequestReader below does build a REST
// client, for pull_request_status. That one only ever reads, and only
// within a scope fixed at process start. Opening a pull request is a
// write, and which task it is opened for, on which branch, against which
// base, has always been grain's decision rather than an agent's -- so it
// is made where those decisions are already made. This asks the daemon
// by task id, and the daemon reads the repo and the branch out of that
// task's own record; nothing in a tool call reaches GitHub as data.
type daemonPullRequests struct {
	client *ui.HTTPClient
	taskID string
}

func (d daemonPullRequests) OpenPullRequest(ctx context.Context) (mcp.PullRequestReport, error) {
	status, err := d.client.OpenPullRequest(ctx, d.taskID)
	if err != nil {
		return mcp.PullRequestReport{}, err
	}
	report := mcp.PullRequestReport{
		Repo:            status.Repo,
		Number:          status.Number,
		URL:             status.URL,
		ChecksAvailable: status.ChecksAvailable,
		ChecksError:     status.ChecksError,
	}
	for _, c := range status.Checks {
		report.Checks = append(report.Checks, mcp.CheckReport{
			Name: c.Name, Status: c.Status, Conclusion: c.Conclusion,
		})
	}
	return report, nil
}

// daemonSandbox implements mcp.SandboxRecreator by asking a running
// "grain daemon" to destroy this run's sandbox and build an empty one in
// its place (pkg/ui's POST /api/tasks/{id}/sandbox/recreate) -- the same
// REST hop daemonPullRequests above makes, over the same client, for the
// same reason.
//
// The reason is sharper here than it is there, though. This process
// cannot do the job at all, not merely "should not": every tool it
// serves reaches into the sandbox and does nothing else, while creating
// and destroying sandboxes needs the VM shape this run asked for, the
// proxy token to mint for it, the capability material to place in it and
// the repo to clone into it -- none of which exists on this side of the
// hop. Which sandbox gets destroyed is fixed at process start by -task,
// so no tool call can name another run's.
type daemonSandbox struct {
	client *ui.HTTPClient
	taskID string
}

func (d daemonSandbox) RecreateSandbox(ctx context.Context) (mcp.SandboxRecreationReport, error) {
	recreation, err := d.client.RecreateSandbox(ctx, d.taskID)
	if err != nil {
		return mcp.SandboxRecreationReport{}, err
	}
	return mcp.SandboxRecreationReport{
		Sandbox:     recreation.Sandbox,
		CheckoutDir: recreation.CheckoutDir,
		Restored:    recreation.Restored,
		Warnings:    recreation.Warnings,
	}, nil
}

func mcpserver(args []string) {
	fs := flag.NewFlagSet("grain mcpserver", flag.ExitOnError)
	root := fs.String("sandbox-root", "",
		"directory the local sandbox tools are confined to (mutually exclusive with -kontur-vm)")

	konturVM := fs.String("kontur-vm", "",
		"name of a kontur-managed VM (bwsalmon/kontur) to run the sandbox tools against over SSH, "+
			"instead of a local directory (mutually exclusive with -sandbox-root)")
	sshUser := fs.String("ssh-user", "", "username to SSH into -kontur-vm as (required with -kontur-vm)")
	execKey := fs.String("exec-key", "", "path, *inside -kontur-vm's container*, of the private key `kontur exec` authenticates to the guest with. Optional: left unset, `kontur exec` uses the keypair `kontur run` generates for that guest at boot, which is what a stock bwsalmon/kontur guest image authorizes (see its internal/guestkey). Set it only for a custom guest image that authorizes a key of its own instead.")
	workspace := fs.String("workspace", "",
		"working directory run_command/read_file/edit_file/write_file operate in on -kontur-vm (required with -kontur-vm)")

	dataDir := fs.String("data-dir", "",
		"grain's own data directory on this controller, holding secrets/github -- the credential "+
			"pull_request_status reads GitHub with. Required with -pr-repo.")
	prRepo := fs.String("pr-repo", "",
		"owner/name of the repository pull_request_status and wait_for_checks report on. Unset "+
			"leaves both tools registered but answering that this run has no repo, which is what "+
			"a task with no target really does have.")
	prBranch := fs.String("pr-branch", "",
		"branch within -pr-repo pull_request_status and wait_for_checks report on -- this run's "+
			"own branch, and the only one they can ever read (required with -pr-repo)")
	githubHost := fs.String("github-host", "github.com",
		"git host whose REST API pull_request_status reads (github.APIHost maps it to the API host)")
	githubInsecureHTTP := fs.Bool("github-insecure-http", false,
		"reach -github-host over plain HTTP instead of HTTPS -- for a local mock GitHub in a "+
			"live test, never for a real deployment")

	server := fs.String("server", "",
		"base URL of the \"grain daemon\" this server's run was dispatched by, e.g. http://127.0.0.1:8420 "+
			"-- with -task, adds the open_pull_request tool, which asks that daemon to open the run's own "+
			"pull request rather than opening one from here")
	taskID := fs.String("task", "",
		"id of the task this server's run belongs to (required with -server)")

	selfDebug := fs.Bool("self-debug", false,
		"serve the self-debug capability's own read-only tools: read_grain_source and "+
			"list_grain_source, over -grain-src-dir. Passed by a Framework only "+
			"for a run whose task holds the self-debug grant (agent.RunConfig.SelfDebug).")
	grainSrcDir := fs.String("grain-src-dir", "",
		"directory holding grain's own source for read_grain_source/list_grain_source to read, "+
			"read-only (cmd/grain/daemon.go's sourceDir: the copy baked into the deployment image, "+
			"or -upgrade-src-dir's checkout). Only used with -self-debug; unset, both tools answer "+
			"that this deployment has no source checkout to read.")

	runDeadline := fs.String("run-deadline", "",
		"RFC3339 time at which grain cancels the run this server serves -- the deadline on "+
			"the context orchestrator.RunDispatch gives framework.Run, passed on by the "+
			"Framework that forked this process (agent.RunDeadlineArgs). Set, every tool "+
			"result carries how much of it is left once the run is inside "+
			"mcp.RunDeadlineNoticeWindow of it; unset, results read exactly as they "+
			"otherwise would.")
	fs.Parse(args)

	var tools []mcp.Tool
	switch {
	case *konturVM != "" && *root != "":
		fmt.Fprintln(os.Stderr, "grain mcpserver: -sandbox-root and -kontur-vm are mutually exclusive")
		os.Exit(2)
	case *konturVM != "":
		tools = mustKonturSandboxTools(*konturVM, *sshUser, *execKey, *workspace)
	case *root != "":
		if info, err := os.Stat(*root); err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "grain mcpserver: -sandbox-root %q is not a directory: %v\n", *root, err)
			os.Exit(2)
		}
		tools = mcp.NewSandboxTools(*root)
	default:
		fmt.Fprintln(os.Stderr, "grain mcpserver: exactly one of -sandbox-root or -kontur-vm is required")
		os.Exit(2)
	}

	registry := mcp.NewRegistry()
	registry.AnnounceDeadline(parsedRunDeadline(*runDeadline), nil)
	registry.Register(tools...)
	// The sink is a throwaway because nothing reads an escape-hatch call
	// back out of *this* process: orchestrator.ProcessResult recovers
	// ask_question, comment_on_issue and propose_task from
	// agent.Result.ToolCalls once the run finishes, and relays each one
	// for real. The sink is here so a call answers with a real Result
	// rather than being a silent no-op, and the tools' own text describes
	// the relay rather than this sink -- see mcp.NewMockTools.
	registry.Register(mcp.NewMockTools(&mcp.MockSink{})...)
	// Unlike those, this one acts within the run rather than after it: it
	// really does read GitHub, from this process, on the controller. See
	// the file's doc comment.
	registry.Register(pullRequestTools(*dataDir, *githubHost, *githubInsecureHTTP, *prRepo, *prBranch)...)

	// open_pull_request and recreate_sandbox are the two tools here whose
	// effect is real and immediate, and both happen by asking the daemon
	// rather than from this process, so both are registered only when
	// this process was actually told which daemon and which task it
	// serves -- a run driven from a bare `grain mcpserver -sandbox-root`
	// (pkg/mcp's own tests, tests/e2e/) has no daemon to ask and is
	// better off not advertising tools that could only ever refuse.
	var client *ui.HTTPClient
	switch {
	case *server != "" && *taskID == "":
		fmt.Fprintln(os.Stderr, "grain mcpserver: -task is required with -server")
		os.Exit(2)
	case *server == "" && *taskID != "":
		fmt.Fprintln(os.Stderr, "grain mcpserver: -server is required with -task")
		os.Exit(2)
	case *server != "":
		client = ui.NewHTTPClient(*server)
		registry.Register(mcp.NewOpenPullRequestTools(
			daemonPullRequests{client: client, taskID: *taskID})...)
		registry.Register(mcp.NewRecreateSandboxTools(
			daemonSandbox{client: client, taskID: *taskID})...)
	}

	// The self-debug capability's own tools: a run gets them exactly
	// when a human attached the self-debug grant to its task, which is
	// what -self-debug says (see that flag, and agent.RunConfig.SelfDebug
	// for how a Framework learns it). Everything they expose is
	// read-only -- grain's own source -- so there is no confirmation
	// step, matching pkg/capability/selfdebug and unlike
	// pkg/capability/selfrepair.
	//
	// A deployment with no checkout of grain's source to point at still
	// gets both tools, each refusing the call with a sentence saying so
	// rather than being absent from the roster (selfdebug.SourceTools'
	// own doc comment), so a run's tool roster stays a property of the
	// vocabulary rather than of one deployment's configuration.
	if *selfDebug {
		registry.Register(selfdebug.SourceTools(*grainSrcDir)...)
	}

	// Serve returns io.EOF once its caller closes the write end of our
	// stdin -- the ordinary way a client signals "done", not a failure.
	// This process's own lifetime is the cancellation boundary: whatever
	// spawned it (claude -p's own --mcp-config fork, in production) is
	// killed as a whole when a run is cancelled, which takes this stdio
	// server down with it -- see Serve's own doc comment on why that
	// makes context.Background() the right ctx here, not a derived one.
	if err := mcp.Serve(context.Background(), registry, bufio.NewReader(os.Stdin), bufio.NewWriter(os.Stdout)); err != nil && !errors.Is(err, io.EOF) {
		log.Fatalf("grain mcpserver: %v", err)
	}
}

// parsedRunDeadline reads -run-deadline, or the zero time for a server
// that was given none -- which mcp.Registry.AnnounceDeadline takes as
// "say nothing about time", the way every caller with no deadline to
// give (pkg/mcp's own tests, tests/e2e, a `grain mcpserver` run by hand)
// already works.
//
// A value that will not parse is a warning on stderr and no more, for
// the reason pullRequestTools below refuses to be fatal: every other
// tool this process serves is how the agent touches its sandbox at all,
// and exiting over a malformed timestamp would turn a wrong reminder
// into a run that cannot edit a file. The daemon's own subprocess
// plumbing carries that line to an operator.
func parsedRunDeadline(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"grain mcpserver: -run-deadline %q is not an RFC3339 time (%v); this run's tool "+
				"results will not say how long it has left\n", value, err)
		return time.Time{}
	}
	return at
}

// pullRequestTools builds pull_request_status and wait_for_checks for
// repo/branch, reading
// GitHub with the credential ladder under dataDir -- the very files
// `grain daemon` loads for its own REST client (its credentialTokenSource
// call site), opened a second time here because this is a separate
// process with no route back into that one.
//
// Nothing here is fatal, and that is deliberate: every other tool this
// process serves is how the agent touches its sandbox at all, so exiting
// because CI happens to be unreadable would turn a missing credential
// into a run that cannot edit a file. mcp.NewPullRequestTools registers
// both tools with a nil reader instead, which answers any call with a
// sentence saying there is nothing to report -- and the reason goes to
// stderr, where the daemon's own subprocess plumbing already carries it
// to an operator.
func pullRequestTools(dataDir, githubHost string, insecureHTTP bool, repo, branch string) []mcp.Tool {
	client, scope, err := pullRequestReader(dataDir, githubHost, insecureHTTP, repo, branch)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"grain mcpserver: pull_request_status and wait_for_checks are unavailable this run: %v\n", err)
		return mcp.NewPullRequestTools(nil, mcp.PullRequestScope{})
	}
	return mcp.NewPullRequestTools(client, scope)
}

// pullRequestReader resolves the flags into a client and the one branch
// it may be asked about. An unset -pr-repo is not an error but a task
// with no repo attached (orchestrator.BuildPrompt has its own sentence
// for that case), so it returns a nil client and no error at all --
// there is nothing to warn an operator about.
func pullRequestReader(dataDir, githubHost string, insecureHTTP bool, repo, branch string) (mcp.PullRequestReader, mcp.PullRequestScope, error) {
	if repo == "" {
		return nil, mcp.PullRequestScope{}, nil
	}
	if branch == "" {
		return nil, mcp.PullRequestScope{}, fmt.Errorf("-pr-branch is required with -pr-repo")
	}
	if dataDir == "" {
		return nil, mcp.PullRequestScope{}, fmt.Errorf("-data-dir is required with -pr-repo")
	}
	ref, err := model.ParseRepo(repo)
	if err != nil {
		return nil, mcp.PullRequestScope{}, fmt.Errorf("-pr-repo %q: %w", repo, err)
	}
	credentials, err := gitproxy.LoadCredentialSet(filepath.Join(dataDir, "secrets", "github"))
	if err != nil {
		return nil, mcp.PullRequestScope{}, fmt.Errorf("loading the GitHub credential ladder: %w", err)
	}
	transport := github.NewRealTransport(githubHost)
	transport.UseTLS = !insecureHTTP
	client := github.NewClient(transport, credentialTokenSource{credentials})
	return client, mcp.PullRequestScope{Owner: ref.Owner, Repo: ref.Name, Branch: branch}, nil
}

// mustKonturSandboxTools builds the sandbox tools against konturVM's
// guest, exiting the process (matching the -sandbox-root checks above) if
// any required flag is missing -- there is no useful degraded mode for an
// mcpserver process that can't reach the sandbox it was told to run
// against.
//
// Unlike the SSH transport this replaced, there is nothing to resolve
// first: the tools reach the guest by exec'ing into konturVM's own
// container (mcp.DockerExecRunner), whose name follows from the VM name
// alone, so no port, pod IP or state file lookup stands between this and
// a usable runner.
func mustKonturSandboxTools(konturVM, sshUser, execKey, workspace string) []mcp.Tool {
	if sshUser == "" {
		fmt.Fprintln(os.Stderr, "grain mcpserver: -ssh-user is required with -kontur-vm")
		os.Exit(2)
	}
	if workspace == "" {
		fmt.Fprintln(os.Stderr, "grain mcpserver: -workspace is required with -kontur-vm")
		os.Exit(2)
	}

	runner := &mcp.DockerExecRunner{
		Container: kontur.PodName(konturVM),
		User:      sshUser,
		KeyPath:   execKey,
	}
	return mcp.NewSSHSandboxTools(runner, workspace)
}
