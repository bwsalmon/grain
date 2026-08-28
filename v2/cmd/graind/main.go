// Command graind is the grain daemon: it runs pkg/orchestrator's
// RunCycle in the background on a timer, until SIGINT/SIGTERM, against
// one real embedded Dolt store.
//
// bwsalmon/agents#254 asked for exactly this, with one simplification: by
// default, sandboxing is still local -- the MCP server's sandbox tools
// confined to a directory on this host, no isolation -- rather than a
// fleet this deployment shape has nowhere to run. -kontur-vm-name-prefix
// is the opt in to the real alternative (bwsalmon/agents#274):
// orchestrator.KonturSandboxes, one real bwsalmon/kontur-managed VM per
// slot, reached over SSH. -slots accepts a comma list either way; nothing
// above pkg/orchestrator.Deps needs to change to serve more than one.
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
	"github.com/bwsalmon/grain/v2/pkg/kontur"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
)

// stringSliceFlag collects every occurrence of a repeatable flag, in
// order, into a []string -- flag.String only ever keeps the last one,
// which -kontur-create-arg (an ordered sequence of flag/value pairs
// passed straight through to `kontur vm create`) can't use.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, " ") }

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	dataDir := flag.String("data-dir", "", "root directory for the store, secrets, and sandbox roots (required)")
	slotList := flag.String("slots", "local", "comma-separated slot names -- the concurrency pool dispatch.Cycle fills")
	pollInterval := flag.Duration("poll-interval", 30*time.Second, "how often to run a reconcile cycle")

	storeAddr := flag.String("store-addr", "", "host:port of a Dolt SQL server holding the task store -- required to share the store with a UI or a CLI")
	storeDatabase := flag.String("store-database", "grain", "database name on -store-addr")
	storeUser := flag.String("store-user", "root", "user to connect to -store-addr as")
	storePasswordFile := flag.String("store-password-file", "", "file holding the password for -store-user")

	geminiAPIKeyFile := flag.String("gemini-api-key-file", "", "file holding the Gemini API key the agent runs as (required)")
	geminiModel := flag.String("gemini-model", gemini.DefaultModel, "Gemini model the agent framework calls")
	maxAgentTurns := flag.Int("max-agent-turns", 0, "cap on model/tool round trips per run (0 = the framework's own default)")

	githubHost := flag.String("github-host", "github.com", "GitHub API host -- override to point at a mock for local testing")
	githubInsecureHTTP := flag.Bool("github-insecure-http", false, "speak plain HTTP to -github-host instead of HTTPS (mock servers only)")

	gcpProject := flag.String("gcp-project", "", "GCP project the gcp-key/gemini-key capabilities mint into; empty disables both")
	gcpServiceAccountEmail := flag.String("gcp-agent-service-account", "", "the narrow agent service account gcp-key mints keys for")

	// Sandboxing defaults to orchestrator.HostSandboxes (execute on this
	// host, no isolation) exactly as it always has -- see run()'s own
	// comment on sandboxes below. -kontur-vm-name-prefix is the opt in to
	// orchestrator.KonturSandboxes instead: one real bwsalmon/kontur-
	// managed VM per dispatch slot, reached over SSH.
	konturVMNamePrefix := flag.String("kontur-vm-name-prefix", "",
		"if set, dispatch onto real bwsalmon/kontur-managed VMs (one per slot, named <prefix>+<slot>) over SSH, "+
			"instead of local host directories -- see orchestrator.KonturConfig.NamePrefix")
	konturStateDir := flag.String("kontur-state-dir", kontur.DefaultStateDir,
		"kontur's VM state directory (only used with -kontur-vm-name-prefix)")
	criRuntimeEndpoint := flag.String("cri-runtime-endpoint", kontur.DefaultRuntimeEndpoint,
		"containerd CRI socket, used to resolve a kontur VM's pod IP via crictl (only used with -kontur-vm-name-prefix)")
	konturSSHUser := flag.String("kontur-ssh-user", "",
		"username to SSH into each kontur VM as (required with -kontur-vm-name-prefix)")
	konturSSHKey := flag.String("kontur-ssh-key", "",
		"path to the SSH private key to authenticate to each kontur VM with (required with -kontur-vm-name-prefix)")
	konturWorkspace := flag.String("kontur-workspace", "",
		"working directory run_command/read_file/edit_file/write_file operate in on each kontur VM (required with -kontur-vm-name-prefix)")
	var konturCreateArgs stringSliceFlag
	flag.Var(&konturCreateArgs, "kontur-create-arg",
		"one argument appended verbatim to `kontur vm create <name> -state-dir <dir>` when a slot's VM does not "+
			"exist yet -- repeat for every flag and value bwsalmon/kontur's own `kontur vm create -h` calls for "+
			"beyond a name and -state-dir (guest image, guest SSH port, resource sizing, ...), e.g. "+
			"-kontur-create-arg=-image -kontur-create-arg=gs://bucket/kontur-guest-....qcow2 to point at "+
			"packer/kontur/build.sh's published output using whatever flag bwsalmon/kontur's own CLI turns out "+
			"to call that (see packer/kontur/README.md, \"What isn't settled here\"). Only used with "+
			"-kontur-vm-name-prefix.")
	flag.Parse()

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "graind: -data-dir is required")
		os.Exit(2)
	}
	if *geminiAPIKeyFile == "" {
		fmt.Fprintln(os.Stderr, "graind: -gemini-api-key-file is required")
		os.Exit(2)
	}
	if *konturVMNamePrefix != "" {
		if *konturSSHUser == "" {
			fmt.Fprintln(os.Stderr, "graind: -kontur-ssh-user is required with -kontur-vm-name-prefix")
			os.Exit(2)
		}
		if *konturSSHKey == "" {
			fmt.Fprintln(os.Stderr, "graind: -kontur-ssh-key is required with -kontur-vm-name-prefix")
			os.Exit(2)
		}
		if *konturWorkspace == "" {
			fmt.Fprintln(os.Stderr, "graind: -kontur-workspace is required with -kontur-vm-name-prefix")
			os.Exit(2)
		}
	}
	slots := strings.Split(*slotList, ",")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, config{
		dataDir: *dataDir, slots: slots, pollInterval: *pollInterval,
		storeAddr: *storeAddr, storeDatabase: *storeDatabase,
		storeUser: *storeUser, storePasswordFile: *storePasswordFile,
		geminiAPIKeyFile: *geminiAPIKeyFile, geminiModel: *geminiModel, maxAgentTurns: *maxAgentTurns,
		githubHost: *githubHost, githubInsecureHTTP: *githubInsecureHTTP,
		gcpProject: *gcpProject, gcpServiceAccountEmail: *gcpServiceAccountEmail,
		konturVMNamePrefix: *konturVMNamePrefix, konturStateDir: *konturStateDir, criRuntimeEndpoint: *criRuntimeEndpoint,
		konturSSHUser: *konturSSHUser, konturSSHKey: *konturSSHKey, konturWorkspace: *konturWorkspace,
		konturCreateArgs: konturCreateArgs,
	}); err != nil {
		log.Fatalf("graind: %v", err)
	}
}

type config struct {
	dataDir      string
	slots        []string
	pollInterval time.Duration

	storeAddr         string
	storeDatabase     string
	storeUser         string
	storePasswordFile string

	geminiAPIKeyFile string
	geminiModel      string
	maxAgentTurns    int

	githubHost         string
	githubInsecureHTTP bool

	gcpProject             string
	gcpServiceAccountEmail string

	// konturVMNamePrefix selects orchestrator.KonturSandboxes over the
	// default orchestrator.HostSandboxes when non-empty; the rest of the
	// kontur* fields are only consulted then. See run()'s own comment on
	// sandboxes.
	konturVMNamePrefix string
	konturStateDir     string
	criRuntimeEndpoint string
	konturSSHUser      string
	konturSSHKey       string
	konturWorkspace    string
	konturCreateArgs   []string
}

// run wires every piece pkg/orchestrator needs from real, on-disk material
// under cfg.dataDir and starts the reconcile loop; it returns only once
// ctx is cancelled (or setup itself fails).
func run(ctx context.Context, cfg config) error {

	server := dolt.ServerConfig{Addr: cfg.storeAddr, Database: cfg.storeDatabase, User: cfg.storeUser}
	if cfg.storePasswordFile != "" {
		password, err := readTrimmedFile(cfg.storePasswordFile)
		if err != nil {
			return fmt.Errorf("reading -store-password-file: %w", err)
		}
		server.Password = password
	}
	store, db, err := openStore(cfg.dataDir, server)
	if err != nil {
		return err
	}
	defer db.Close()

	// Sandboxing defaults to orchestrator.HostSandboxes -- one local
	// directory per slot, no isolation -- exactly as it always has;
	// -kontur-vm-name-prefix opts into orchestrator.KonturSandboxes
	// instead: one real bwsalmon/kontur-managed VM per slot, reached over
	// SSH (pkg/orchestrator's own doc comment: "Sandboxing defaults to
	// 'execute on the host,' deliberately, for now, with a real host
	// adapter available as an opt in"). Exactly one of hostSandboxes/
	// konturSandboxes is non-nil below; sandboxes is Deps.Sandboxes
	// either way.
	var sandboxes orchestrator.Sandboxes
	var hostSandboxes *orchestrator.HostSandboxes
	var konturSandboxes *orchestrator.KonturSandboxes
	if cfg.konturVMNamePrefix != "" {
		konturSandboxes = orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
			NamePrefix:      cfg.konturVMNamePrefix,
			StateDir:        cfg.konturStateDir,
			RuntimeEndpoint: cfg.criRuntimeEndpoint,
			CreateArgs:      cfg.konturCreateArgs,
			SSHUser:         cfg.konturSSHUser,
			SSHKey:          cfg.konturSSHKey,
			Workspace:       cfg.konturWorkspace,
		})
		sandboxes = konturSandboxes
	} else {
		hostSandboxes = orchestrator.NewHostSandboxes(filepath.Join(cfg.dataDir, "sandbox"))
		sandboxes = hostSandboxes
	}

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
	roots := map[string]string{}
	slotTokens := map[string]string{}
	tokens := gitproxy.NewSandboxTokenStore(filepath.Join(cfg.dataDir, "secrets", "sandbox-tokens.json"))
	for _, slot := range cfg.slots {
		if hostSandboxes != nil {
			root, err := hostSandboxes.RootFor(slot)
			if err != nil {
				return fmt.Errorf("preparing sandbox for %s: %w", slot, err)
			}
			roots[slot] = root
		}
		token, err := tokens.EnsureToken(slot)
		if err != nil {
			return fmt.Errorf("minting sandbox token for %s: %w", slot, err)
		}
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
		// mcp/git_credentials.go's own doc comment. For a kontur-backed
		// slot this also creates that slot's VM, the same way ToolsFor's
		// first call would have -- doing it here instead means a slot's
		// VM is up, and reachable, before RunCycle ever tries to dispatch
		// onto it.
		remoteURL := proxyURL + "/placeholder/placeholder.git"
		if hostSandboxes != nil {
			if err := mcp.ConfigureGitCredentials(roots[slot], remoteURL, slotTokens[slot]); err != nil {
				return fmt.Errorf("configuring git credentials for %s: %w", slot, err)
			}
			continue
		}
		if err := konturSandboxes.ConfigureGitCredentials(ctx, slot, remoteURL, slotTokens[slot]); err != nil {
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
			Capabilities:  registry,
			Credentials:   secrets.New(filepath.Join(cfg.dataDir, "secrets")),
			MaxAgentTurns: cfg.maxAgentTurns,
		},
		Slots: cfg.slots,
	}
	log.Printf("graind: reconciling every %s across slots %v", cfg.pollInterval, cfg.slots)
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
//
// -store-addr points at a Dolt SQL server, which is what a deployment
// running a UI or a CLI alongside this daemon needs: embedded Dolt is a
// single writer, and those are writers now (README, "Single writer").
// Without it, the embedded database under -data-dir is used, and nothing
// else may be running against it.
func openStore(dataDir string, server dolt.ServerConfig) (*model.Store, *sql.DB, error) {
	db, err := dolt.OpenOrConnect(filepath.Join(dataDir, "store"), server)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the task store: %w", err)
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
