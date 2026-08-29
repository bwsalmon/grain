// daemon.go implements `grain daemon`, formerly its own cmd/graind
// binary before bwsalmon/agents#313 combined every mode into one: it
// runs pkg/orchestrator's RunCycle in the background on a timer, until
// SIGINT/SIGTERM, against one real embedded SQLite store, and -- unless
// -ui-addr is emptied out -- also serves pkg/ui's JSON API and static
// frontend directly over that same store (bwsalmon/agents#363). That
// used to be a separate "ui" subcommand (formerly its own cmd/ui binary),
// opening the store a second time -- SQLite's own file locking is what
// makes a daemon and a UI both writing that same file at once safe
// (pkg/model/sqlite's own doc comment). Folding UI serving into the same
// process this dispatch loop already runs in removes the need for a
// second process entirely: there is one process, one store connection,
// one thing to run behind Tailscale or IAP (the issue's own "so the
// server ui/api does not need auth on its own"), and cmd/grain's own
// task CLI is a REST client of the API this serves (main.go's own doc
// comment) rather than a second direct store writer.
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
//
// Most of this file's own flags (-slots, -poll-interval, -gemini-model,
// -max-agent-turns, -github-host, -github-insecure-http, -gcp-project,
// -gcp-agent-service-account, -target-repos) are store-backed now
// (bwsalmon/agents#320):
// loadConfig writes them into model.Store's grain_config row the first
// time a deployment's store has none, and reads them back out of it on
// every start after that, so a UI or a CLI editing model.Config changes
// what the next restart runs with. What stays flags-only either has to
// be known before there is a store to read it from (-data-dir) or names
// secret material rather than being configuration itself
// (-gemini-api-key-file, -kontur-ssh-key) -- bwsalmon/agents#320's own
// "but not the secrets."
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/agent/gemini"
	"github.com/bwsalmon/grain/v2/pkg/capability/gcpkey"
	"github.com/bwsalmon/grain/v2/pkg/capability/geminikey"
	"github.com/bwsalmon/grain/v2/pkg/capability/githubsandbox"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/gitproxy"
	"github.com/bwsalmon/grain/v2/pkg/kontur"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
	"github.com/bwsalmon/grain/v2/pkg/ui"
	"github.com/bwsalmon/grain/v2/pkg/upgrade"
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

func daemon(args []string) {
	// seedOnly marks a flag loadConfig only consults the first time this
	// -data-dir has ever seen a daemon: it seeds grain_config, and every
	// start after that reads the stored value back instead, silently
	// ignoring whatever this flag says -- see loadConfig's own doc
	// comment for why.
	const seedOnly = " (bwsalmon/agents#320: seeds the stored configuration on first use; ignored once one exists -- see loadConfig)"

	fs := flag.NewFlagSet("grain daemon", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "root directory for the store, secrets, and sandbox roots (required)")
	slotList := fs.String("slots", "local", "comma-separated slot names -- the concurrency pool dispatch.Cycle fills"+seedOnly)
	pollInterval := fs.Duration("poll-interval", 30*time.Second, "how often to run a reconcile cycle"+seedOnly)

	uiAddr := fs.String("ui-addr", "127.0.0.1:8420", "address to serve the UI/API on, in-process, over this same store -- empty disables it")
	uiOpen := fs.Bool("ui-open", false, "open the UI in the system's default browser once it's listening")
	actor := fs.String("as", "", "principal the UI/API attributes tasks and comments it creates to (defaults to the OS user)")
	defaultTargetRepo := fs.String("default-target-repo", "", "owner/name a task created through the UI/API with no repo of its own targets")
	targetRepos := fs.String("target-repos", "", "comma-separated owner/name list a task's repo may name -- empty allows any"+seedOnly)

	geminiAPIKeyFile := fs.String("gemini-api-key-file", "", "file holding the Gemini API key the agent runs as (required)")
	geminiModel := fs.String("gemini-model", gemini.DefaultModel, "Gemini model the agent framework calls"+seedOnly)
	maxAgentTurns := fs.Int("max-agent-turns", 0, "cap on model/tool round trips per run (0 = the framework's own default)"+seedOnly)

	githubHost := fs.String("github-host", "github.com", "GitHub API host -- override to point at a mock for local testing"+seedOnly)
	githubInsecureHTTP := fs.Bool("github-insecure-http", false, "speak plain HTTP to -github-host instead of HTTPS (mock servers only)"+seedOnly)

	gcpProject := fs.String("gcp-project", "", "GCP project the gcp-key/gemini-key capabilities mint into; empty disables both"+seedOnly)
	gcpServiceAccountEmail := fs.String("gcp-agent-service-account", "", "the narrow agent service account gcp-key mints keys for"+seedOnly)

	// Upgrading (bwsalmon/agents#396, v2/pkg/upgrade): the UI's own
	// "target a branch and click Upgrade" button. -upgrade-src-dir is the
	// opt in -- empty (the default) disables the whole feature and the
	// pane reports itself unavailable, same as -gcp-project disabling
	// gcp-key/gemini-key above. See v2/scripts/setup.sh for how a
	// deployment built by it wires these three flags up so the feature
	// works out of the box.
	upgradeSrcDir := fs.String("upgrade-src-dir", "",
		"git checkout of bwsalmon/grain to fetch/build from when the UI's Upgrade button is used; empty disables the feature")
	upgradeInstallPath := fs.String("upgrade-install-path", "",
		"where a successful upgrade's binary is installed to (required with -upgrade-src-dir)")
	var upgradeRestartCmd stringSliceFlag
	fs.Var(&upgradeRestartCmd, "upgrade-restart-cmd",
		"one argument of the command run after a successful build+install to bring the new binary up -- repeat for "+
			"every argument, e.g. -upgrade-restart-cmd=sudo -upgrade-restart-cmd=systemctl -upgrade-restart-cmd=restart "+
			"-upgrade-restart-cmd=grain-daemon.service. Omitted entirely, the build and install still happen but "+
			"nothing brings the new binary up on its own. Only used with -upgrade-src-dir.")

	// Sandboxing defaults to orchestrator.HostSandboxes (execute on this
	// host, no isolation) exactly as it always has -- see run()'s own
	// comment on sandboxes below. -kontur-vm-name-prefix is the opt in to
	// orchestrator.KonturSandboxes instead: one real bwsalmon/kontur-
	// managed VM per slot, reached over SSH.
	konturVMNamePrefix := fs.String("kontur-vm-name-prefix", "",
		"if set, dispatch onto real bwsalmon/kontur-managed VMs (one per slot, named <prefix>+<slot>) over SSH, "+
			"instead of local host directories -- see orchestrator.KonturConfig.NamePrefix")
	konturBackend := fs.String("kontur-backend", kontur.BackendDocker,
		"backend `kontur vm create -backend` builds each slot's VM with (only used with -kontur-vm-name-prefix): "+
			"\"docker\" (the default -- bwsalmon/agents#353: no konturctl setup, containerd, CNI or kubelet needed on "+
			"the host, just a local docker daemon) or \"static-pod\" to run under a standalone kubelet instead")
	konturStateDir := fs.String("kontur-state-dir", kontur.DefaultStateDir,
		"kontur's VM state directory (only used with -kontur-vm-name-prefix)")
	criRuntimeEndpoint := fs.String("cri-runtime-endpoint", kontur.DefaultRuntimeEndpoint,
		"containerd CRI socket, used to resolve a kontur VM's pod IP via crictl (only used with -kontur-vm-name-prefix "+
			"and -kontur-backend static-pod; the docker backend has no CRI to ask)")
	konturSSHUser := fs.String("kontur-ssh-user", "",
		"username to SSH into each kontur VM as (required with -kontur-vm-name-prefix)")
	konturSSHKey := fs.String("kontur-ssh-key", "",
		"path to the SSH private key to authenticate to each kontur VM with (required with -kontur-vm-name-prefix)")
	konturWorkspace := fs.String("kontur-workspace", "",
		"working directory run_command/read_file/edit_file/write_file operate in on each kontur VM (required with -kontur-vm-name-prefix)")
	var konturCreateArgs stringSliceFlag
	fs.Var(&konturCreateArgs, "kontur-create-arg",
		"one argument appended verbatim to `kontur vm create <name> -state-dir <dir>` when a slot's VM does not "+
			"exist yet -- repeat for every flag and value bwsalmon/kontur's own `kontur vm create -h` calls for "+
			"beyond a name and -state-dir (guest image, guest SSH port, resource sizing, ...), e.g. "+
			"-kontur-create-arg=-image -kontur-create-arg=gs://bucket/kontur-guest-....qcow2 to point at "+
			"packer/kontur/build.sh's published output using whatever flag bwsalmon/kontur's own CLI turns out "+
			"to call that (see packer/kontur/README.md, \"What isn't settled here\"). Only used with "+
			"-kontur-vm-name-prefix.")
	fs.Parse(args)

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "grain daemon: -data-dir is required")
		os.Exit(2)
	}
	if *geminiAPIKeyFile == "" {
		fmt.Fprintln(os.Stderr, "grain daemon: -gemini-api-key-file is required")
		os.Exit(2)
	}
	if *upgradeSrcDir != "" && *upgradeInstallPath == "" {
		fmt.Fprintln(os.Stderr, "grain daemon: -upgrade-install-path is required with -upgrade-src-dir")
		os.Exit(2)
	}
	if *konturVMNamePrefix != "" {
		if *konturSSHUser == "" {
			fmt.Fprintln(os.Stderr, "grain daemon: -kontur-ssh-user is required with -kontur-vm-name-prefix")
			os.Exit(2)
		}
		if *konturSSHKey == "" {
			fmt.Fprintln(os.Stderr, "grain daemon: -kontur-ssh-key is required with -kontur-vm-name-prefix")
			os.Exit(2)
		}
		if *konturWorkspace == "" {
			fmt.Fprintln(os.Stderr, "grain daemon: -kontur-workspace is required with -kontur-vm-name-prefix")
			os.Exit(2)
		}
	}
	slots := strings.Split(*slotList, ",")
	var targetReposList []string
	if strings.TrimSpace(*targetRepos) != "" {
		targetReposList = strings.Split(*targetRepos, ",")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, config{
		dataDir: *dataDir, slots: slots, pollInterval: *pollInterval,
		uiAddr: *uiAddr, uiOpen: *uiOpen, actor: *actor, defaultTargetRepo: *defaultTargetRepo,
		targetRepos:      targetReposList,
		geminiAPIKeyFile: *geminiAPIKeyFile, geminiModel: *geminiModel, maxAgentTurns: *maxAgentTurns,
		githubHost: *githubHost, githubInsecureHTTP: *githubInsecureHTTP,
		gcpProject: *gcpProject, gcpServiceAccountEmail: *gcpServiceAccountEmail,
		upgradeSrcDir: *upgradeSrcDir, upgradeInstallPath: *upgradeInstallPath, upgradeRestartCmd: upgradeRestartCmd,
		konturVMNamePrefix: *konturVMNamePrefix, konturBackend: *konturBackend,
		konturStateDir: *konturStateDir, criRuntimeEndpoint: *criRuntimeEndpoint,
		konturSSHUser: *konturSSHUser, konturSSHKey: *konturSSHKey, konturWorkspace: *konturWorkspace,
		konturCreateArgs: konturCreateArgs,
	}); err != nil {
		log.Fatalf("grain daemon: %v", err)
	}
}

type config struct {
	dataDir      string
	slots        []string
	pollInterval time.Duration

	// uiAddr, uiOpen, actor and defaultTargetRepo configure the in-process
	// pkg/ui.Server this daemon serves alongside RunCycle (bwsalmon/agents#363);
	// uiAddr empty disables it. actor and defaultTargetRepo build the same
	// ui.Config the old standalone "ui" subcommand's -as/-default-target-repo
	// flags did.
	uiAddr            string
	uiOpen            bool
	actor             string
	defaultTargetRepo string
	// targetRepos is store-backed (model.Config.TargetRepos), unlike
	// defaultTargetRepo above -- see loadConfig/toModelConfig/
	// withModelConfig. Empty allows a task's repo to name anything.
	targetRepos []string

	geminiAPIKeyFile string
	geminiModel      string
	maxAgentTurns    int

	githubHost         string
	githubInsecureHTTP bool

	gcpProject             string
	gcpServiceAccountEmail string

	// upgradeSrcDir, upgradeInstallPath and upgradeRestartCmd configure
	// v2/pkg/upgrade.Upgrader (bwsalmon/agents#396); upgradeSrcDir empty
	// disables it, the same "empty disables" shape gcpProject uses for
	// gcp-key/gemini-key above.
	upgradeSrcDir      string
	upgradeInstallPath string
	upgradeRestartCmd  []string

	// konturVMNamePrefix selects orchestrator.KonturSandboxes over the
	// default orchestrator.HostSandboxes when non-empty; the rest of the
	// kontur* fields are only consulted then. See run()'s own comment on
	// sandboxes.
	konturVMNamePrefix string
	konturBackend      string
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

	store, db, err := openStore(cfg.dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	cfg, err = loadConfig(ctx, store, cfg)
	if err != nil {
		return fmt.Errorf("loading deployment configuration: %w", err)
	}

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
			Backend:         cfg.konturBackend,
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

	if cfg.uiAddr != "" {
		stopUI, err := startUIServer(cfg, store)
		if err != nil {
			return fmt.Errorf("starting the UI/API server: %w", err)
		}
		defer stopUI(context.Background())
	}

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
	log.Printf("grain daemon: reconciling every %s across slots %v", cfg.pollInterval, cfg.slots)
	reconcile(ctx, deps, cfg.pollInterval)
	return nil
}

// reapInterval is how often reconcile calls reapCapabilities -- not
// configurable, since nothing about it needs to race a deployment's own
// -poll-interval: it only has to run comfortably more often than the
// shortest ReapAfter/DeleteExpired cutoff any registered
// model.Reaper carries (24 hours, for both gcpkey.Reap and
// githubsandbox.Provider.Reap) for "clean up after N hours if leaked" to
// actually hold within roughly that bound, not "eventually".
const reapInterval = time.Hour

// reapCapabilities calls Reap on every registered capability provider
// that implements model.Reaper, logging (never failing the sweep on)
// whatever one provider's Reap returns -- the same "one bad cycle must
// not take the whole daemon down" tolerance reconcile's own tick already
// holds RunCycle to. This is the thing that actually makes "clean up
// after N hours if leaked" true rather than merely documented: Reap
// exists on gcpkey.Provider and githubsandbox.Provider today, but until
// something calls it periodically it is dead code, reachable only from a
// test -- this closes that gap for both, not just the capability
// bwsalmon/agents#354 asked for it on. geminikey's own DeleteExpired
// plays the same role but is a package-level function, not a
// model.Reaper, so it is not reached here; wiring it in is a separate,
// smaller follow-up (bwsalmon/agents#354's PR notes this explicitly).
func reapCapabilities(ctx context.Context, registry *model.CapabilityRegistry, creds model.CredentialResolver, now time.Time) {
	if registry == nil {
		return
	}
	for _, p := range registry.Providers() {
		reaper, ok := p.(model.Reaper)
		if !ok {
			continue
		}
		deleted, err := reaper.Reap(ctx, creds, now)
		if err != nil {
			log.Printf("grain daemon: reaping capability %q: %v", p.Spec().Name, err)
			continue
		}
		if len(deleted) > 0 {
			log.Printf("grain daemon: reaped %d stale resource(s) for capability %q: %v", len(deleted), p.Spec().Name, deleted)
		}
	}
}

// reconcile calls orchestrator.RunCycle every interval, and
// reapCapabilities every reapInterval, until ctx is done, logging (never
// panicking on) whatever either returns -- one bad cycle must not take
// the whole daemon down, since the next tick gets another chance at
// whatever failed. RunCycle ticks are not overlapped: reconcile waits for
// one to return before the next interval starts, so a slow GitHub poll
// simply delays the next dispatch rather than racing it; a reap running
// concurrently with a RunCycle tick is fine either way; the two touch
// disjoint state (a reap only ever deletes a resource no live Lease
// still names an outstanding run against). Ported from pkg/orchestrate's
// own Reconciler.Run (bwsalmon/agents#254) when that package merged into
// pkg/orchestrator, which -- being "a library, not a binary" (its own
// doc comment) -- has no timer loop of its own.
func reconcile(ctx context.Context, deps orchestrator.Deps, interval time.Duration) {
	tick := func() {
		if err := orchestrator.RunCycle(ctx, deps, time.Now().UTC()); err != nil {
			log.Printf("grain daemon: reconcile cycle: %v", err)
		}
	}
	reap := func() {
		reapCapabilities(ctx, deps.Config.Capabilities, deps.Config.Credentials, time.Now().UTC())
	}
	tick()
	reap()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	reapTicker := time.NewTicker(reapInterval)
	defer reapTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		case <-reapTicker.C:
			reap()
		}
	}
}

// loadConfig resolves cfg's store-backed fields (bwsalmon/agents#320)
// against store's own grain_config row: on a database with no row yet --
// a fresh -data-dir, or a store no daemon has ever started against --
// flagCfg's own values (the daemon's flags, as parsed) are the seed,
// written once so a UI or a CLI has something to read and edit; on a
// database that already has a row, every field below is read back from
// it instead, discarding whatever the flags said. That is deliberate,
// not a bug: bwsalmon/agents#320 asked for the store to become the
// record the same way it already is for tasks (README, "Input is a
// model update, not a GitHub issue"), and a flag whose value silently
// depended on how many times the daemon had already started would be a
// worse surprise than one that is simply ignored after the first.
//
// Picking up a change made through the store still needs a restart --
// this reads grain_config exactly once, at startup, applying no update
// while RunCycle is running. bwsalmon/agents#320 explicitly did not ask
// for graceful in-flight reloading, so run() does not attempt it.
//
// Only the fields with no bearing on reaching the store in the first
// place move through here: -data-dir and -gemini-api-key-file (a
// secret, not configuration -- bwsalmon/agents#320's own "but not the
// secrets") stay flags-only, read by run() before loadConfig has a store
// to call.
func loadConfig(ctx context.Context, store *model.Store, flagCfg config) (config, error) {
	stored, err := store.GetConfig(ctx)
	if err != nil {
		return config{}, fmt.Errorf("reading stored configuration: %w", err)
	}
	if stored == nil {
		seed := flagCfg.toModelConfig()
		if err := store.PutConfig(ctx, seed); err != nil {
			return config{}, fmt.Errorf("seeding stored configuration from flags: %w", err)
		}
		return flagCfg, nil
	}
	return flagCfg.withModelConfig(*stored), nil
}

// toModelConfig is the flag-parsed subset of config that mirrors
// model.Config -- the seed loadConfig writes when a deployment has never
// stored one.
func (c config) toModelConfig() model.Config {
	return model.Config{
		PollInterval: c.pollInterval, Slots: c.slots,
		GeminiModel: c.geminiModel, MaxAgentTurns: c.maxAgentTurns,
		GitHubHost: c.githubHost, GitHubInsecureHTTP: c.githubInsecureHTTP,
		GCPProject: c.gcpProject, GCPServiceAccountEmail: c.gcpServiceAccountEmail,
		TargetRepos: c.targetRepos,
	}
}

// withModelConfig returns c with every store-backed field replaced by
// mc's -- everything loadConfig reads back out of grain_config once a
// row exists.
func (c config) withModelConfig(mc model.Config) config {
	c.pollInterval = mc.PollInterval
	c.slots = mc.Slots
	c.geminiModel = mc.GeminiModel
	c.maxAgentTurns = mc.MaxAgentTurns
	c.githubHost = mc.GitHubHost
	c.githubInsecureHTTP = mc.GitHubInsecureHTTP
	c.gcpProject = mc.GCPProject
	c.gcpServiceAccountEmail = mc.GCPServiceAccountEmail
	c.targetRepos = mc.TargetRepos
	return c
}

// capabilityProviders builds every capability provider this deployment
// has enough configuration for. No provider is required: a task that
// grants a capability against a registry that never registered one is
// refused, cleanly, by model.ResolveGrants -- not a crash at startup.
//
// gcp-key and gemini-key both need a GCP project to mint into, so both
// stay gated on cfg.gcpProject exactly as before. github-sandbox needs
// no equivalent deployment-level config -- its two secrets are all it
// resolves, and FindInstallation asks GitHub itself which account to
// act on (pkg/capability/githubsandbox's own doc comment) -- so it
// registers unconditionally rather than sharing that gate; an operator
// who never runs `grain controller bootstrap-github-app` simply leaves
// its two secrets unresolvable, and Materialize fails closed the same
// way any other missing secret does.
func capabilityProviders(cfg config) []model.CapabilityProvider {
	var providers []model.CapabilityProvider
	if cfg.gcpProject != "" {
		if cfg.gcpServiceAccountEmail != "" {
			providers = append(providers, gcpkey.NewProvider(gcpkey.Config{
				ProjectID: cfg.gcpProject, ServiceAccountEmail: cfg.gcpServiceAccountEmail,
			}))
		}
		providers = append(providers, geminikey.New(cfg.gcpProject, model.CredentialRef{Name: gcpkey.DefaultMinterCredential}))
	}
	providers = append(providers, githubsandbox.NewProvider(githubsandbox.Config{
		Host: cfg.githubHost, InsecureHTTP: cfg.githubInsecureHTTP,
	}))
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
// Close, deliberately: it imports no driver (pkg/model/sqlite's own doc
// comment), so closing is the caller's job.
//
// -data-dir/store is a plain SQLite database file, and a UI or a CLI
// pointed at the same -data-dir opens that same file directly -- no
// server to run alongside this daemon, and no separate flag to address
// one: SQLite's own file locking is what lets a daemon, a UI and a CLI
// all write to it at once (pkg/model/sqlite's own doc comment).
func openStore(dataDir string) (*model.Store, *sql.DB, error) {
	db, err := sqlite.Open(sqlite.DefaultConfig(filepath.Join(dataDir, "store")))
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
			log.Printf("grain daemon: git proxy: %v", err)
		}
	}()
	return "http://" + ln.Addr().String(), srv.Shutdown, nil
}

// startUIServer serves pkg/ui.Server -- the JSON API plus the static
// frontend -- on addr, over the same store RunCycle dispatches against,
// returning a shutdown func the same shape startGitProxy's own is.
// Running it in-process rather than as a separate "ui" binary/service is
// exactly what bwsalmon/agents#363 asked for: one store connection, no
// second process needed just to let a daemon and a UI coexist (see this
// file's own doc comment).
//
// uiCfg.Secrets always points at this same process's own secrets
// directory (dataDir/secrets, the exact root run() hands orchestrator.Deps'
// own Credentials, above): bwsalmon/agents#357 added write-only secrets
// access for a UI colocated with the server, gated behind a
// -server-data-dir flag on the old standalone "ui" subcommand naming the
// server's own -data-dir from a second process. Now that the UI only
// ever runs inside the daemon that owns the store (the old standalone
// mode is gone -- see this file's own doc comment), it always has that
// directory to hand; there is no longer a cross-process case where it
// would not.
func startUIServer(cfg config, store *model.Store) (stop func(context.Context) error, err error) {
	uiCfg := ui.Config{
		Actor:        ui.DefaultActor(actorID(cfg.actor)),
		Capabilities: ui.DefaultCapabilities(),
		Secrets:      secrets.New(filepath.Join(cfg.dataDir, "secrets")),
		Reboot:       rebootHost,
		TargetRepos:  cfg.targetRepos,
	}
	if cfg.defaultTargetRepo != "" {
		repo, err := model.ParseRepo(cfg.defaultTargetRepo)
		if err != nil {
			return nil, fmt.Errorf("-default-target-repo: %w", err)
		}
		uiCfg.DefaultTarget = &repo
	}
	if cfg.upgradeSrcDir != "" {
		uiCfg.Upgrader = upgrade.New(upgrade.Config{
			SrcDir:      cfg.upgradeSrcDir,
			BuildCmd:    []string{"make", "container-build"},
			BuiltBinary: filepath.Join(cfg.upgradeSrcDir, "v2", "bin", "grain"),
			InstallPath: cfg.upgradeInstallPath,
			RestartCmd:  cfg.upgradeRestartCmd,
			StatusFile:  filepath.Join(cfg.dataDir, "upgrade-status.json"),
		})
	}
	srv := ui.NewServer(uiCfg, store)

	ln, err := net.Listen("tcp", cfg.uiAddr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", cfg.uiAddr, err)
	}
	url := "http://" + ln.Addr().String()
	log.Printf("grain daemon: serving the UI/API on %s as %s", url, uiCfg.Actor.ID)
	if cfg.uiOpen {
		openBrowser(url)
	}
	httpSrv := &http.Server{Handler: srv}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("grain daemon: UI/API server: %v", err)
		}
	}()
	return httpSrv.Shutdown, nil
}

// rebootHost is startUIServer's ui.Config.Reboot: `sudo systemctl
// reboot`, the exact command v1's mcp_server.py already ran for its own
// `reboot_controller` tool (grain/automation/mcp_server.py) and the one
// line scripts/setup.sh's sudoers drop-in grants $GRAIN_USER passwordless
// sudo for. Run as the plain, unprivileged user grain-daemon.service
// already runs as (scripts/setup.sh's own ensure_user), not as root
// itself, the same "only exactly this one command line" restriction v1
// gave its own self-repair sudoers file.
func rebootHost(ctx context.Context) error {
	return exec.CommandContext(ctx, "sudo", "systemctl", "reboot").Run()
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
		log.Printf("grain: opening browser: %v", err)
	}
}

func readTrimmedFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
