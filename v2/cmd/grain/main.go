// Command grain is a standalone CLI over pkg/ui's Client -- the same
// model code cmd/ui's Server wraps in JSON and HTTP, driven straight
// from a terminal instead. It takes GitHub credentials for a task repo
// and lets the caller do what a human does to a task issue by hand:
// create one, edit its declared fields, attach or detach a capability,
// accept (approve) a proposal, comment, and close (grain's own stand-in
// for "delete" -- see Client.Close's own doc comment for why) or reopen
// one. bwsalmon/agents#271 asks for exactly this, to be driven by the UI
// and other future callers alongside a person at a shell.
//
// Folder and repo *management* (docs/data-model.md's Folder tree, the
// containment structure a capability's `offers` are attached to) is
// deliberately absent: v2/README.md already states folders are unbuilt
// there is no store-backed concept yet for a command here to manage.
// -repo on `create`/`update` only ever sets one task's own /repo
// directive, which already existed in Client before this command did.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "grain: "+err.Error())
		os.Exit(1)
	}
}

const usage = `usage: grain [global flags] <command> [args]

Global flags (must come before the command):
  -task-repo string          owner/name of the GitHub repo task issues are filed against (required)
  -github-host string        GitHub API host (default "github.com")
  -github-insecure-http      speak plain HTTP to -github-host instead of HTTPS (mock servers only)
  -github-token-file string  file holding a GitHub token (falls back to $GITHUB_TOKEN, then unauthenticated)
  -dry-run                   print every GitHub mutation instead of making it
  -json                      print machine-readable JSON instead of a human-readable table

Commands:
  list                                     list every task
  get <number>                             show one task and its comments
  create -title T [flags]                  file a new task
  update <number> [flags]                  edit a task's title or declared fields
  approve <number>                         accept a proposed task (needs-approval -> queued)
  capability <number> <id> attach|detach   attach or detach a capability
  comment <number> <body...>               post a comment
  close <number>                           close a task (grain's "delete" -- see the package doc comment)
  delete <number>                          alias for close
  reopen <number>                          reopen a closed task
  config                                   show the label and capability taxonomy this deployment uses
`

func run(args []string) error {
	fs := flag.NewFlagSet("grain", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	taskRepo := fs.String("task-repo", "", "owner/name of the GitHub repo task issues are filed against (required)")
	githubHost := fs.String("github-host", "github.com", "GitHub API host -- override to point at a mock for local testing")
	githubInsecureHTTP := fs.Bool("github-insecure-http", false, "speak plain HTTP to -github-host instead of HTTPS (mock servers only)")
	githubTokenFile := fs.String("github-token-file", "", "file holding a GitHub token to authenticate as (falls back to $GITHUB_TOKEN, then unauthenticated)")
	dryRun := fs.Bool("dry-run", false, "print every GitHub mutation instead of making it")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON instead of a human-readable table")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("a command is required")
	}
	if *taskRepo == "" {
		return errors.New("-task-repo is required")
	}
	repo, err := model.ParseRepo(*taskRepo)
	if err != nil {
		return fmt.Errorf("-task-repo: %w", err)
	}

	client, err := buildClient(*githubHost, *githubInsecureHTTP, *githubTokenFile, *dryRun)
	if err != nil {
		return err
	}
	c := ui.NewClient(ui.Config{
		TaskRepo:     repo,
		Labels:       ui.DefaultLabels(),
		Capabilities: ui.DefaultCapabilities(),
	}, client)
	out := &printer{json: *jsonOutput}

	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "list":
		return cmdList(c, out, cmdArgs)
	case "get":
		return cmdGet(c, out, cmdArgs)
	case "create":
		return cmdCreate(c, out, cmdArgs)
	case "update":
		return cmdUpdate(c, out, cmdArgs)
	case "approve":
		return cmdApprove(c, out, cmdArgs)
	case "capability":
		return cmdCapability(c, out, cmdArgs)
	case "comment":
		return cmdComment(c, out, cmdArgs)
	case "close", "delete":
		return cmdClose(c, out, cmdArgs)
	case "reopen":
		return cmdReopen(c, out, cmdArgs)
	case "config":
		return cmdConfig(c, out, cmdArgs)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// buildClient resolves the GitHub token ladder (-github-token-file, then
// $GITHUB_TOKEN, then unauthenticated -- fine for a public repo, per
// github.NewClient's own doc comment) and wraps it in DryRunClient when
// -dry-run asks for one, the same split cmd/ui's own buildClient makes.
func buildClient(host string, insecureHTTP bool, tokenFile string, dryRun bool) (github.Client, error) {
	var token *string
	switch {
	case tokenFile != "":
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading -github-token-file: %w", err)
		}
		t := strings.TrimSpace(string(data))
		token = &t
	case os.Getenv("GITHUB_TOKEN") != "":
		t := os.Getenv("GITHUB_TOKEN")
		token = &t
	}

	transport := github.NewRealTransport(host)
	transport.UseTLS = !insecureHTTP
	var client github.Client = github.NewClient(transport, github.StaticToken{Token: token})
	if dryRun {
		client = github.DryRunClient{Inner: client}
	}
	return client, nil
}

func parseNumber(s string) (int, error) {
	if s == "" {
		return 0, errors.New("a task number is required")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid task number %q", s)
	}
	return n, nil
}

// respond re-reads number and prints it -- what every mutating command
// below does once its own call to Client succeeds, the CLI's analogue of
// pkg/ui's own respondWithTask.
func respond(c *ui.Client, out *printer, number int) error {
	task, err := c.Task(number)
	if err != nil {
		return err
	}
	out.task(task)
	return nil
}

func cmdList(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	tasks, err := c.ListTasks()
	if err != nil {
		return err
	}
	out.tasks(tasks)
	return nil
}

func cmdGet(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain get", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	number, err := parseNumber(fs.Arg(0))
	if err != nil {
		return err
	}
	detail, err := c.GetTask(number)
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

func cmdCreate(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain create", flag.ContinueOnError)
	title := fs.String("title", "", "task title (required)")
	body := fs.String("body", "", "task description")
	repo := fs.String("repo", "", "owner/name of the repo this task's work targets")
	base := fs.String("base", "", "base branch, if not the target repo's default")
	var autoMerge bool
	fs.BoolVar(&autoMerge, "auto-merge", false, "set the task's auto-merge directive")
	var capabilities stringList
	fs.Var(&capabilities, "capability", "capability ID to attach (repeatable)")
	approve := fs.Bool("approve", false, "queue the task immediately instead of filing it as a proposal needing approval")
	if err := fs.Parse(args); err != nil {
		return err
	}

	req := ui.CreateTaskRequest{
		Title: *title, Description: *body, Repo: *repo, Base: *base,
		Capabilities: capabilities, Approved: *approve,
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "auto-merge" {
			v := autoMerge
			req.AutoMerge = &v
		}
	})

	task, err := c.CreateTask(req)
	if err != nil {
		return err
	}
	out.task(task)
	return nil
}

func cmdUpdate(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain update", flag.ContinueOnError)
	title := fs.String("title", "", "new task title")
	body := fs.String("body", "", "new task description")
	repo := fs.String("repo", "", "new /repo directive (an empty string clears it)")
	base := fs.String("base", "", "new /base directive")
	var autoMerge bool
	fs.BoolVar(&autoMerge, "auto-merge", false, "new /auto-merge directive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	number, err := parseNumber(fs.Arg(0))
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
		}
	})

	task, err := c.UpdateTask(number, req)
	if err != nil {
		return err
	}
	out.task(task)
	return nil
}

func cmdApprove(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain approve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	number, err := parseNumber(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := c.Approve(number); err != nil {
		return err
	}
	return respond(c, out, number)
}

func cmdCapability(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain capability", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 3 {
		return errors.New("usage: grain capability <number> <capability-id> attach|detach")
	}
	number, err := parseNumber(fs.Arg(0))
	if err != nil {
		return err
	}
	id := fs.Arg(1)
	var attach bool
	switch fs.Arg(2) {
	case "attach":
		attach = true
	case "detach":
		attach = false
	default:
		return fmt.Errorf("third argument must be attach or detach, got %q", fs.Arg(2))
	}
	if err := c.SetCapability(number, id, attach); err != nil {
		return err
	}
	return respond(c, out, number)
}

func cmdComment(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain comment", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: grain comment <number> <body...>")
	}
	number, err := parseNumber(fs.Arg(0))
	if err != nil {
		return err
	}
	body := strings.Join(fs.Args()[1:], " ")
	if err := c.AddComment(number, body); err != nil {
		return err
	}
	return respond(c, out, number)
}

func cmdClose(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain close", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	number, err := parseNumber(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := c.Close(number); err != nil {
		return err
	}
	return respond(c, out, number)
}

func cmdReopen(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain reopen", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	number, err := parseNumber(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := c.Reopen(number); err != nil {
		return err
	}
	return respond(c, out, number)
}

func cmdConfig(c *ui.Client, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain config", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	out.config(c.Config)
	return nil
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
	for _, cm := range d.Comments {
		fmt.Printf("\n--- comment #%d by %s ---\n%s\n", cm.ID, cm.User, cm.Body)
	}
}

func (p *printer) config(cfg ui.Config) {
	if p.json {
		p.encode(cfg)
		return
	}
	fmt.Printf("task repo: %s\n\nlabels:\n", cfg.TaskRepo)
	fmt.Printf("  trigger:        %s\n", cfg.Labels.Trigger)
	fmt.Printf("  in-progress:    %s\n", cfg.Labels.InProgress)
	fmt.Printf("  awaiting-reply: %s\n", cfg.Labels.AwaitingReply)
	fmt.Printf("  needs-approval: %s\n", cfg.Labels.NeedsApproval)
	fmt.Printf("  completed:      %s\n", cfg.Labels.Completed)
	fmt.Println("\ncapabilities:")
	for _, cp := range cfg.Capabilities {
		fmt.Printf("  %-14s %-20s %s\n", cp.ID, cp.Label, cp.Description)
	}
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
	return fmt.Sprintf("#%-5d %-14s %-24s %s", t.Number, t.State, repo, t.Title)
}

func taskBlock(t ui.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s\n", t.Number, t.Title)
	fmt.Fprintf(&b, "state:    %s (github: %s)\n", t.State, t.GitHubState)
	if t.Repo != "" {
		fmt.Fprintf(&b, "repo:     %s\n", t.Repo)
	}
	if t.Base != "" {
		fmt.Fprintf(&b, "base:     %s\n", t.Base)
	}
	if t.AutoMerge != nil {
		fmt.Fprintf(&b, "auto-merge: %t\n", *t.AutoMerge)
	}
	if len(t.Capabilities) > 0 {
		fmt.Fprintf(&b, "capabilities: %s\n", strings.Join(t.Capabilities, ", "))
	}
	fmt.Fprintf(&b, "url:      %s\n", t.HTMLURL)
	if t.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", t.Description)
	}
	return b.String()
}
