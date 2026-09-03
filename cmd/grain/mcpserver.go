// mcpserver.go implements `grain mcpserver`, formerly its own
// cmd/mcpserver binary before bwsalmon/agents#313 combined every mode
// into one: it serves the sandbox tools and the mocked GitHub-shaped
// escape hatches over its own stdin/stdout, so anything that can spawn a
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
// -pr-repo/-pr-branch add pkg/mcp's pull_request_status to that roster,
// so a run can read CI's verdict on the commits it pushes and repair a
// red build inside its own turn budget instead of leaving it for the
// merge queue's separate fix task. That read happens *here*, in this
// process, which runs on the controller: the GitHub credential comes off
// the controller's own -data-dir (the same secrets/github ladder
// `grain daemon` loads) and never crosses into the sandbox, so
// docs/design.md's split surface -- "Sandboxes: git transport only" --
// is untouched. See pkg/mcp/pullrequest_tools.go's own doc comment.
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

	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/kontur"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
)

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
		"owner/name of the repository pull_request_status reports on. Unset leaves the tool "+
			"registered but answering that this run has no repo, which is what a task with no "+
			"target really does have.")
	prBranch := fs.String("pr-branch", "",
		"branch within -pr-repo pull_request_status reports on -- this run's own branch, and the "+
			"only one it can ever read (required with -pr-repo)")
	githubHost := fs.String("github-host", "github.com",
		"git host whose REST API pull_request_status reads (github.APIHost maps it to the API host)")
	githubInsecureHTTP := fs.Bool("github-insecure-http", false,
		"reach -github-host over plain HTTP instead of HTTPS -- for a local mock GitHub in a "+
			"live test, never for a real deployment")
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
	registry.Register(tools...)
	// No controller reads a mocked GitHub-shaped tool call back out of this
	// process today -- there is nothing downstream of it yet (see README.md).
	// The sink exists so calls fail loudly (a real Result, not a silent
	// no-op) rather than because there's anywhere for it to report to.
	registry.Register(mcp.NewMockTools(&mcp.MockSink{})...)
	// Unlike those, this one is not mocked: it really does read GitHub,
	// from this process, on the controller. See the file's doc comment.
	registry.Register(pullRequestTools(*dataDir, *githubHost, *githubInsecureHTTP, *prRepo, *prBranch)...)

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

// pullRequestTools builds pull_request_status for repo/branch, reading
// GitHub with the credential ladder under dataDir -- the very files
// `grain daemon` loads for its own REST client (its credentialTokenSource
// call site), opened a second time here because this is a separate
// process with no route back into that one.
//
// Nothing here is fatal, and that is deliberate: every other tool this
// process serves is how the agent touches its sandbox at all, so exiting
// because CI happens to be unreadable would turn a missing credential
// into a run that cannot edit a file. mcp.NewPullRequestTools registers
// the tool with a nil reader instead, which answers any call with a
// sentence saying there is nothing to report -- and the reason goes to
// stderr, where the daemon's own subprocess plumbing already carries it
// to an operator.
func pullRequestTools(dataDir, githubHost string, insecureHTTP bool, repo, branch string) []mcp.Tool {
	client, scope, err := pullRequestReader(dataDir, githubHost, insecureHTTP, repo, branch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grain mcpserver: pull_request_status is unavailable this run: %v\n", err)
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
