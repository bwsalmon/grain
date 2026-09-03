// Command grain is the one binary this repo builds (bwsalmon/agents#313):
// given no recognized subcommand, or one of the task-management verbs
// below, it is a standalone CLI over pkg/ui's HTTPClient -- a plain REST
// client of the same JSON API (`grain daemon`'s own embedded `pkg/ui.Server`)
// the browser frontend speaks, driven straight from a terminal instead.
// It lets the caller do what a human does to a task: create one, edit its
// fields, attach or detach a capability, accept (approve) a proposal,
// comment, and close (grain's own stand-in for "delete" -- see
// Client.Close's own doc comment for why) or reopen one.
//
// This used to open the task store directly -- embedded, or a Dolt SQL
// server via -store-addr -- and call pkg/ui.Client's methods in-process,
// which made the CLI a second direct writer against whatever daemon a
// deployment also runs (README.md used to carry a whole "Single
// writer" section about exactly that). bwsalmon/agents#363 closed that
// gap: the daemon is the only thing that ever opens the store now, and
// serves pkg/ui's JSON API and static frontend itself (daemon.go's own
// doc comment); this talks to that API over plain HTTP, wherever an
// operator's network puts it -- a loopback port, an SSH tunnel,
// Tailscale, IAP. -server names it; there is no GitHub credential and no
// store flag here at all any more.
//
// Other subcommands -- daemon, mcpserver, secrets, controller, setup,
// sync -- select entirely different modes, each what used to be its own
// binary, its own flag or store carve-out, or a later addition with no
// business touching the task store at all. daemon (daemon.go, formerly
// cmd/graind) runs pkg/orchestrator's RunCycle on a timer and, unless
// -ui-addr is emptied out, serves the UI/API in-process too -- the
// standalone "ui" subcommand this project used to have is gone,
// folded into daemon by #363 rather than kept as a second way to open
// the same store. mcpserver (mcpserver.go, formerly cmd/mcpserver)
// speaks MCP over stdin/stdout against the sandbox tools; a running
// daemon forks mcpserver instances of itself -- this same binary,
// re-invoked with "mcpserver" as argv[1] -- rather than needing a second
// binary on disk (see pkg/agent/claude's own doc comment for the one
// place that matters today). secrets (bwsalmon/agents#357, secrets.go)
// sets/deletes/lists a colocated server's secrets directly on disk, no
// store or RPC involved. controller (controller.go) runs one-time
// interactive setup verbs (currently just bootstrap-github-app) against
// an operator's own machine, never a running daemon. setup and sync
// (bwsalmon/agents#358, setup.go and sync.go) bootstrap and reconcile a
// deployment's external GCP infrastructure and, for sync, its stored
// settings too -- sync reaches those over the same REST API this CLI
// does now, rather than opening the store itself. "demo" (demo.go) is a
// smaller mode still: a throwaway UI server over fake data, for trying
// out the frontend with no daemon, no store and no deployment behind it
// at all.
//
// The task CLI itself takes no GitHub credentials at all. A task used to
// be a GitHub issue, so this command needed a token and a task repo to
// file one; a task is a store row now, and creating one is a write to
// the store (README, "Input is a model update, not a GitHub issue").
//
// Folder management (docs/data-model.md's Folder tree, the containment
// structure a capability's `offers` are attached to) is deliberately
// absent: README.md already states folders are unbuilt, so there is no
// store-backed concept yet for a command here to manage. What a repo
// *does* have stored -- its own default capability set, and whether the
// deployment's allowlist names it -- is `grain repo` (repo.go, grain/
// task-36), which is deployment configuration in the same sense `grain
// settings` and `grain secrets` are, not the containment tree. -repo on
// `create`/`update` still only ever sets one task's own target.
//
// Schedules, templates, suites and qualification plans remain UI-only,
// and repo.go's own doc comment has why that is not the same question:
// they are authored content rather than deployment configuration, and
// docs/scheduled-tasks.md records their absence here as an open gap
// waiting on somebody who needs it.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/ui"
)

// main dispatches on argv[1] before parsing anything else: "daemon",
// "demo", "mcpserver", "secrets", "controller", "setup" and "sync" are
// modes with their own, unrelated flag sets, so they are matched exactly
// and handed the rest of argv verbatim rather than folded into runCLI's
// own flag.FlagSet. Anything else -- including no arguments at all, or a
// leading global flag like -json -- falls through to the task CLI
// exactly as it always has, so a task command itself is never allowed to
// collide with one of the mode names above (none of the ones below do).
func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "daemon":
			daemon(args[1:])
			return
		case "demo":
			demo(args[1:])
			return
		case "mcpserver":
			mcpserver(args[1:])
			return
		case "secrets":
			secretsCmd(args[1:])
			return
		case "controller":
			controller(args[1:])
			return
		case "setup":
			setupCmd(args[1:])
			return
		case "sync":
			syncCmd(args[1:])
			return
		case "schema-version":
			schemaVersionCmd(args[1:])
			return
		case "sandbox-image":
			sandboxImageCmd(args[1:])
			return
		}
	}
	if err := runCLI(args); err != nil {
		fmt.Fprintln(os.Stderr, "grain: "+err.Error())
		os.Exit(1)
	}
}

const usage = `usage: grain [global flags] <command> [args]
       grain daemon [flags]    run pkg/orchestrator's RunCycle on a timer, and serve the UI/API (see daemon.go)
       grain demo [flags]      serve the task UI on localhost over a throwaway store of fake tasks (see demo.go)
       grain mcpserver [flags] speak MCP over stdin/stdout against the sandbox tools (see mcpserver.go)
       grain secrets [flags]   set/delete/list a colocated server's secrets on disk (see secrets.go)
       grain controller bootstrap-github-app [flags]  one-time interactive setup for the github-sandbox capability (see controller.go)
       grain setup gcp [flags] bootstrap external GCP infrastructure for a new installation (see setup.go)
       grain sync [flags]      reconcile a live deployment's settings and/or GCP infrastructure from a config file (see sync.go)
       grain schema-version    print pkg/model.SchemaVersion and exit (see schemaversion.go)
       grain sandbox-image     print the sandbox container this build expects and exit (see sandboximage.go)

Global flags (must come before the command):
  -server string  base URL of a running "grain daemon"'s UI/API. Defaults to
                  $GRAIN_SERVER when that is set, else http://127.0.0.1:8420 --
                  a host installed by scripts/setup.sh exports it already
  -json           print machine-readable JSON instead of a human-readable table

Commands:
  list                                 list every task
  get <id>                             show one task and its conversation
  create -title T [flags]              file a new task
  update <id> [flags]                  edit a task's title or fields
  approve <id>                         accept a proposed task (proposed -> queued)
  capability <id> <cap> attach|detach  attach or detach a capability
  comment <id> <body...>               post a comment (and answer a parked question)
  close <id>                           close a task (grain's "delete" -- see the package doc comment)
  delete <id>                          alias for close
  reopen <id>                          reopen a closed task
  retry <id>                           clear a failed task's retry cap so it dispatches again
  config                               show the capabilities this deployment offers
  settings [flags]                     show, or change, the daemon's stored configuration (bwsalmon/agents#320)
  repo <subcommand> [args]             list repos, and read or change one repo's own settings (see repo.go)
  metrics [-window 7d]                 throughput and latency over a window (see metrics.go)
`

const defaultServerURL = "http://127.0.0.1:8420"

// serverEnvVar overrides defaultServerURL, so a deployment serving the
// UI on any other port does not make every CLI invocation on its own
// host carry a -server flag. A deployment sets it once, for every shell
// on the box, rather than each operator remembering the port: scripts/
// setup.sh writes /etc/profile.d/grain.sh from the -ui-addr it started
// the daemon with.
//
// An explicit -server still wins -- see serverDefault, which supplies
// this only as the flag's default.
const serverEnvVar = "GRAIN_SERVER"

// serverDefault is what -server defaults to: GRAIN_SERVER if it is set
// to anything non-empty, defaultServerURL otherwise. Empty is treated as
// unset rather than as a request to talk to no server at all, since an
// exported-but-empty variable is a broken profile script, not an
// intention.
func serverDefault() string {
	if v := strings.TrimSpace(os.Getenv(serverEnvVar)); v != "" {
		return v
	}
	return defaultServerURL
}

func runCLI(args []string) error {
	fs := flag.NewFlagSet("grain", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	server := fs.String("server", serverDefault(), "base URL of a running \"grain daemon\"'s UI/API ($"+serverEnvVar+" overrides the default)")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON instead of a human-readable table")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("a command is required")
	}

	c := ui.NewHTTPClient(*server)
	out := &printer{json: *jsonOutput}
	ctx := context.Background()

	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "list":
		return cmdList(ctx, c, out, cmdArgs)
	case "get":
		return cmdGet(ctx, c, out, cmdArgs)
	case "create":
		return cmdCreate(ctx, c, out, cmdArgs)
	case "update":
		return cmdUpdate(ctx, c, out, cmdArgs)
	case "approve":
		return cmdApprove(ctx, c, out, cmdArgs)
	case "capability":
		return cmdCapability(ctx, c, out, cmdArgs)
	case "comment":
		return cmdComment(ctx, c, out, cmdArgs)
	case "close", "delete":
		return cmdClose(ctx, c, out, cmdArgs)
	case "reopen":
		return cmdReopen(ctx, c, out, cmdArgs)
	case "retry":
		return cmdRetry(ctx, c, out, cmdArgs)
	case "config":
		return cmdConfig(ctx, c, out, cmdArgs)
	case "metrics":
		return cmdMetrics(ctx, c, out, cmdArgs)
	case "settings":
		return cmdSettings(ctx, c, out, cmdArgs)
	case "repo":
		return cmdRepo(ctx, c, out, cmdArgs)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// actorID resolves who a local, non-REST command (currently just "demo")
// acts as: the -as flag, else the OS user, else ui.DefaultActor's own
// fallback. The task CLI itself no longer has a notion of this: attribution
// is the daemon's own deployment-wide setting now (pkg/ui's HTTPClient.Config
// doc comment), not something a REST caller can override per request.
func actorID(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return ""
}

// taskID reads the task id positional argument. It is not parsed as a
// number: ids are decimal today because that is what Store.NewTaskID
// allocates, but Task.ID is an opaque string and nothing here is entitled
// to assume otherwise.
func taskID(s string) (string, error) {
	if s == "" {
		return "", errors.New("a task id is required")
	}
	return s, nil
}

// respond re-reads number and prints it -- what every mutating command
// below does once its own call to Client succeeds, the CLI's analogue of
// pkg/ui's own respondWithTask.
func respond(ctx context.Context, c *ui.HTTPClient, out *printer, id string) error {
	task, err := c.Task(ctx, id)
	if err != nil {
		return err
	}
	out.task(task)
	return nil
}

func cmdList(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		return err
	}
	out.tasks(tasks)
	return nil
}

func cmdGet(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain get", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := taskID(fs.Arg(0))
	if err != nil {
		return err
	}
	detail, err := c.GetTask(ctx, id)
	if err != nil {
		return err
	}
	out.detail(detail)
	return nil
}

// stringList is a repeatable flag.Value -- one -capability per capability
// ID, rather than a single comma-joined flag a capability ID containing
// a comma could never round-trip through.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdCreate(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain create", flag.ContinueOnError)
	title := fs.String("title", "", "task title (required)")
	body := fs.String("body", "", "task description")
	repo := fs.String("repo", "", "owner/name of the repo this task's work targets")
	noRepo := fs.Bool("no-repo", false, "file this task with no repo at all, rather than falling back to the deployment default")
	base := fs.String("base", "", "base branch, if not the target repo's default")
	var autoMerge bool
	fs.BoolVar(&autoMerge, "auto-merge", false, "merge this task's pull request automatically once it reads clean")
	var capabilities stringList
	fs.Var(&capabilities, "capability", "capability ID to attach (repeatable)")
	var reads stringList
	fs.Var(&reads, "read", "owner/name of a repo this task's run may read but never push to (repeatable)")
	approve := fs.Bool("approve", false, "queue the task immediately instead of filing it as a proposal awaiting approval")
	interactive := fs.Bool("interactive", false, "file this as a live chat rather than a change to run unattended (bwsalmon/agents#539); implies -approve and dispatches ahead of the backlog")
	var attach stringList
	fs.Var(&attach, "attach", "path to a local file to attach to the task (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	attachments, err := loadAttachments(attach)
	if err != nil {
		return err
	}
	req := ui.CreateTaskRequest{
		Title: *title, Description: *body, Repo: *repo, NoRepo: *noRepo, Base: *base,
		AutoMerge: autoMerge, Reads: reads, Approved: *approve,
		Interactive: *interactive, Attachments: attachments,
	}
	// Naming any -capability names the whole set (ui.CreateTaskRequest.
	// Capabilities); naming none leaves the field unset, so the task is
	// filed with whatever this deployment attaches by default -- the same
	// answer the UI's own new-task form starts from. To file one with
	// nothing at all on a deployment that defaults something, detach it
	// afterwards (`grain capability <task> <cap> detach`).
	if len(capabilities) > 0 {
		ids := []string(capabilities)
		req.Capabilities = &ids
	}

	task, err := c.CreateTask(ctx, req)
	if err != nil {
		return err
	}
	out.task(task)
	return nil
}

func cmdUpdate(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain update", flag.ContinueOnError)
	title := fs.String("title", "", "new task title")
	body := fs.String("body", "", "new task description")
	repo := fs.String("repo", "", "new target repo (owner/name)")
	base := fs.String("base", "", "new base branch")
	var autoMerge bool
	fs.BoolVar(&autoMerge, "auto-merge", false, "whether the task auto-merges once clean")
	var reads stringList
	fs.Var(&reads, "read", "owner/name of a repo this task's run may read but never push to (repeatable) -- replaces the whole set")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := taskID(fs.Arg(0))
	if err != nil {
		return err
	}

	var req ui.UpdateTaskRequest
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "title":
			v := *title
			req.Title = &v
		case "body":
			v := *body
			req.Description = &v
		case "repo":
			v := *repo
			req.Repo = &v
		case "base":
			v := *base
			req.Base = &v
		case "auto-merge":
			v := autoMerge
			req.AutoMerge = &v
		case "read":
			v := []string(reads)
			req.Reads = &v
		}
	})

	task, err := c.UpdateTask(ctx, id, req)
	if err != nil {
		return err
	}
	out.task(task)
	return nil
}

func cmdApprove(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain approve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := taskID(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := c.Approve(ctx, id); err != nil {
		return err
	}
	return respond(ctx, c, out, id)
}

func cmdCapability(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain capability", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 3 {
		return errors.New("usage: grain capability <id> <capability-id> attach|detach")
	}
	id, err := taskID(fs.Arg(0))
	if err != nil {
		return err
	}
	capability := fs.Arg(1)
	var attach bool
	switch fs.Arg(2) {
	case "attach":
		attach = true
	case "detach":
		attach = false
	default:
		return fmt.Errorf("third argument must be attach or detach, got %q", fs.Arg(2))
	}
	if err := c.SetCapability(ctx, id, capability, attach); err != nil {
		return err
	}
	return respond(ctx, c, out, id)
}

func cmdComment(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain comment", flag.ContinueOnError)
	var attach stringList
	fs.Var(&attach, "attach", "path to a local file to attach to the comment (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: grain comment [-attach path]... <id> [body...]")
	}
	id, err := taskID(fs.Arg(0))
	if err != nil {
		return err
	}
	body := strings.Join(fs.Args()[1:], " ")
	attachments, err := loadAttachments(attach)
	if err != nil {
		return err
	}
	if body == "" && len(attachments) == 0 {
		return errors.New("usage: grain comment [-attach path]... <id> [body...] -- a body, an attachment, or both is required")
	}
	if err := c.AddComment(ctx, id, body, attachments); err != nil {
		return err
	}
	return respond(ctx, c, out, id)
}

// loadAttachments reads each path in paths off local disk and returns it
// as the base64 upload CreateTaskRequest/addCommentRequest send over the
// wire -- grain create/comment's own -attach flag's encoding step, so a
// human filing a task from the CLI can hand the agent a screenshot or a
// repro zip the same way the web UI's file picker does.
func loadAttachments(paths []string) ([]ui.AttachmentUpload, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make([]ui.AttachmentUpload, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading attachment %s: %w", p, err)
		}
		contentType := mime.TypeByExtension(filepath.Ext(p))
		if contentType == "" {
			contentType = http.DetectContentType(data)
		}
		out = append(out, ui.AttachmentUpload{
			Filename:    filepath.Base(p),
			ContentType: contentType,
			Content:     base64.StdEncoding.EncodeToString(data),
		})
	}
	return out, nil
}

func cmdClose(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain close", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := taskID(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := c.Close(ctx, id); err != nil {
		return err
	}
	return respond(ctx, c, out, id)
}

func cmdReopen(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain reopen", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := taskID(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := c.Reopen(ctx, id); err != nil {
		return err
	}
	return respond(ctx, c, out, id)
}

// cmdRetry is `grain retry <id>`: the only way to clear a task's own
// failure streak once it has reached model.MaxConsecutiveFailures and
// state has stopped calling it "queued" at all (bwsalmon/agents#403).
func cmdRetry(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain retry", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := taskID(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := c.Retry(ctx, id); err != nil {
		return err
	}
	return respond(ctx, c, out, id)
}

func cmdConfig(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain config", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := c.Config(ctx)
	if err != nil {
		return err
	}
	out.config(cfg)
	return nil
}

// cmdSettings shows, or changes, the daemon's own store-backed
// configuration (bwsalmon/agents#320, ui.Settings/model.Config) -- with
// no flags given it prints what is currently stored (or that nothing is
// yet, on a store no daemon has ever started against); with any given,
// it applies only those, the same fs.Visit convention cmdUpdate uses for
// a task's own fields, and prints the result.
func cmdSettings(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain settings", flag.ContinueOnError)
	pollInterval := fs.String("poll-interval", "", "how often the daemon runs a reconcile cycle, e.g. 30s")
	maxWorkers := fs.Int("max-workers", 0, "maximum number of ordinary tasks dispatched at once")
	maxMergers := fs.Int("max-mergers", 0, "capacity on top of -max-workers only the merge queue's own fix tasks may use (0 lets them contend for it like anything else)")
	geminiModel := fs.String("gemini-model", "", "Gemini model the antigravity agent framework calls")
	claudeModel := fs.String("claude-model", "", "Claude model the claude agent framework calls")
	maxAgentTurns := fs.Int("max-agent-turns", 0, "cap on model/tool round trips per run (0 = uncapped; runs are bounded by wall-clock runtime instead)")
	githubHost := fs.String("github-host", "", "GitHub API host (the one setting here that needs a daemon restart to take effect)")
	var githubInsecureHTTP bool
	fs.BoolVar(&githubInsecureHTTP, "github-insecure-http", false, "speak plain HTTP to -github-host instead of HTTPS (needs a daemon restart to take effect)")
	gcpProject := fs.String("gcp-project", "", "GCP project the gcp-key/gemini-key capabilities mint into")
	gcpServiceAccountEmail := fs.String("gcp-agent-service-account", "", "the narrow agent service account gcp-key mints keys for")
	// The deployment-wide sandbox VM shape (ui.Settings.SandboxCPUs/
	// SandboxMemoryMB/SandboxDiskGB, bwsalmon/agents#534,
	// grain/task-41). 0 is a real value for all three -- "leave the
	// default in place" -- so an operator shrinking a deployment back to
	// that default sets the flag to 0 rather than omitting it, the same
	// way an empty -target-repos clears the allowlist. What that default
	// is differs for disk: kontur names its own vCPU and memory
	// defaults, but a VM's disk is however large the guest image behind
	// it happens to be, which is a property of the image a deployment
	// built rather than a constant this build could print.
	sandboxCPUs := fs.Int("sandbox-cpus", 0,
		"deployment-wide default vCPU count for a kontur-managed sandbox VM; 0 leaves bwsalmon/kontur's own default in place")
	sandboxMemoryMB := fs.Int("sandbox-memory-mb", 0,
		"deployment-wide default guest memory, in MiB, for a kontur-managed sandbox VM; 0 leaves bwsalmon/kontur's own default in place")
	sandboxDiskGB := fs.Int("sandbox-disk-gb", 0,
		"deployment-wide default root disk size, in GiB, for a kontur-managed sandbox VM; 0 leaves the VM's disk as large as the guest image behind it")
	// No back quotes in this usage string, here or on any flag below:
	// flag.PrintDefaults reads the first back-quoted word as the name of
	// the flag's operand, so "`grain repo add`" would print as
	// "-target-repos grain repo add".
	targetRepos := fs.String("target-repos", "",
		"comma-separated owner/name list a task's repo may name -- empty allows any; replaces the whole list, where \"grain repo add\"/\"remove\" change one entry")
	defaultCapabilities := fs.String("default-capabilities", "",
		"comma-separated capability IDs every new task is filed holding -- empty files each task with only what it asks for")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var req ui.UpdateSettingsRequest
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "poll-interval":
			v := *pollInterval
			req.PollInterval = &v
		case "max-workers":
			v := *maxWorkers
			req.MaxWorkers = &v
		case "max-mergers":
			v := *maxMergers
			req.MaxMergers = &v
		case "gemini-model":
			v := *geminiModel
			req.GeminiModel = &v
		case "claude-model":
			v := *claudeModel
			req.ClaudeModel = &v
		case "max-agent-turns":
			v := *maxAgentTurns
			req.MaxAgentTurns = &v
		case "github-host":
			v := *githubHost
			req.GitHubHost = &v
		case "github-insecure-http":
			v := githubInsecureHTTP
			req.GitHubInsecureHTTP = &v
		case "gcp-project":
			v := *gcpProject
			req.GCPProject = &v
		case "gcp-agent-service-account":
			v := *gcpServiceAccountEmail
			req.GCPServiceAccountEmail = &v
		case "sandbox-cpus":
			v := *sandboxCPUs
			req.SandboxCPUs = &v
		case "sandbox-memory-mb":
			v := *sandboxMemoryMB
			req.SandboxMemoryMB = &v
		case "sandbox-disk-gb":
			v := *sandboxDiskGB
			req.SandboxDiskGB = &v
		case "target-repos":
			v := splitRepoList(*targetRepos)
			req.TargetRepos = &v
		case "default-capabilities":
			// splitRepoList is a comma-separated list with "" meaning
			// none, which is exactly what this needs too -- an empty
			// -default-capabilities is how an operator turns the default
			// set back off, the same way an empty -target-repos clears
			// the allowlist.
			v := splitRepoList(*defaultCapabilities)
			req.DefaultCapabilities = &v
		}
	})

	if fs.NFlag() == 0 {
		settings, err := c.GetSettings(ctx)
		if err != nil {
			return err
		}
		out.settings(settings)
		return nil
	}
	settings, err := c.UpdateSettings(ctx, req)
	if err != nil {
		return err
	}
	out.settings(settings)
	return nil
}

// splitRepoList parses a comma-separated owner/name list -- "" means
// none, unlike strings.Split's own [""], since an empty -target-repos is
// how an operator clears the restriction back to unrestricted.
func splitRepoList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// printer renders CLI output either as a human-readable table (the
// default) or as JSON (-json) -- the same shape a script or another
// program (the UI included, per this command's own doc comment) would
// want instead of text meant for a terminal.
type printer struct{ json bool }

func (p *printer) tasks(tasks []ui.Task) {
	if p.json {
		p.encode(tasks)
		return
	}
	for _, t := range tasks {
		fmt.Println(taskLine(t))
	}
}

func (p *printer) task(t ui.Task) {
	if p.json {
		p.encode(t)
		return
	}
	fmt.Print(taskBlock(t))
}

func (p *printer) detail(d ui.TaskDetail) {
	if p.json {
		p.encode(d)
		return
	}
	fmt.Print(taskBlock(d.Task))
	if d.FailedAttempts > 0 {
		fmt.Printf("failed attempts: %d in a row", d.FailedAttempts)
		if d.LastFailureAt != nil {
			fmt.Printf(" (last at %s)", d.LastFailureAt.Format(time.RFC3339))
		}
		fmt.Println()
		if d.LastFailureReason != "" {
			fmt.Printf("last failure:    %s\n", d.LastFailureReason)
		}
	}
	if len(d.Transitions) > 0 {
		fmt.Println("\nhistory:")
		for _, tr := range d.Transitions {
			fmt.Printf("  -> %-14s %s\n", tr.State, tr.At.Format(time.RFC3339))
		}
	}
	for _, cm := range d.Comments {
		who := cm.Author
		if cm.OnBehalfOf != "" {
			// Grain relaying somebody else's words -- the distinction
			// model.Attribution exists to carry.
			who = fmt.Sprintf("%s on behalf of %s", cm.Author, cm.OnBehalfOf)
		}
		fmt.Printf("\n--- comment #%d by %s ---\n%s\n", cm.ID, who, cm.Body)
	}
}

func (p *printer) config(cfg ui.Config) {
	if p.json {
		p.encode(cfg)
		return
	}
	fmt.Printf("acting as: %s (%s)\n", cfg.Actor.ID, cfg.Actor.Kind)
	if cfg.DefaultTarget != nil {
		fmt.Printf("default target: %s\n", cfg.DefaultTarget)
	}
	fmt.Println("\ncapabilities:")
	for _, cp := range cfg.Capabilities {
		fmt.Printf("  %-14s %-20s %s\n", cp.ID, cp.Name, cp.Description)
	}
}

func (p *printer) settings(s ui.Settings) {
	if p.json {
		p.encode(s)
		return
	}
	if !s.Configured {
		fmt.Println("not configured yet -- nothing here until a daemon starts, or a value is set")
		return
	}
	fmt.Printf("poll interval:  %s\n", s.PollInterval)
	fmt.Printf("max workers:    %d\n", s.MaxWorkers)
	fmt.Printf("max mergers:    %d\n", s.MaxMergers)
	fmt.Printf("gemini model:   %s\n", s.GeminiModel)
	fmt.Printf("claude model:   %s\n", s.ClaudeModel)
	fmt.Printf("max agent turns: %d\n", s.MaxAgentTurns)
	fmt.Printf("github host:    %s\n", s.GitHubHost)
	fmt.Printf("github insecure http: %t\n", s.GitHubInsecureHTTP)
	if s.GCPProject != "" {
		fmt.Printf("gcp project:    %s\n", s.GCPProject)
	}
	if s.GCPServiceAccountEmail != "" {
		fmt.Printf("gcp agent service account: %s\n", s.GCPServiceAccountEmail)
	}
	// The sandbox shape prints what is actually in effect, not the bare
	// stored value: 0 means "whatever bwsalmon/kontur defaults to", and
	// printing that literal 0 would read as a deliberately empty VM.
	// ui.Settings carries kontur's own defaults alongside the stored
	// values (SandboxCPUsDefault/SandboxMemoryMBDefault) for exactly
	// this.
	//
	// Disk has no such default to name, deliberately: there is no
	// ui.Settings.SandboxDiskGBDefault because a VM's disk is however
	// large the guest image behind it happens to be, a property of the
	// image a deployment built rather than a constant this build could
	// print. Passing 0 makes an unset disk print as "unset", which is
	// all that can honestly be said about it here.
	fmt.Printf("sandbox cpus:   %s\n", sandboxShapeValue(s.SandboxCPUs, s.SandboxCPUsDefault))
	fmt.Printf("sandbox memory mb: %s\n", sandboxShapeValue(s.SandboxMemoryMB, s.SandboxMemoryMBDefault))
	fmt.Printf("sandbox disk gb: %s\n", sandboxShapeValue(s.SandboxDiskGB, 0))
	if len(s.TargetRepos) > 0 {
		fmt.Printf("target repos:   %s\n", strings.Join(s.TargetRepos, ", "))
	} else {
		fmt.Println("target repos:   unrestricted")
	}
	if len(s.DefaultCapabilities) > 0 {
		fmt.Printf("default capabilities: %s\n", strings.Join(s.DefaultCapabilities, ", "))
	} else {
		fmt.Println("default capabilities: none")
	}
	// Every other setting above is already in effect: the daemon
	// re-reads this row each reconcile tick (cmd/grain/daemon.go's
	// liveConfig). These are the ones that are not, so saying so here is
	// the CLI's half of what the Settings pane annotates the field with
	// -- otherwise `grain settings -github-host=...` prints the new value
	// back and reads as though it had done something.
	if len(s.PendingRestart) > 0 {
		fmt.Printf("\nsaved, but not running yet -- restart the daemon to apply: %s\n",
			strings.Join(s.PendingRestart, ", "))
	}
	if len(s.Capabilities) > 0 {
		fmt.Println("\ncapabilities:")
		for _, cp := range s.Capabilities {
			fmt.Println(capabilityStatusLine(cp))
		}
	}
}

// sandboxShapeValue renders one dimension of the deployment-wide sandbox
// VM shape: the stored value when there is one, and otherwise the shape
// actually in effect -- bwsalmon/kontur's own default -- named as the
// default rather than printed as the bare 0 that is stored, so that
// "unset" and "in effect" are both legible from one line. A build whose
// ui.Settings reports no default to fall back on (0) says only that the
// setting is unset, which is all it can honestly say.
func sandboxShapeValue(stored, konturDefault int) string {
	if stored != 0 {
		return strconv.Itoa(stored)
	}
	if konturDefault != 0 {
		return fmt.Sprintf("%d (kontur default, unset)", konturDefault)
	}
	return "unset"
}

// capabilityStatusLine renders one ui.CapabilityStatus as a line of
// `grain settings` -- the CLI's half of the Settings pane's own
// Capabilities tab, printed here because "why did a task never get the
// thing it was granted" is a question asked from a shell on the host at
// least as often as from a browser, and until now the only answer
// available there was the two GCP fields above, which say nothing about
// secrets and nothing at all about the two gaps below.
//
// Both gaps are named, not just the configuration one, because they are
// fixed in different places and confusing them costs a debugging
// session: "needs"/"missing secrets" is this deployment (set it in
// Settings, or `grain secrets set`), while "NOT GRANTABLE" is grain's
// own code and cannot be configured around -- see
// ui.CapabilityStatus.Grantable.
func capabilityStatusLine(cp ui.CapabilityStatus) string {
	state := "not ready"
	if cp.Ready {
		state = "ready"
	}
	var notes []string
	// First, ahead of the gaps below, for the same reason the pane puts
	// "not grantable" first: a capability every task is filed holding is
	// the one whose "not ready" is a deployment-wide problem rather than
	// a per-task one.
	if cp.Default {
		notes = append(notes, "default -- every new task is filed with this")
	}
	// The per-repo layer, named as the repos it applies to rather than
	// as a second bare "default": with two layers, a line that only said
	// "default" would describe a deployment-wide default that only some
	// tasks actually get (ui.CapabilityStatus.DefaultRepos). Listed even
	// alongside the deployment-wide note above, since a repo can restate
	// one the deployment already gives and dropping it deployment-wide
	// leaves the repo's own entry standing.
	if len(cp.DefaultRepos) > 0 {
		notes = append(notes, "default in: "+strings.Join(cp.DefaultRepos, ", "))
	}
	if !cp.Grantable {
		notes = append(notes, "NOT GRANTABLE -- grain registers a provider for this, but no task can ask for it")
	}
	if len(cp.MissingConfig) > 0 {
		notes = append(notes, "needs: "+strings.Join(cp.MissingConfig, ", "))
	}
	if len(cp.MissingSecrets) > 0 {
		notes = append(notes, "missing secrets: "+strings.Join(cp.MissingSecrets, ", "))
	}
	line := fmt.Sprintf("  %-20s %-9s", cp.ID, state)
	if len(notes) > 0 {
		line += " " + strings.Join(notes, "; ")
	}
	return strings.TrimRight(line, " ")
}

func (p *printer) encode(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func taskLine(t ui.Task) string {
	repo := t.Repo
	if repo == "" {
		repo = "-"
	}
	return fmt.Sprintf("%-6s %-14s %-24s %s", t.ID, t.State, repo, t.Title)
}

func taskBlock(t ui.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", t.ID, t.Title)
	fmt.Fprintf(&b, "state:      %s\n", t.State)
	if t.Repo != "" {
		fmt.Fprintf(&b, "repo:       %s\n", t.Repo)
	}
	if t.Base != "" {
		fmt.Fprintf(&b, "base:       %s\n", t.Base)
	}
	if len(t.Reads) > 0 {
		fmt.Fprintf(&b, "reads:      %s\n", strings.Join(t.Reads, ", "))
	}
	fmt.Fprintf(&b, "auto-merge: %t\n", t.AutoMerge)
	if len(t.Capabilities) > 0 {
		fmt.Fprintf(&b, "capabilities: %s\n", strings.Join(t.Capabilities, ", "))
	}
	if t.PullRequest != "" {
		fmt.Fprintf(&b, "pull request: %s\n", t.PullRequest)
	}
	if t.MergeQueueBlockedAt != nil {
		fmt.Fprintf(&b, "merge queue blocked since: %s\n", t.MergeQueueBlockedAt.Format(time.RFC3339))
	}
	if t.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", t.Description)
	}
	return b.String()
}
