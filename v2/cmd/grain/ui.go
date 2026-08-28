package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

// runUI is `grain ui`: grain's standalone task UI, serving pkg/ui's JSON
// API and static frontend on localhost and opening it in the system's
// default browser. See v2/README.md's "The UI" section and pkg/ui's own
// package doc comment for why it is shaped this way -- a local web
// server rather than an Electron/Tauri/native app, so the same binary
// that runs on a developer's Mac or Linux box today is exactly the API
// surface a future iOS/Android client would speak to, with nothing about
// the server itself to rebuild.
func runUI(args []string) {
	fs := flag.NewFlagSet("grain ui", flag.ExitOnError)
	taskRepo := fs.String("task-repo", "", "owner/name of the GitHub repo task issues are filed against (required)")
	addr := fs.String("addr", "127.0.0.1:8420", "address to serve the UI on")
	githubHost := fs.String("github-host", "github.com", "GitHub API host -- override to point at a mock for local testing")
	githubInsecureHTTP := fs.Bool("github-insecure-http", false, "speak plain HTTP to -github-host instead of HTTPS (mock servers only)")
	githubTokenFile := fs.String("github-token-file", "", "file holding a GitHub token to authenticate as (falls back to $GITHUB_TOKEN, then unauthenticated)")
	dryRun := fs.Bool("dry-run", false, "print every GitHub mutation instead of making it -- for trying the UI with no write access")
	open := fs.Bool("open", true, "open the UI in the system's default browser once it's listening")
	fs.Parse(args)

	if *taskRepo == "" {
		fmt.Fprintln(os.Stderr, "grain ui: -task-repo is required")
		os.Exit(2)
	}
	repo, err := model.ParseRepo(*taskRepo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grain ui: -task-repo: %v\n", err)
		os.Exit(2)
	}

	client, err := buildUIGitHubClient(*githubHost, *githubInsecureHTTP, *githubTokenFile, *dryRun)
	if err != nil {
		log.Fatalf("grain ui: %v", err)
	}

	srv := ui.NewServer(ui.Config{
		TaskRepo:     repo,
		Labels:       ui.DefaultLabels(),
		Capabilities: ui.DefaultCapabilities(),
	}, client)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("grain ui: listening on %s: %v", *addr, err)
	}
	url := "http://" + ln.Addr().String()
	log.Printf("grain ui: serving %s on %s", repo, url)
	if *open {
		openBrowser(url)
	}
	log.Fatal(http.Serve(ln, srv))
}

// buildUIGitHubClient resolves the GitHub token ladder
// (-github-token-file, then $GITHUB_TOKEN, then unauthenticated -- fine
// for a public repo, per github.NewClient's own doc comment) and wraps
// it in DryRunClient when -dry-run asks for one, the same split every
// other v2 binary that talks to GitHub offers.
func buildUIGitHubClient(host string, insecureHTTP bool, tokenFile string, dryRun bool) (github.Client, error) {
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

// openBrowser best-effort launches url in the system's default browser --
// failing to do so (headless box, unknown OS) is never fatal, since the
// server is up and the URL is printed regardless.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("grain ui: opening browser: %v", err)
	}
}
