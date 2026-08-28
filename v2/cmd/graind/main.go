// Command graind is the grain daemon: it runs pkg/orchestrator's
// RunCycle in the background on a timer, until SIGINT/SIGTERM, against
// one real embedded Dolt store.
//
// bwsalmon/agents#254 asked for exactly this, with one simplification: v2
// has no host adapter yet (v2/README.md), so there is no fleet of real
// sandbox VMs to dispatch onto. graind assumes what that issue grants --
// the MCP server's sandbox tools are confined to a local directory, and
// one slot is the whole concurrency pool -- rather than inventing a fleet
// this deployment shape has nowhere to run. -slots accepts a comma list
// for the day a host adapter exists to give a second slot somewhere real
// to point at; nothing above pkg/orchestrator.Deps needs to change to
// serve more than one.
//
// graind originally drove pkg/orchestrate, a package built independently
// of, and in parallel with, pkg/orchestrator (bwsalmon/agents#249) --
// bwsalmon/agents#263 reconciled the two, keeping pkg/orchestrator (issue
// intake, directive parsing, and PR-health sync were all already wired to
// dispatch.Cycle there) and porting pkg/orchestrate's own capability
// resolution/materialization and reconcile-loop shape onto it. See
// v2/README.md for what that merge kept and dropped.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/agent/gemini"
	"github.com/bwsalmon/grain/v2/pkg/capability/gcpkey"
	"github.com/bwsalmon/grain/v2/pkg/capability/geminikey"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/gitproxy"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
)

func main() {
	dataDir := flag.String("data-dir", "", "root directory for the store, secrets, and sandbox roots (required)")
	slotList := flag.String("slots", "local", "comma-separated slot names -- the concurrency pool dispatch.Cycle fills")
	pollInterval := flag.Duration("poll-interval", 30*time.Second, "how often to run a reconcile cycle")

	taskRepo := flag.String("task-repo", "", "owner/name of the repo whose labelled issues become tasks (required)")
	triggerLabel := flag.String("trigger-label", "grain-agent", "label that marks an issue ready to dispatch")
	defaultTargetRepo := flag.String("default-target-repo", "",
		"owner/name used when a task's issue carries no /repo directive (optional)")

	geminiAPIKeyFile := flag.String("gemini-api-key-file", "", "file holding the Gemini API key the agent runs as (required)")
	geminiModel := flag.String("gemini-model", gemini.DefaultModel, "Gemini model the agent framework calls")
	maxAgentTurns := flag.Int("max-agent-turns", 0, "cap on model/tool round trips per run (0 = the framework's own default)")

	githubHost := flag.String("github-host", "github.com", "GitHub API host -- override to point at a mock for local testing")
	githubInsecureHTTP := flag.Bool("github-insecure-http", false, "speak plain HTTP to -github-host instead of HTTPS (mock servers only)")

	gcpProject := flag.String("gcp-project", "", "GCP project the gcp-key/gemini-key capabilities mint into; empty disables both")
	gcpServiceAccountEmail := flag.String("gcp-agent-service-account", "", "the narrow agent service account gcp-key mints keys for")
	flag.Parse()

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "graind: -data-dir is required")
		os.Exit(2)
	}
	if *taskRepo == "" {
		fmt.Fprintln(os.Stderr, "graind: -task-repo is required")
		os.Exit(2)
	}
	if *geminiAPIKeyFile == "" {
		fmt.Fprintln(os.Stderr, "graind: -gemini-api-key-file is required")
		os.Exit(2)
	}
	slots := strings.Split(*slotList, ",")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, config{
		dataDir: *dataDir, slots: slots, pollInterval: *pollInterval,
		taskRepo: *taskRepo, triggerLabel: *triggerLabel, defaultTargetRepo: *defaultTargetRepo,
		geminiAPIKeyFile: *geminiAPIKeyFile, geminiModel: *geminiModel, maxAgentTurns: *maxAgentTurns,
		githubHost: *githubHost, githubInsecureHTTP: *githubInsecureHTTP,
		gcpProject: *gcpProject, gcpServiceAccountEmail: *gcpServiceAccountEmail,
	}); err != nil {
		log.Fatalf("graind: %v", err)
	}
}

type config struct {
	dataDir      string
	slots        []string
	pollInterval time.Duration

	taskRepo          string
	triggerLabel      string
	defaultTargetRepo string

	geminiAPIKeyFile string
	geminiModel      string
	maxAgentTurns    int

	githubHost         string
	githubInsecureHTTP bool

	gcpProject             string
	gcpServiceAccountEmail string
}

// run wires every piece pkg/orchestrator needs from real, on-disk material
// under cfg.dataDir and starts the reconcile loop; it returns only once
// ctx is cancelled (or setup itself fails).
func run(ctx context.Context, cfg config) error {
	taskRepo, err := model.ParseRepo(cfg.taskRepo)
	if err != nil {
		return fmt.Errorf("parsing -task-repo: %w", err)
	}
	var defaultTarget *model.RepoRef
	if cfg.defaultTargetRepo != "" {
		dt, err := model.ParseRepo(cfg.defaultTargetRepo)
		if err != nil {
			return fmt.Errorf("parsing -default-target-repo: %w", err)
		}
		defaultTarget = &dt
	}

	store, db, err := openStore(cfg.dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	// Mint every slot's sandbox token, and only then start the git proxy
	// -- BuildProxy's own doc comment on gitproxy.LoadSandboxTokens says
	// the proxy "loads the map once at startup and only ever looks
	// tokens up," never re-reading sandbox-tokens.json afterward. Doing
	// this the other way around (as an earlier version of this function
	// did) starts the proxy against whatever tokens already happened to
	// be on disk -- none, on a fresh -data-dir -- so every token minted
	// afterward is one the running proxy can never recognize, and every
	// git push through it fails closed with 401 "authentication
	// required" for the rest of the process's life. cmd/graind/live_test.go's
	// TestRunLiveDispatchesAndOpensAPullRequest caught this live: a real
	// dispatched agent's push was rejected by the proxy every time.
	sandboxes := orchestrator.NewHostSandboxes(filepath.Join(cfg.dataDir, "sandbox"))
	roots := map[string]string{}
	slotTokens := map[string]string{}
	tokens := gitproxy.NewSandboxTokenStore(filepath.Join(cfg.dataDir, "secrets", "sandbox-tokens.json"))
	for _, slot := range cfg.slots {
		root, err := sandboxes.RootFor(slot)
		if err != nil {
			return fmt.Errorf("preparing sandbox for %s: %w", slot, err)
		}
		token, err := tokens.EnsureToken(slot)
		if err != nil {
			return fmt.Errorf("minting sandbox token for %s: %w", slot, err)
		}
		roots[slot] = root
		slotTokens[slot] = token
	}

	proxyURL, stopProxy, err := startGitProxy(cfg.dataDir, store, cfg.githubHost, cfg.githubInsecureHTTP)
	if err != nil {
		return fmt.Errorf("starting git proxy: %w", err)
	}
	defer stopProxy(context.Background())

	for _, slot := range cfg.slots {
		// Configuring git credentials is a one-time, per-slot setup step,
		// not a per-task one -- git-credential-store matches on
		// protocol+host, not path, so this single line covers every repo
		// this slot will ever be pointed at through the proxy. See
		// mcp/git_credentials.go's own doc comment.
		if err := mcp.ConfigureGitCredentials(roots[slot], proxyURL+"/placeholder/placeholder.git", slotTokens[slot]); err != nil {
			return fmt.Errorf("configuring git credentials for %s: %w", slot, err)
		}
	}

	apiKey, err := readTrimmedFile(cfg.geminiAPIKeyFile)
	if err != nil {
		return fmt.Errorf("reading -gemini-api-key-file: %w", err)
	}
	agentFramework, err := gemini.New(ctx, apiKey, gemini.WithModel(cfg.geminiModel))
	if err != nil {
		return fmt.Errorf("building the Gemini agent: %w", err)
	}

	credentials, err := gitproxy.LoadCredentialSet(filepath.Join(cfg.dataDir, "secrets", "github"))
	if err != nil {
		return fmt.Errorf("loading GitHub credential ladder: %w", err)
	}
	githubTransport := github.NewRealTransport(cfg.githubHost)
	githubTransport.UseTLS = !cfg.githubInsecureHTTP
	githubClient := github.NewClient(githubTransport, credentialTokenSource{credentials})

	registry := model.NewCapabilityRegistry(capabilityProviders(cfg)...)

	deps := orchestrator.Deps{
		Store: store, Client: githubClient, Sandboxes: sandboxes,
		Framework: func() agent.Framework { return agentFramework },
		Config: orchestrator.Config{
			TaskRepo: taskRepo, TriggerLabel: cfg.triggerLabel, DefaultTarget: defaultTarget,
			Capabilities:  registry,
			Credentials:   secrets.New(filepath.Join(cfg.dataDir, "secrets")),
			MaxAgentTurns: cfg.maxAgentTurns,
		},
		Slots: cfg.slots,
	}
	log.Printf("graind: reconciling every %s across slots %v for task repo %s", cfg.pollInterval, cfg.slots, taskRepo)
	reconcile(ctx, deps, cfg.pollInterval)
	return nil
}

// reconcile calls orchestrator.RunCycle every interval until ctx is done,
// logging (never panicking on) whatever it returns -- one bad cycle must
// not take the whole daemon down, since the next tick gets another chance
// at whatever failed. Ticks are not overlapped: reconcile waits for one
// RunCycle to return before the next interval starts, so a slow GitHub
// poll simply delays the next dispatch rather than racing it. Ported from
// pkg/orchestrate's own Reconciler.Run (bwsalmon/agents#254) when that
// package merged into pkg/orchestrator, which -- being "a library, not a
// binary" (its own doc comment) -- has no timer loop of its own.
func reconcile(ctx context.Context, deps orchestrator.Deps, interval time.Duration) {
	tick := func() {
		if err := orchestrator.RunCycle(ctx, deps, time.Now().UTC()); err != nil {
			log.Printf("graind: reconcile cycle: %v", err)
		}
	}
	tick()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// capabilityProviders builds every capability provider this deployment
// has enough configuration for. Neither provider is required: a task
// that grants "gcp-key" or "gemini-key" against a registry that never
// registered one is refused, cleanly, by model.ResolveGrants -- not a
// crash at startup.
func capabilityProviders(cfg config) []model.CapabilityProvider {
	if cfg.gcpProject == "" {
		return nil
	}
	var providers []model.CapabilityProvider
	if cfg.gcpServiceAccountEmail != "" {
		providers = append(providers, gcpkey.NewProvider(gcpkey.Config{
			ProjectID: cfg.gcpProject, ServiceAccountEmail: cfg.gcpServiceAccountEmail,
		}))
	}
	providers = append(providers, geminikey.New(cfg.gcpProject, model.CredentialRef{Name: gcpkey.DefaultMinterCredential}))
	return providers
}

// credentialTokenSource adapts gitproxy's own owner/repo credential
// ladder into a github.TokenSource, so the REST client polling issues and
// pull requests and the git proxy pushing to it authenticate off the one
// ladder an operator configures under secrets/github/, rather than a
// second copy of the same decision.
type credentialTokenSource struct{ credentials *gitproxy.CredentialSet }

func (c credentialTokenSource) TokenFor(owner, repo string) *string {
	cred, ok := c.credentials.Select(owner, repo)
	if !ok {
		return nil
	}
	return cred.Token
}

// openStore returns both the Store and the *sql.DB behind it, so run can
// close the connection on the way out -- model.Store itself has no
// Close, deliberately: it imports no driver (pkg/model/dolt's own doc
// comment), so closing is the caller's job.
func openStore(dataDir string) (*model.Store, *sql.DB, error) {
	db, err := dolt.Open(dolt.DefaultConfig(filepath.Join(dataDir, "store")))
	if err != nil {
		return nil, nil, fmt.Errorf("opening embedded dolt: %w", err)
	}
	store := model.New(db)
	if err := store.Init(context.Background()); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("applying schema: %w", err)
	}
	return store, db, nil
}

// startGitProxy serves gitproxy.NewHandler on a local, random port, and
// returns the URL to point every slot's git credential helper at plus a
// shutdown func. Running it in-process rather than as a separate systemd
// unit (v1's shape, docs/design.md) is exactly bwsalmon/agents#254's
// "the MCP server just uses the local machine" simplification applied to
// the proxy too: one process, one machine, no unit to keep in sync with
// this one's own lifecycle.
func startGitProxy(dataDir string, store *model.Store, githubHost string, insecureHTTP bool) (url string, stop func(context.Context) error, err error) {
	proxy, err := gitproxy.BuildProxy(gitproxy.BuildConfig{
		DataDir: dataDir, Store: store, ForwardHost: githubHost, ForwardTLS: !insecureHTTP,
	})
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: gitproxy.NewHandler(proxy)}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("graind: git proxy: %v", err)
		}
	}()
	return "http://" + ln.Addr().String(), srv.Shutdown, nil
}

func readTrimmedFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
