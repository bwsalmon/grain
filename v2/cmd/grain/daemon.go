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

	"github.com/bwsalmon/grain/v2/pkg/agent/gemini"
	"github.com/bwsalmon/grain/v2/pkg/capability/gcpkey"
	"github.com/bwsalmon/grain/v2/pkg/capability/geminikey"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/gitproxy"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
	"github.com/bwsalmon/grain/v2/pkg/orchestrate"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
)

// runDaemon is `grain daemon`: the grain daemon, running pkg/orchestrate's
// reconcile loop in the background on a timer, until SIGINT/SIGTERM,
// against one real embedded Dolt store.
//
// bwsalmon/agents#254 asks for exactly this, with one simplification: v2
// has no host adapter yet (v2/README.md), so there is no fleet of real
// sandbox VMs to dispatch onto. runDaemon assumes what that issue grants
// -- the MCP server's sandbox tools are confined to a local directory,
// and one slot is the whole concurrency pool -- rather than inventing a
// fleet this deployment shape has nowhere to run. -slots accepts a comma
// list for the day a host adapter exists to give a second slot somewhere
// real to point at; nothing above pkg/orchestrate.Config needs to change
// to serve more than one.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("grain daemon", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "root directory for the store, secrets, and sandbox roots (required)")
	slotList := fs.String("slots", "local", "comma-separated slot names -- the concurrency pool loop.Cycle fills")
	pollInterval := fs.Duration("poll-interval", 30*time.Second, "how often to run a reconcile cycle")

	geminiAPIKeyFile := fs.String("gemini-api-key-file", "", "file holding the Gemini API key the agent runs as (required)")
	geminiModel := fs.String("gemini-model", gemini.DefaultModel, "Gemini model the agent framework calls")
	maxAgentTurns := fs.Int("max-agent-turns", 0, "cap on model/tool round trips per run (0 = the framework's own default)")

	githubHost := fs.String("github-host", "github.com", "GitHub API host -- override to point at a mock for local testing")
	githubInsecureHTTP := fs.Bool("github-insecure-http", false, "speak plain HTTP to -github-host instead of HTTPS (mock servers only)")

	gcpProject := fs.String("gcp-project", "", "GCP project the gcp-key/gemini-key capabilities mint into; empty disables both")
	gcpServiceAccountEmail := fs.String("gcp-agent-service-account", "", "the narrow agent service account gcp-key mints keys for")
	fs.Parse(args)

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "grain daemon: -data-dir is required")
		os.Exit(2)
	}
	if *geminiAPIKeyFile == "" {
		fmt.Fprintln(os.Stderr, "grain daemon: -gemini-api-key-file is required")
		os.Exit(2)
	}
	slots := strings.Split(*slotList, ",")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runDaemonLoop(ctx, daemonConfig{
		dataDir: *dataDir, slots: slots, pollInterval: *pollInterval,
		geminiAPIKeyFile: *geminiAPIKeyFile, geminiModel: *geminiModel, maxAgentTurns: *maxAgentTurns,
		githubHost: *githubHost, githubInsecureHTTP: *githubInsecureHTTP,
		gcpProject: *gcpProject, gcpServiceAccountEmail: *gcpServiceAccountEmail,
	}); err != nil {
		log.Fatalf("grain daemon: %v", err)
	}
}

type daemonConfig struct {
	dataDir      string
	slots        []string
	pollInterval time.Duration

	geminiAPIKeyFile string
	geminiModel      string
	maxAgentTurns    int

	githubHost         string
	githubInsecureHTTP bool

	gcpProject             string
	gcpServiceAccountEmail string
}

// runDaemonLoop wires every piece pkg/orchestrate needs from real,
// on-disk material under cfg.dataDir and starts the reconcile loop; it
// returns only once ctx is cancelled (or setup itself fails).
func runDaemonLoop(ctx context.Context, cfg daemonConfig) error {
	store, db, err := openStore(cfg.dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	proxyURL, stopProxy, err := startGitProxy(cfg.dataDir, store, cfg.githubHost, cfg.githubInsecureHTTP)
	if err != nil {
		return fmt.Errorf("starting git proxy: %w", err)
	}
	defer stopProxy(context.Background())

	roots := map[string]string{}
	tokens := gitproxy.NewSandboxTokenStore(filepath.Join(cfg.dataDir, "secrets", "sandbox-tokens.json"))
	for _, slot := range cfg.slots {
		root := filepath.Join(cfg.dataDir, "sandbox", slot)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("creating sandbox root for %s: %w", slot, err)
		}
		token, err := tokens.EnsureToken(slot)
		if err != nil {
			return fmt.Errorf("minting sandbox token for %s: %w", slot, err)
		}
		// Configuring git credentials is a one-time, per-slot setup step,
		// not a per-task one -- git-credential-store matches on
		// protocol+host, not path, so this single line covers every repo
		// this slot will ever be pointed at through the proxy. See
		// mcp/git_credentials.go's own doc comment.
		if err := mcp.ConfigureGitCredentials(root, proxyURL+"/placeholder/placeholder.git", token); err != nil {
			return fmt.Errorf("configuring git credentials for %s: %w", slot, err)
		}
		roots[slot] = root
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

	r := orchestrate.New(orchestrate.Config{
		Store: store, Slots: cfg.slots, Roots: roots,
		Agent:         agentFramework,
		Capabilities:  registry,
		Credentials:   secrets.New(filepath.Join(cfg.dataDir, "secrets")),
		MaxAgentTurns: cfg.maxAgentTurns,
		GitHub:        githubClient,
	})
	log.Printf("grain daemon: reconciling every %s across slots %v", cfg.pollInterval, cfg.slots)
	r.Run(ctx, cfg.pollInterval)
	return nil
}

// capabilityProviders builds every capability provider this deployment
// has enough configuration for. Neither provider is required: a task
// that grants "gcp-key" or "gemini-key" against a registry that never
// registered one is refused, cleanly, by model.ResolveGrants -- not a
// crash at startup.
func capabilityProviders(cfg daemonConfig) []model.CapabilityProvider {
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
// ladder into a github.TokenSource, so the REST client polling PR state
// and the git proxy pushing to it authenticate off the one ladder an
// operator configures under secrets/github/, rather than a second copy
// of the same decision.
type credentialTokenSource struct{ credentials *gitproxy.CredentialSet }

func (c credentialTokenSource) TokenFor(owner, repo string) *string {
	cred, ok := c.credentials.Select(owner, repo)
	if !ok {
		return nil
	}
	return cred.Token
}

// openStore returns both the Store and the *sql.DB behind it, so
// runDaemonLoop can close the connection on the way out --
// model.Store itself has no Close, deliberately: it imports no driver
// (pkg/model/dolt's own doc comment), so closing is the caller's job.
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
			log.Printf("grain daemon: git proxy: %v", err)
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
