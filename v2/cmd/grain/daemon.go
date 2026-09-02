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
// fleet this deployment shape has nowhere to run. -kontur-sandboxes
// is the opt in to the real alternative (bwsalmon/agents#274):
// orchestrator.KonturSandboxes, one real bwsalmon/kontur-managed VM per
// run, reached over SSH. -max-concurrent caps how many runs are in
// flight at once either way, and a sandbox is built for each of them and
// destroyed with it; nothing
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
// Most of this file's own flags (-max-concurrent, -poll-interval, -agent-framework,
// -gemini-model, -claude-model, -max-agent-turns, -github-host, -github-insecure-http, -gcp-project,
// -gcp-agent-service-account, -target-repos) are store-backed now
// (bwsalmon/agents#320):
// loadConfig writes them into model.Store's grain_config row the first
// time a deployment's store has none, and reads them back out of it on
// every start after that, so a UI or a CLI editing model.Config changes
// what the next restart runs with. What stays flags-only either has to
// be known before there is a store to read it from (-data-dir) or names
// secret material rather than being configuration itself
// (-gemini-api-key-file, -kontur-ssh-key, -claude-oauth-token-file) --
// bwsalmon/agents#320's own "but not the secrets." -claude-path joins
// them not because it is secret but because, like -kontur-ssh-key, it
// names something about *this host's* filesystem rather than the
// deployment's own behaviour.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/agent/antigravity"
	"github.com/bwsalmon/grain/v2/pkg/agent/claude"
	"github.com/bwsalmon/grain/v2/pkg/capability/bootstrap"
	"github.com/bwsalmon/grain/v2/pkg/capability/gcpkey"
	"github.com/bwsalmon/grain/v2/pkg/capability/geminikey"
	"github.com/bwsalmon/grain/v2/pkg/capability/githubsandbox"
	"github.com/bwsalmon/grain/v2/pkg/capability/selfdebug"
	"github.com/bwsalmon/grain/v2/pkg/capability/selfrepair"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/gitproxy"
	"github.com/bwsalmon/grain/v2/pkg/kontur"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
	"github.com/bwsalmon/grain/v2/pkg/sysstat"
	"github.com/bwsalmon/grain/v2/pkg/systemlog"
	"github.com/bwsalmon/grain/v2/pkg/ui"
	"github.com/bwsalmon/grain/v2/pkg/upgrade"
)

// stringSliceFlag collects every occurrence of a repeatable flag, in
// order, into a []string -- flag.String only ever keeps the last one,
// which -kontur-create-arg (an ordered sequence of flag/value pairs
// passed straight through to `konturctl vm create`) can't use.
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
	dataDir := fs.String("data-dir", "", "root directory for the store and secrets -- the state a redeploy must not lose (required)")
	sandboxDir := fs.String("sandbox-dir", "", "root directory orchestrator.HostSandboxes creates one working directory per run under, for local "+
		"(non-kontur) sandboxing -- required unless -kontur-sandboxes selects orchestrator.KonturSandboxes instead. Deliberately a separate "+
		"flag from -data-dir rather than a subdirectory of it (bwsalmon/agents#587): a task's checked-out repo and whatever it wrote into its "+
		"sandbox are disposable, unlike the store and secrets under -data-dir, so this belongs on storage that a VM wipe or redeploy is free to "+
		"discard along with the rest of the host")
	maxConcurrent := fs.Int("max-concurrent", 1, "maximum number of tasks dispatched at once -- the size of the concurrency pool dispatch.Cycle fills"+seedOnly)
	pollInterval := fs.Duration("poll-interval", 30*time.Second, "how often to run a reconcile cycle"+seedOnly)

	uiAddr := fs.String("ui-addr", "127.0.0.1:8420", "address to serve the UI/API on, in-process, over this same store -- empty disables it")
	uiOpen := fs.Bool("ui-open", false, "open the UI in the system's default browser once it's listening")
	actor := fs.String("as", "", "principal the UI/API attributes tasks and comments it creates to (defaults to the OS user)")
	defaultTargetRepo := fs.String("default-target-repo", "", "owner/name a task created through the UI/API with no repo of its own targets")
	targetRepos := fs.String("target-repos", "", "comma-separated owner/name list a task's repo may name -- empty allows any"+seedOnly)

	agentFramework := fs.String("agent-framework", model.AgentFrameworkAntigravity,
		"which agent.Framework a run is driven by by default: \""+model.AgentFrameworkAntigravity+
			"\" (agent/antigravity, the Antigravity CLI's agy binary as a subprocess -- see -agy-path) or \""+
			model.AgentFrameworkClaude+"\" (agent/claude, the real claude CLI as a subprocess -- see "+
			"-claude-path/-claude-oauth-token-file). \""+model.LegacyAgentFrameworkGemini+"\" is accepted as the "+
			"former spelling of "+model.AgentFrameworkAntigravity+". Seeds the store-backed setting the UI edits, "+
			"and a task can override it for its own dispatch"+seedOnly)
	geminiAPIKeyFile := fs.String("gemini-api-key-file", "", "file holding the Gemini API key the agent runs as. "+
		"Optional now that the key can be set from the UI instead (Settings -> Agent frameworks, stored as the "+
		"\""+secrets.GeminiAPIKeySecret+"\" secret): a key set there wins, and this file is what a deployment "+
		"seeded one with before that existed. With neither, a run driven by the gemini framework fails as "+
		"setup-failed saying so, rather than the daemon refusing to start")
	geminiModel := fs.String("gemini-model", antigravity.DefaultModel, "model the antigravity agent framework calls"+seedOnly)
	claudeModel := fs.String("claude-model", claude.DefaultModel, "model the claude agent framework calls"+seedOnly)
	maxAgentTurns := fs.Int("max-agent-turns", 0, "cap on model/tool round trips per run (0 = the framework's own default)"+seedOnly)

	// claudePath and claudeOAuthTokenFile are only consulted when a run
	// is actually driven by agent/claude -- the store's agent-framework
	// setting (bwsalmon/agents#609, `grain settings`), or a task's own
	// override of it (model.Task.AgentFramework). Neither is required at
	// flag-parse time, and neither is required at all any more: both
	// frameworks' credentials are settable from the UI, into the secrets
	// database (secrets.ClaudeOAuthTokenSecret), which is where
	// agentCredential looks before it looks at either file. They stay
	// flags-only rather than moving into model.Config for the same
	// reason -gemini-api-key-file does: secret material, not
	// configuration itself.
	agyPath := fs.String("agy-path", "", "path to the Antigravity CLI binary (agy) agent/antigravity runs as a "+
		"subprocess; empty resolves \"agy\" against $PATH instead. Only used by a run driven by the "+
		"\""+model.AgentFrameworkAntigravity+"\" framework")
	claudePath := fs.String("claude-path", "", "path to the claude CLI binary agent/claude runs as a subprocess; "+
		"empty resolves \"claude\" against $PATH instead. Only used when the agent-framework setting is \"claude\"")
	claudeOAuthTokenFile := fs.String("claude-oauth-token-file", "", "file holding the Claude Code OAuth token the "+
		"agent authenticates as, passed to the claude subprocess as CLAUDE_CODE_OAUTH_TOKEN. Optional, and the "+
		"exact counterpart of -gemini-api-key-file above: the UI stores one as the "+
		"\""+secrets.ClaudeOAuthTokenSecret+"\" secret and that wins over this file")

	githubHost := fs.String("github-host", "github.com", "GitHub git host -- what the proxy forwards to and, via github.APIHost, where REST calls go; override to point at a mock for local testing"+seedOnly)
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
		"one argument of the command run at the end of a successful upgrade to bring the new build up -- "+
			"repeat for every argument, e.g. -upgrade-restart-cmd=sudo -upgrade-restart-cmd=systemctl "+
			"-upgrade-restart-cmd=restart -upgrade-restart-cmd=grain-daemon.service. Omitted entirely, the "+
			"upgrade still happens but nothing brings the new build up on its own. Used by both upgrade "+
			"paths below.")

	// The container deployment's own upgrade path (bwsalmon/agents#645,
	// v2/pkg/upgrade/image.go). Set, it replaces the build-a-binary path
	// above entirely: there is no toolchain on a host that runs grain
	// from an image, so "upgrade to branch X" means pulling the tag CI
	// published for X and pointing the unit's own image ref file at it.
	// -upgrade-src-dir may still be set alongside it -- image mode
	// ignores it for upgrading, but grantTools reads the checkout it
	// names for the self-debug capability -- and no -upgrade-install-path
	// is needed then, since nothing installs a binary.
	upgradeImage := fs.String("upgrade-image", "",
		"image repository, with no tag, that CI publishes a tag per branch to (e.g. "+
			"ghcr.io/bwsalmon/grain/grain); set, the UI's Upgrade button pulls a branch's image instead of "+
			"building one, and -upgrade-image-ref-file is required")
	upgradeImageRefFile := fs.String("upgrade-image-ref-file", "",
		"file a successful -upgrade-image upgrade rewrites with the image the service should run, as one "+
			"GRAIN_IMAGE=<ref> line for grain-daemon.service to read as an EnvironmentFile "+
			"(required with -upgrade-image)")

	// How the UI's "reboot host" button reboots (pkg/ui/host.go). The
	// default is the `sudo systemctl reboot` this always ran, which is
	// right for a daemon running directly on the host under systemd and
	// impossible for one running in a container, where there is no host
	// systemd to talk to -- v2/scripts/setup.sh points this at a file
	// touch instead, watched by a host-side path unit. See rebootHost.
	var rebootCmd stringSliceFlag
	fs.Var(&rebootCmd, "reboot-cmd",
		"one argument of the command the UI's reboot-host button runs -- repeat for every argument. "+
			"Defaults to `sudo systemctl reboot`.")

	// Sandboxing defaults to orchestrator.HostSandboxes (execute on this
	// host, no isolation) exactly as it always has -- see run()'s own
	// comment on sandboxes below. -kontur-sandboxes is the opt in to
	// orchestrator.KonturSandboxes instead: one real bwsalmon/kontur-
	// managed VM per run, reached over SSH.
	konturSandboxes := fs.Bool("kontur-sandboxes", false,
		"dispatch onto real bwsalmon/kontur-managed VMs (one per run, named orchestrator.VMNamePrefix+<run id>) "+
			"over SSH, instead of local host directories. This was -kontur-vm-name-prefix, whose value both opted "+
			"in and named the VMs; the name is a constant now, since a VM name has 11 bytes to live in and a run "+
			"id needs nine of them, leaving nothing an operator could usefully choose -- see "+
			"orchestrator.VMNamePrefix")
	konturStateDir := fs.String("kontur-state-dir", kontur.DefaultStateDir,
		"kontur's VM state directory (only used with -kontur-sandboxes)")
	konturSSHUser := fs.String("kontur-ssh-user", "",
		"username to SSH into each kontur VM as (required with -kontur-sandboxes)")
	konturExecKey := fs.String("kontur-exec-key", "",
		"path, *inside the VM's container*, of the private key `kontur exec` authenticates to the guest with "+
			"(required with -kontur-sandboxes) -- e.g. /images/kontur-exec-key for a key placed in the "+
			"directory -kontur-create-arg's own -images-hostpath already mounts read-only at /images. Left "+
			"unset, `kontur exec` falls back to the key bwsalmon/kontur bakes into its own image, which only a "+
			"guest image built by that same Dockerfile authorizes.")
	konturWorkspace := fs.String("kontur-workspace", "",
		"working directory run_command/read_file/edit_file/write_file operate in on each kontur VM (required with -kontur-sandboxes)")
	var konturCreateArgs stringSliceFlag
	fs.Var(&konturCreateArgs, "kontur-create-arg",
		"one argument appended verbatim to the `konturctl vm create <name> -state-dir <dir>` that builds a run's "+
			"VM -- repeat for every flag and value bwsalmon/kontur's own `konturctl vm create -h` calls for "+
			"beyond a name and -state-dir (guest image, guest SSH port, resource sizing, ...), e.g. "+
			"-kontur-create-arg=-images-hostpath -kontur-create-arg=/var/lib/vm-images -kontur-create-arg=-disk "+
			"-kontur-create-arg=/images/current/disk.img -kontur-create-arg=-kernel "+
			"-kontur-create-arg=/images/current/vmlinuz -kontur-create-arg=-initramfs "+
			"-kontur-create-arg=/images/current/initrd.img -kontur-create-arg=-guest-port "+
			"-kontur-create-arg=22 to point at packer/kontur/build-guest.sh's published output, already copied onto "+
			"this host under -images-hostpath's directory (see packer/kontur/README.md, \"Building and "+
			"publishing\", and v2/scripts/setup.sh's own ensure_kontur_images, which is what actually copies "+
			"it there for terraform/gcp-v2 -- -guest-port 22 is not optional: konturctl's own default is 80, "+
			"which silently refuses every connection to this image's actual sshd). Only used with "+
			"-kontur-sandboxes. Under -kontur-net nat, prefer -kontur-base-ip/-kontur-base-port over "+
			"putting -ip/-port here: they are appended last, so they win over this list.")
	konturNet := fs.String("kontur-net", kontur.NetModeFlat,
		"how a kontur VM reaches the network: \"flat\" (the default -- the guest is spliced onto the sandbox "+
			"container's own segment and takes over the address docker assigned it, so -kontur-base-ip/"+
			"-kontur-base-port are unnecessary and ignored) or \"nat\" (kontur's original mode: a private "+
			"subnet per namespace, with an -ip and a forwarded -port assigned per VM). Flat mode needs a guest "+
			"image built from kontur's own guest overlays, for the control link \"kontur exec\" arrives on -- "+
			"packer/kontur/build-guest.sh produces one.")
	konturBaseIP := fs.String("kontur-base-ip", "",
		"the -ip every kontur VM is created with under -kontur-net nat, passed verbatim. Each VM has its own "+
			"network namespace under the docker backend (its own netns-holder container), so they do not "+
			"collide; this used to be a base that each slot's number was added to, back when a slot's VM was "+
			"long-lived, which was guarding against "+
			"a shared bridge the docker backend does not have. Ignored under flat mode, and only used with "+
			"-kontur-sandboxes.")
	konturBasePort := fs.Int("kontur-base-port", 0,
		"the -port every kontur VM is created with under -kontur-net nat, passed verbatim -- a DNAT target "+
			"inside that VM's own namespace, not a port published on this host. Ignored under flat mode, and "+
			"only used with -kontur-sandboxes.")
	konturGitProxyHost := fs.String("kontur-git-proxy-host", "",
		"host (no port) this daemon's git proxy is reachable at from inside a kontur VM's own guest, in place "+
			"of the loopback address the proxy binds to by default -- required with -kontur-sandboxes. A "+
			"kontur VM (KonturConfig.createArgs always builds one against -backend docker) runs its guest in its "+
			"own network namespace behind netshim's NAT (third_party/kontur/internal/netshim), with its own "+
			"127.0.0.1 that never reaches this process's -- so a clone against the default 127.0.0.1 proxy URL "+
			"fails the moment it leaves the guest (bwsalmon/agents#567: \"Failed to connect to 127.0.0.1 ... "+
			"Couldn't connect to server\"). Setting this makes startGitProxy bind on every interface instead of "+
			"just loopback, and advertise this host to every run's sandbox in loopback's place -- typically the "+
			"docker bridge gateway address the guest's own outbound NAT routes through to reach this host (see "+
			"netshim's masqueradeExprs); run `docker network inspect bridge` (or whichever network the kontur VM "+
			"containers join) and read \"Gateway\" if unsure -- commonly 172.17.0.1.")
	sandboxCPUs := fs.Int("sandbox-cpus", 0,
		"deployment-wide default vCPU count for a kontur-managed sandbox VM, passed as `konturctl vm create`'s "+
			"own -cpus (only used with -kontur-sandboxes); 0 leaves bwsalmon/kontur's own default in place. "+
			"Overridable per task from the UI/API (model.Task.SandboxCPUs)"+seedOnly)
	sandboxMemoryMB := fs.Int("sandbox-memory-mb", 0,
		"deployment-wide default guest memory, in MiB, for a kontur-managed sandbox VM, passed as `konturctl vm "+
			"create`'s own -memory-mb (only used with -kontur-sandboxes); 0 leaves bwsalmon/kontur's own "+
			"default in place. Overridable per task from the UI/API (model.Task.SandboxMemoryMB)"+seedOnly)
	fs.Parse(args)

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "grain daemon: -data-dir is required")
		os.Exit(2)
	}
	if *sandboxDir == "" && !*konturSandboxes {
		fmt.Fprintln(os.Stderr, "grain daemon: -sandbox-dir is required unless -kontur-sandboxes is set")
		os.Exit(2)
	}
	if *geminiAPIKeyFile == "" {
		fmt.Fprintln(os.Stderr, "grain daemon: -gemini-api-key-file is required")
		os.Exit(2)
	}
	if *upgradeSrcDir != "" && *upgradeInstallPath == "" && *upgradeImage == "" {
		fmt.Fprintln(os.Stderr, "grain daemon: -upgrade-install-path is required with -upgrade-src-dir")
		os.Exit(2)
	}
	if *upgradeImage != "" && *upgradeImageRefFile == "" {
		fmt.Fprintln(os.Stderr, "grain daemon: -upgrade-image-ref-file is required with -upgrade-image")
		os.Exit(2)
	}
	if *konturSandboxes {
		if *konturSSHUser == "" {
			fmt.Fprintln(os.Stderr, "grain daemon: -kontur-ssh-user is required with -kontur-sandboxes")
			os.Exit(2)
		}
		if *konturExecKey == "" {
			fmt.Fprintln(os.Stderr, "grain daemon: -kontur-exec-key is required with -kontur-sandboxes")
			os.Exit(2)
		}
		if *konturWorkspace == "" {
			fmt.Fprintln(os.Stderr, "grain daemon: -kontur-workspace is required with -kontur-sandboxes")
			os.Exit(2)
		}
		if *konturGitProxyHost == "" {
			fmt.Fprintln(os.Stderr, "grain daemon: -kontur-git-proxy-host is required with -kontur-sandboxes")
			os.Exit(2)
		}
	}
	if *maxConcurrent < 1 {
		fmt.Fprintln(os.Stderr, "grain daemon: -max-concurrent must be at least 1")
		os.Exit(2)
	}
	// NormalizeAgentFramework first, so the legacy "gemini" spelling a
	// deployment's own unit file may still pass keeps working across the
	// upgrade that replaced that framework with agent/antigravity.
	*agentFramework = model.NormalizeAgentFramework(*agentFramework)
	if *agentFramework != model.AgentFrameworkAntigravity && *agentFramework != model.AgentFrameworkClaude {
		fmt.Fprintf(os.Stderr, "grain daemon: -agent-framework must be %q or %q\n",
			model.AgentFrameworkAntigravity, model.AgentFrameworkClaude)
		os.Exit(2)
	}
	var targetReposList []string
	if strings.TrimSpace(*targetRepos) != "" {
		targetReposList = strings.Split(*targetRepos, ",")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, config{
		dataDir: *dataDir, sandboxDir: *sandboxDir, maxConcurrent: *maxConcurrent, pollInterval: *pollInterval,
		uiAddr: *uiAddr, uiOpen: *uiOpen, actor: *actor, defaultTargetRepo: *defaultTargetRepo,
		targetRepos:      targetReposList,
		agentFramework:   *agentFramework,
		geminiAPIKeyFile: *geminiAPIKeyFile, geminiModel: *geminiModel, maxAgentTurns: *maxAgentTurns,
		agyPath:    *agyPath,
		claudePath: *claudePath, claudeOAuthTokenFile: *claudeOAuthTokenFile, claudeModel: *claudeModel,
		githubHost: *githubHost, githubInsecureHTTP: *githubInsecureHTTP,
		gcpProject: *gcpProject, gcpServiceAccountEmail: *gcpServiceAccountEmail,
		upgradeSrcDir: *upgradeSrcDir, upgradeInstallPath: *upgradeInstallPath, upgradeRestartCmd: upgradeRestartCmd,
		upgradeImage: *upgradeImage, upgradeImageRefFile: *upgradeImageRefFile,
		rebootCmd:       rebootCmd,
		konturSandboxes: *konturSandboxes,
		konturStateDir:  *konturStateDir,
		konturSSHUser:   *konturSSHUser, konturWorkspace: *konturWorkspace,
		konturExecKey:    *konturExecKey,
		konturCreateArgs: konturCreateArgs, konturNet: *konturNet,
		konturBaseIP: *konturBaseIP, konturBasePort: *konturBasePort,
		konturGitProxyHost: *konturGitProxyHost,
		sandboxCPUs:        *sandboxCPUs, sandboxMemoryMB: *sandboxMemoryMB,
	}); err != nil {
		log.Fatalf("grain daemon: %v", err)
	}
}

type config struct {
	dataDir       string
	maxConcurrent int
	pollInterval  time.Duration
	// sandboxDir roots orchestrator.HostSandboxes -- see -sandbox-dir's
	// own flag doc comment for why this is not just a subdirectory of
	// dataDir. Only consulted when konturSandboxes is false, the same
	// as every other non-kontur-only field would be if HostSandboxes had
	// more than this one.
	sandboxDir string

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

	// agentFramework is this deployment's default agent.Framework --
	// model.AgentFrameworkAntigravity or model.AgentFrameworkClaude -- and is
	// store-backed (model.Config.AgentFramework) the same way geminiModel
	// is: -agent-framework only seeds it the first time a deployment's
	// store has none; `grain settings` (or the Settings UI) is what
	// changes it after that. Only a fallback here: defaultAgentFramework
	// re-reads that stored row per dispatch, so a default changed in the
	// UI takes effect on the next run rather than the next restart, and
	// a task naming a framework of its own never consults either.
	agentFramework string
	// agyPath is flags-only, like claudePath below: where a binary lives
	// is a property of the machine, not of the deployment's stored
	// configuration.
	agyPath string
	// claudePath and claudeOAuthTokenFile are flags-only, like
	// geminiAPIKeyFile -- see -claude-path/-claude-oauth-token-file's own
	// flag doc comments for why neither is store-backed.
	claudePath           string
	claudeOAuthTokenFile string
	// claudeModel is store-backed (model.Config.ClaudeModel), the exact
	// counterpart of geminiModel above: -claude-model only seeds it the
	// first time a deployment's store has none.
	claudeModel string

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
	// upgradeImage/upgradeImageRefFile select v2/pkg/upgrade's image path
	// over the build path above -- what an upgrade means on a host that
	// runs grain from a container (bwsalmon/agents#645).
	upgradeImage        string
	upgradeImageRefFile string

	// rebootCmd is what the UI's reboot-host button runs; empty means
	// rebootHost's own `sudo systemctl reboot` default.
	rebootCmd []string

	// konturSandboxes selects orchestrator.KonturSandboxes over the
	// default orchestrator.HostSandboxes when non-empty; the rest of the
	// kontur* fields are only consulted then. See run()'s own comment on
	// sandboxes.
	konturSandboxes  bool
	konturStateDir   string
	konturSSHUser    string
	konturExecKey    string
	konturWorkspace  string
	konturCreateArgs []string
	konturNet        string
	konturBaseIP     string
	konturBasePort   int
	// konturGitProxyHost is the address startGitProxy advertises to a
	// kontur-managed sandbox in place of the loopback address it binds to
	// by default -- see -kontur-git-proxy-host's own flag doc comment for
	// why a kontur VM cannot reach that default (bwsalmon/agents#567).
	// Required whenever konturSandboxes is set; unused otherwise.
	konturGitProxyHost string
	// sandboxCPUs and sandboxMemoryMB are store-backed
	// (model.Config.SandboxCPUs/SandboxMemoryMB, bwsalmon/agents#534),
	// like poll-interval and the rest of the seedOnly flags above --
	// only consulted with -kontur-sandboxes, the same as every
	// other kontur* field here.
	sandboxCPUs     int
	sandboxMemoryMB int
}

// run wires every piece pkg/orchestrator needs from real, on-disk material
// under cfg.dataDir (the store, secrets) and, for local sandboxing,
// cfg.sandboxDir (HostSandboxes' own per-run directories -- deliberately
// not under cfg.dataDir, see -sandbox-dir's own flag doc comment), and
// starts the reconcile loop; it returns only once
// ctx is cancelled. With -ui-addr set, a failure in the rest of the
// daemon -- runDaemon, below -- no longer counts as "ctx cancelled" for
// this purpose (bwsalmon/agents#550): only a failure to open the store,
// load configuration, or start the UI server itself still returns an
// error here early, since those leave nothing running worth keeping the
// process up for. See runDaemon's own doc comment.
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
	// directory per run, torn down with it; -kontur-sandboxes opts
	// into orchestrator.KonturSandboxes instead: one real
	// bwsalmon/kontur-managed VM per run, reached over
	// SSH (pkg/orchestrator's own doc comment: "Sandboxing defaults to
	// 'execute on the host,' deliberately, for now, with a real host
	// adapter available as an opt in"). sandboxes is Deps.Sandboxes either
	// way; konturSandboxes is kept as its concrete self alongside it
	// purely for the one thing only that backend has -- ReapOrphans, in
	// runDaemon -- and stays nil for a host-backed deployment, which is
	// how that is skipped.
	var sandboxes orchestrator.Sandboxes
	var konturSandboxes *orchestrator.KonturSandboxes
	if cfg.konturSandboxes {
		konturSandboxes = orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
			StateDir:        cfg.konturStateDir,
			CreateArgs:      cfg.konturCreateArgs,
			NetMode:         cfg.konturNet,
			SSHUser:         cfg.konturSSHUser,
			ExecKeyPath:     cfg.konturExecKey,
			Workspace:       cfg.konturWorkspace,
			IP:              cfg.konturBaseIP,
			Port:            cfg.konturBasePort,
			DefaultCPUs:     cfg.sandboxCPUs,
			DefaultMemoryMB: cfg.sandboxMemoryMB,
		})
		sandboxes = konturSandboxes
	} else {
		// orchestrator.NewHostSandboxes' own doc comment says its baseDir
		// "must already exist" -- true of scripts/setup.sh's own
		// GRAIN_SANDBOX_DIR, but not of every caller (the tests below
		// among them), so make that true here rather than leaning on it.
		if err := os.MkdirAll(cfg.sandboxDir, 0o755); err != nil {
			return fmt.Errorf("creating sandbox directory: %w", err)
		}
		sandboxes = orchestrator.NewHostSandboxes(cfg.sandboxDir)
	}

	// transcriptDir is where a run's own agent.Framework may mirror its
	// transcript-in-progress live, and where the UI server below reads one
	// back from for a still-running attempt -- the two sides of
	// bwsalmon/agents#467's live tailing (agent/gemini's own share of it
	// added by bwsalmon/agents#513), agreeing on this
	// directory (and, within it, the run-ID-named file
	// orchestrator.RunDispatch and each framework's LiveTranscriptDir
	// independently compute) is what lets them talk to each other without
	// either package importing the other. It must exist before any run
	// can write into it -- orchestrator's own NewHostSandboxes makes the
	// same "must already exist" assumption about its own baseDir.
	transcriptDir := filepath.Join(cfg.dataDir, "state", "transcripts")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		return fmt.Errorf("creating transcript directory: %w", err)
	}

	// The UI/API server starts here, as early as its own dependencies (the
	// store, sandboxes, and transcriptDir above) allow -- deliberately
	// before the git proxy, the Gemini agent
	// framework, and RunCycle's own reconcile loop, none of which the UI
	// needs a working copy of to serve the store's existing tasks, runs,
	// and logs (bwsalmon/agents#550: "make sure ui with logs stays up even
	// if the daemon has failed"). Everything from here down used to run
	// inline in this same function, so a `return err` from any one of
	// those steps -- a bad -gemini-api-key-file, a git proxy that failed
	// to bind -- or an unrecovered panic anywhere beneath them (a single
	// dispatch's own goroutine included -- see reconcileDispatch's doc
	// comment in pkg/orchestrator/cycle.go) unwound run() itself, which
	// tore this UI server down right along with it via the very defer
	// below. It now runs in runDaemon, in its own goroutine, so that no
	// longer happens: runDaemon's own failure is logged, not fatal, and
	// run() itself only returns once ctx is actually cancelled.
	if cfg.uiAddr != "" {
		stopUI, err := startUIServer(cfg, store, transcriptDir, sandboxes)
		if err != nil {
			return fmt.Errorf("starting the UI/API server: %w", err)
		}
		defer stopUI(context.Background())

		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := runDaemon(ctx, cfg, store, sandboxes, konturSandboxes, transcriptDir); err != nil {
				// reconcilerDown is what turns this log line into
				// something GET /api/config (and, through it, the UI
				// itself) can also see -- bwsalmon/agents#576: before
				// this, a runDaemon failure here was visible only to
				// whoever happened to be reading this process's log,
				// while the UI it left running kept looking perfectly
				// healthy.
				reconcilerDown.Store(true)
				log.Printf("grain daemon: %v -- the UI/API server above is still up, but nothing is "+
					"dispatching or reconciling tasks until this is fixed and the process is restarted", err)
			}
		}()
		select {
		case <-ctx.Done():
		case <-done:
			// runDaemon gave up (or panicked) before ctx was ever
			// cancelled -- already logged above. The UI stays up
			// regardless: wait for a real shutdown signal instead of
			// falling through to the deferred stopUI/db.Close below, which
			// would tear it down right after bringing it up for exactly
			// this reason.
			<-ctx.Done()
		}
		<-done
		return nil
	}

	return runDaemon(ctx, cfg, store, sandboxes, konturSandboxes, transcriptDir)
}

// runDaemon is everything that makes cfg's deployment actually dispatch
// and reconcile tasks: the git proxy, the per-dispatch agent framework
// factory (agentFrameworks), orphaned-run and orphaned-VM recovery, and
// RunCycle's own reconcile loop. Sandbox tokens and git credentials are
// not among them any more:
// both are per run now, minted and configured as each run's sandbox is
// built (orchestrator's runOne). run() calls it exactly once, either
// inline (-ui-addr disabled, so there is no UI server worth
// keeping up on its own -- a setup failure here is still this process's
// only job, so it is still fatal the way it always was) or in a goroutine
// recovered from panics (-ui-addr set), so that nothing in here -- a
// config problem this returns as an error, or a bug that panics -- can
// take the UI server run() already started down with it
// (bwsalmon/agents#550). It returns once ctx is cancelled, the same as
// reconcile itself does; a non-nil error means it never got that far.
func runDaemon(ctx context.Context, cfg config, store *model.Store, sandboxes orchestrator.Sandboxes, konturSandboxes *orchestrator.KonturSandboxes, transcriptDir string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()

	// The git proxy is started before anything can dispatch onto it, but
	// nothing is minted or configured here any more. A sandbox token used
	// to be minted per slot, right here, specifically because the proxy
	// read sandbox-tokens.json once at startup and never again -- so a
	// token minted after it started was one it could never recognise, and
	// every push through it failed closed with a 401 (cmd/graind/
	// live_test.go's TestRunLiveDispatchesAndOpensAPullRequest caught
	// exactly that, live). With a sandbox per run there is no set of
	// sandboxes to mint for ahead of time: each run mints its own as it
	// prepares its sandbox (orchestrator's runOne), and the proxy re-reads
	// the file when shown a token it does not know (gitproxy.
	// SandboxTokens' own doc comment), which is what makes that safe.
	tokens := gitproxy.NewSandboxTokenStore(filepath.Join(cfg.dataDir, "secrets", "sandbox-tokens.json"))

	proxyURL, stopProxy, err := startGitProxy(cfg.dataDir, store, cfg.githubHost, cfg.githubInsecureHTTP, cfg.konturGitProxyHost)
	if err != nil {
		return fmt.Errorf("starting git proxy: %w", err)
	}
	defer stopProxy(context.Background())

	// The secrets database this deployment's UI writes agent credentials
	// into, and the one orchestrator.Config.Credentials below already
	// resolves a capability's own secrets through -- the same directory,
	// opened once and shared by both.
	secretStore := secrets.New(filepath.Join(cfg.dataDir, "secrets"))

	// Built per dispatch now rather than here (agentFrameworks' own doc
	// comment), so nothing about a missing or not-yet-pasted agent
	// credential can stop this loop from starting: a task needing one
	// fails as its own setup-failed run instead, which is a state the UI
	// shows and an operator can fix in Settings without a restart. What
	// is worth saying at startup is that the default framework cannot
	// run yet, since otherwise the first symptom is a task that fails
	// the moment anyone files one.
	buildFramework := agentFrameworks(cfg, store, secretStore)
	if _, err := buildFramework(ctx, ""); err != nil {
		log.Printf("grain daemon: the default agent framework is not ready: %v", err)
	}

	credentials, err := gitproxy.LoadCredentialSet(filepath.Join(cfg.dataDir, "secrets", "github"))
	if err != nil {
		return fmt.Errorf("loading GitHub credential ladder: %w", err)
	}
	githubTransport := github.NewRealTransport(cfg.githubHost)
	githubTransport.UseTLS = !cfg.githubInsecureHTTP
	githubClient := github.NewClient(githubTransport, credentialTokenSource{credentials})

	registry := model.NewCapabilityRegistry(capabilityProviders(cfg)...)

	// Recovering any run a previous process left running (bwsalmon/agents#425)
	// has to happen here, once, before reconcile's first tick -- see
	// orchestrator.RecoverOrphanedRuns's own doc comment for why it is a
	// startup pass rather than something reconcile also runs on a timer.
	if err := orchestrator.RecoverOrphanedRuns(ctx, store, githubClient, time.Now().UTC()); err != nil {
		log.Printf("grain daemon: recovering orphaned runs: %v", err)
	}

	// The sandbox-side half of that same recovery, and at the same moment
	// for the same reason: a run's VM is deleted when the run ends, so at
	// startup -- before this process has dispatched anything -- any VM
	// under this deployment's prefix belongs to a process that died before
	// it could do that. Logged rather than fatal: a VM that cannot be
	// reaped costs some memory on the host, where refusing to start costs
	// the whole deployment, and every run this process dispatches builds
	// its own VM regardless.
	if konturSandboxes != nil {
		if reaped, err := konturSandboxes.ReapOrphans(ctx); err != nil {
			log.Printf("grain daemon: reaping orphaned kontur VMs: %v", err)
		} else if reaped > 0 {
			log.Printf("grain daemon: reaped %d kontur VM(s) left behind by a previous process", reaped)
		}
	}

	inFlight := &orchestrator.InFlight{}
	deps := orchestrator.Deps{
		Store: store, Client: githubClient, Sandboxes: sandboxes,
		Framework: buildFramework,
		Config: orchestrator.Config{
			Capabilities:  registry,
			Credentials:   secretStore,
			MaxAgentTurns: cfg.maxAgentTurns,
			TranscriptDir: transcriptDir,
			// The same proxy URL the credential files above are written
			// for, now also the address each dispatched run's own
			// checkout is cloned from (orchestrator.prepareCheckout).
			// Nothing else ever told a sandbox where its repo lives.
			GitRemoteBase: proxyURL,
			GrantTools:    grantTools(cfg.upgradeSrcDir),
		},
		MintSandboxToken:   tokens.EnsureToken,
		RevokeSandboxToken: tokens.Revoke,
		MaxConcurrent:      cfg.maxConcurrent,
		// A run outlives the cycle that started it (orchestrator.Deps.Runs):
		// without this the loop below could not tick again -- and so could
		// not dispatch into the rest of -max-concurrent, nor sync a single
		// pull request -- until every agent a cycle started had finished.
		Runs: inFlight,
	}
	log.Printf("grain daemon: reconciling every %s across %d concurrent run(s)", cfg.pollInterval, cfg.maxConcurrent)
	reconcile(ctx, deps, cfg.pollInterval)
	// reconcile only returns once ctx is done, which is also what tells
	// every live run to wind up -- so this is the shutdown drain, not a
	// wait for work still to be done.
	drainInFlight(inFlight)
	return nil
}

// shutdownDrain bounds how long the daemon waits, after its reconcile
// loop has stopped, for the runs still in flight to unwind.
//
// It is worth waiting at all because unwinding is not nothing: a
// cancelled run still finishes its own row and still releases its
// sandbox, both on contexts deliberately detached from the cancellation
// (orchestrator.runCleanupTimeout, which bounds each of those two at 2
// minutes). Exiting the instant the loop stops would leave a kontur VM
// running and a run row live for the next process to recover.
//
// It is bounded because a shutdown that never ends is worse than one
// that leaves something behind: a VM this gives up on is reaped at the
// next startup (KonturSandboxes.ReapOrphans), and a run row is recovered
// there too (orchestrator.RecoverOrphanedRuns). Generous enough for both
// cleanup steps of a single run, and no more.
const shutdownDrain = 4*time.Minute + 30*time.Second

// drainInFlight waits for runs to finish unwinding, logging what it is
// waiting on and what it gave up on -- the two things an operator
// watching a slow SIGINT wants to know.
func drainInFlight(runs *orchestrator.InFlight) {
	if runs.Len() == 0 {
		return
	}
	log.Printf("grain daemon: shutting down; waiting up to %s for %d run(s) to release their sandboxes: %v",
		shutdownDrain, runs.Len(), runs.Runs())
	ctx, cancel := context.WithTimeout(context.Background(), shutdownDrain)
	defer cancel()
	if err := runs.Wait(ctx); err != nil {
		log.Printf("grain daemon: gave up waiting on %d run(s) still in flight (%v); "+
			"the next start recovers their rows and reaps their VMs", runs.Len(), runs.Runs())
		return
	}
	log.Printf("grain daemon: every run in flight at shutdown has finished")
}

// agentFrameworks returns the factory orchestrator.Deps.Framework wants:
// the agent.Framework one dispatch is driven by, chosen per run rather
// than once at startup (bwsalmon/agents#615 built exactly one, here, and
// every run used it).
//
// Two things made per-run construction necessary. A task can now name a
// framework of its own (model.Task.AgentFramework) and override the
// deployment-wide default, so which one a run needs is not known until
// that run comes up; and both frameworks' credentials are settable from
// the UI now (pkg/ui's "Agent frameworks" section, writing the
// secrets.GeminiAPIKeySecret/ClaudeOAuthTokenSecret entries this reads),
// so the key a run authenticates with is whatever is in the secrets
// database at the moment it is dispatched -- not whatever a file held
// when this process started.
//
// The cost is one client construction per dispatch instead of one per
// process, which is nothing next to the run itself: both constructors
// resolve a binary and a credential and build a struct.
func agentFrameworks(cfg config, store *model.Store, secretStore *secrets.Store) func(context.Context, string) (agent.Framework, error) {
	return func(ctx context.Context, framework string) (agent.Framework, error) {
		if framework == "" {
			framework = defaultAgentFramework(ctx, store, cfg)
		}
		// Normalized so a task or a config row still carrying the legacy
		// "gemini" spelling dispatches onto the framework that name now
		// means, rather than falling into the error below.
		switch model.NormalizeAgentFrameworkName(framework) {
		case model.AgentFrameworkClaude:
			return buildClaudeFramework(ctx, cfg, secretStore)
		case model.AgentFrameworkAntigravity:
			return buildAntigravityFramework(ctx, cfg, secretStore)
		default:
			// Unreachable through the UI (ui.UpdateSettings and
			// ui.CreateTask both validate against the same two names),
			// so this is a store written by hand or by a newer build --
			// worth naming rather than silently running the default.
			return nil, fmt.Errorf("unknown agent framework %q: expected %q or %q",
				framework, model.AgentFrameworkAntigravity, model.AgentFrameworkClaude)
		}
	}
}

// defaultAgentFramework is the deployment-wide setting a task that named
// no framework of its own is dispatched with. Read from the store on
// every dispatch, not from cfg: unlike the seed-only settings loadConfig
// folds in at startup, this one is a live choice an operator makes in
// Settings and expects the next run to honour, and re-reading one row
// per dispatch is far cheaper than the run it precedes. cfg's own value
// (the -agent-framework flag, seeded into that row the first time) is
// the fallback for a store that cannot be read or has no config row yet.
func defaultAgentFramework(ctx context.Context, store *model.Store, cfg config) string {
	if store != nil {
		if stored, err := store.GetConfig(ctx); err == nil && stored != nil && stored.AgentFramework != "" {
			return model.NormalizeAgentFramework(stored.AgentFramework)
		}
	}
	return model.NormalizeAgentFramework(cfg.agentFramework)
}

// buildAntigravityFramework builds the framework a run driven by
// agent/antigravity uses: Google's Antigravity CLI (agy) as a subprocess,
// authenticating with the same Gemini API key the in-process runtime it
// replaced ran as. Unlike that runtime it needs a binary on the host, so
// this has the same shape buildClaudeFramework does -- resolve the CLI,
// resolve the credential, fail with something an operator can act on if
// either is missing.
func buildAntigravityFramework(ctx context.Context, cfg config, secretStore *secrets.Store) (agent.Framework, error) {
	agyPath := cfg.agyPath
	if agyPath == "" {
		resolved, err := exec.LookPath("agy")
		if err != nil {
			// Named as an install, not as a lookup failure, for the same
			// reason buildClaudeFramework does: an operator who has just
			// switched frameworks in Settings (where nothing mentions a
			// CLI) reads a bare "executable file not found in $PATH" as
			// grain being broken rather than as a host missing a package.
			return nil, fmt.Errorf("the Antigravity CLI (agy) is not installed: %w -- "+
				"the deployment image carries one (v2/Dockerfile), so this is either an image "+
				"built without it or a grain running outside one; deploy an image that has it, "+
				"or point -agy-path at an existing copy", err)
		}
		agyPath = resolved
	}
	// Checked here, so a task dispatched with no key ends as a
	// setup-failed run naming what is missing (orchestrator.runOne's own
	// guard) rather than as an agy subprocess failing to authenticate
	// later with a message that says nothing about where grain reads
	// credentials from.
	apiKey, err := agentCredential(ctx, secretStore, secrets.GeminiAPIKeySecret, cfg.geminiAPIKeyFile)
	if err != nil {
		return nil, fmt.Errorf("reading the Gemini API key: %w", err)
	}
	if apiKey == "" {
		return nil, errors.New("no Gemini API key is configured: set one in the UI " +
			"(Settings -> Agent frameworks), or point -gemini-api-key-file at a file holding one")
	}
	grainBinaryPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving this binary's own path, for agy's MCP settings: %w", err)
	}
	// Resolved again inside the Run rather than closing over the key just
	// read, exactly as buildClaudeFramework does with its token: a key
	// replaced in Settings between this construction and the subprocess
	// actually starting is the one that should be used, and re-reading it
	// is one SQLite row.
	opts := []antigravity.Option{
		antigravity.WithAPIKeyFunc(func(ctx context.Context) (string, error) {
			return agentCredential(ctx, secretStore, secrets.GeminiAPIKeySecret, cfg.geminiAPIKeyFile)
		}),
		antigravity.WithModel(cfg.geminiModel),
	}
	if cfg.konturSandboxes {
		// Only meaningful with -kontur-sandboxes, exactly as for
		// agent/claude: a run dispatched onto a plain
		// orchestrator.HostSandboxes directory reaches it through
		// RunConfig.SandboxRoot instead.
		opts = append(opts, antigravity.WithKonturSSH(cfg.konturSSHUser, cfg.konturExecKey, cfg.konturWorkspace))
	}
	return antigravity.New(agyPath, grainBinaryPath, opts...), nil
}

func buildClaudeFramework(ctx context.Context, cfg config, secretStore *secrets.Store) (agent.Framework, error) {
	claudePath := cfg.claudePath
	if claudePath == "" {
		resolved, err := exec.LookPath("claude")
		if err != nil {
			// Named as an install, not as a lookup failure. Both agent
			// frameworks need a binary, and an operator who has just
			// switched frameworks in Settings (where nothing mentions a
			// CLI at all) reads a bare "executable file not found in
			// $PATH" as grain being broken rather than as something
			// missing an install. The deployment image carries both
			// (v2/Dockerfile), so on a real deployment this means an
			// image built without it -- not a host to go install
			// anything on.
			return nil, fmt.Errorf("the claude CLI is not installed: %w -- "+
				"the deployment image carries one (v2/Dockerfile), so this is either an image "+
				"built without it or a grain running outside one; deploy an image that has it, "+
				"or point -claude-path at an existing copy", err)
		}
		claudePath = resolved
	}
	// Checked here, so a task dispatched with no token ends as a
	// setup-failed run naming what is missing (orchestrator.runOne's own
	// guard), rather than as a claude subprocess failing to authenticate
	// several minutes later with a message about credentials that says
	// nothing about where grain reads them from.
	token, err := agentCredential(ctx, secretStore, secrets.ClaudeOAuthTokenSecret, cfg.claudeOAuthTokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading the Claude Code OAuth token: %w", err)
	}
	if token == "" {
		return nil, errors.New("no Claude Code OAuth token is configured: set one in the UI " +
			"(Settings -> Agent frameworks), or point -claude-oauth-token-file at a file holding one")
	}
	grainBinaryPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving this binary's own path, for claude's --mcp-config: %w", err)
	}
	// Resolved again inside the Run rather than closing over the token
	// just read: a token replaced in Settings between this construction
	// and the subprocess actually starting is the one that should be
	// used, and re-reading it is one SQLite row.
	opts := []claude.Option{
		claude.WithOAuthTokenFunc(func(ctx context.Context) (string, error) {
			return agentCredential(ctx, secretStore, secrets.ClaudeOAuthTokenSecret, cfg.claudeOAuthTokenFile)
		}),
		claude.WithModel(cfg.claudeModel),
	}
	if cfg.konturSandboxes {
		// Only meaningful with -kontur-sandboxes: a run dispatched
		// onto a plain orchestrator.HostSandboxes directory reaches it
		// through RunConfig.SandboxRoot instead (RunDispatch, via
		// cycle.go's own rootedSandbox/vmNamedSandbox split), which
		// needs none of this.
		opts = append(opts, claude.WithKonturSSH(cfg.konturSSHUser, cfg.konturExecKey, cfg.konturWorkspace))
	}
	return claude.New(claudePath, grainBinaryPath, opts...), nil
}

// agentCredential reads the credential one agent framework runs as: the
// secrets database first -- the one the UI writes (pkg/ui's
// handleSetAgentKey), so a key pasted into Settings takes effect on the
// next dispatch with no restart and no file to place -- and only then
// the startup flag naming a file, which is how a deployment seeded one
// before the UI could (scripts/setup.sh's own seed_secret) and how a
// bare `grain daemon` run outside a deployment still can.
//
// "" with no error means this deployment has neither, which is a state
// the daemon now runs in perfectly happily: the UI is up, and that is
// where the missing key gets set. A secret that is simply absent is not
// an error either -- secrets.Store.Resolve reports one as such, and
// every deployment that has only ever used the file has exactly that.
func agentCredential(ctx context.Context, secretStore *secrets.Store, secret, file string) (string, error) {
	if secretStore != nil {
		if value, err := secretStore.Resolve(ctx, secret); err == nil {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed, nil
			}
		}
	}
	if file == "" {
		return "", nil
	}
	value, err := readTrimmedFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		// A path that names nothing is the same "nothing configured this
		// way" as an unset flag: scripts/setup.sh passes
		// -gemini-api-key-file unconditionally, pointing at a file it
		// only writes once a key exists to write.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// reconcilerDown reports whether runDaemon has given up entirely --
// returned a non-nil error, including from a recovered panic -- rather
// than a run-level failure the next tick can still recover from. It was
// once also the "not given up yet" side of a per-slot provisioning step
// that retried with backoff; there is no such step now, since a sandbox
// is prepared per run and a failure there finishes that one run rather
// than wedging the deployment (orchestrator's runOne). Set once, from
// run()'s own goroutine, alongside the log line that already reports the
// same failure; never cleared, since -- like orchestrator.
// ChecksUnavailable -- this is a standing fact about *this process*, not
// an event, and only a restart (a fresh process, with a fresh zero
// value) can turn it back to false. GET /api/config surfaces it as
// reconcilerDown so a UI, or an external monitor polling that same
// endpoint, can see reconciliation is dead without an operator having to
// notice and interpret a single log line (bwsalmon/agents#576).
var reconcilerDown atomic.Bool

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
// simply delays the next dispatch rather than racing it. What a tick no
// longer waits for is the runs it starts: deps.Runs (set in runDaemon)
// makes RunCycle hand each dispatch to a goroutine and return, so a tick
// takes as long as the cycle's decisions rather than as long as its
// agents -- see orchestrator.InFlight for what the old wait cost, which
// was every other task's dispatch for as long as any one run lasted. A
// reap running concurrently with a RunCycle tick is fine either way; the
// two touch disjoint state (a reap only ever deletes a resource no live Lease
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
// Picking up a change made through the store still needs a restart for
// every field but one -- this reads grain_config exactly once, at
// startup, applying no update while RunCycle is running.
// bwsalmon/agents#320 explicitly did not ask for graceful in-flight
// reloading, so run() does not attempt it. MaxConcurrent is the
// exception: RunCycle itself re-reads grain_config every cycle (its own
// doc comment), so cfg.maxConcurrent below only ever matters as the
// value a fresh database is seeded with.
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
	flagCfg.logStoreOverrides(*stored)
	return flagCfg.withModelConfig(*stored), nil
}

// logStoreOverrides logs one line for every seedOnly flag whose
// command-line value doesn't match what's already stored -- withModelConfig
// is about to silently prefer the stored value over it. Without this, an
// operator re-running v2/scripts/setup.sh (or `grain daemon` by hand) with
// a changed flag sees no effect and no explanation until they find `grain
// settings`, the actual way to change a seeded value (bwsalmon/agents#574).
func (c config) logStoreOverrides(mc model.Config) {
	warn := func(flag string, flagVal, storedVal any) {
		if fmt.Sprint(flagVal) != fmt.Sprint(storedVal) {
			log.Printf("loadConfig: ignoring -%s=%v, stored config already has %v -- use 'grain settings' to change it", flag, flagVal, storedVal)
		}
	}
	warn("poll-interval", c.pollInterval, mc.PollInterval)
	warn("max-concurrent", c.maxConcurrent, mc.MaxConcurrent)
	warn("agent-framework", c.agentFramework, mc.AgentFramework)
	warn("gemini-model", c.geminiModel, mc.GeminiModel)
	warn("claude-model", c.claudeModel, mc.ClaudeModel)
	warn("max-agent-turns", c.maxAgentTurns, mc.MaxAgentTurns)
	warn("github-host", c.githubHost, mc.GitHubHost)
	warn("github-insecure-http", c.githubInsecureHTTP, mc.GitHubInsecureHTTP)
	warn("gcp-project", c.gcpProject, mc.GCPProject)
	warn("gcp-agent-service-account", c.gcpServiceAccountEmail, mc.GCPServiceAccountEmail)
	warn("target-repos", c.targetRepos, mc.TargetRepos)
	warn("sandbox-cpus", c.sandboxCPUs, mc.SandboxCPUs)
	warn("sandbox-memory-mb", c.sandboxMemoryMB, mc.SandboxMemoryMB)
}

// toModelConfig is the flag-parsed subset of config that mirrors
// model.Config -- the seed loadConfig writes when a deployment has never
// stored one.
func (c config) toModelConfig() model.Config {
	return model.Config{
		PollInterval: c.pollInterval, MaxConcurrent: c.maxConcurrent,
		AgentFramework: c.agentFramework,
		GeminiModel:    c.geminiModel, ClaudeModel: c.claudeModel, MaxAgentTurns: c.maxAgentTurns,
		GitHubHost: c.githubHost, GitHubInsecureHTTP: c.githubInsecureHTTP,
		GCPProject: c.gcpProject, GCPServiceAccountEmail: c.gcpServiceAccountEmail,
		TargetRepos: c.targetRepos,
		SandboxCPUs: c.sandboxCPUs, SandboxMemoryMB: c.sandboxMemoryMB,
	}
}

// withModelConfig returns c with every store-backed field replaced by
// mc's -- everything loadConfig reads back out of grain_config once a
// row exists.
func (c config) withModelConfig(mc model.Config) config {
	c.pollInterval = mc.PollInterval
	c.maxConcurrent = mc.MaxConcurrent
	c.agentFramework = mc.AgentFramework
	c.geminiModel = mc.GeminiModel
	c.claudeModel = mc.ClaudeModel
	c.maxAgentTurns = mc.MaxAgentTurns
	c.githubHost = mc.GitHubHost
	c.githubInsecureHTTP = mc.GitHubInsecureHTTP
	c.gcpProject = mc.GCPProject
	c.gcpServiceAccountEmail = mc.GCPServiceAccountEmail
	c.targetRepos = mc.TargetRepos
	c.sandboxCPUs = mc.SandboxCPUs
	c.sandboxMemoryMB = mc.SandboxMemoryMB
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
	providers = append(providers, selfdebug.New(), selfrepair.New(), bootstrap.New())
	return providers
}

// grantTools wires selfdebug/selfrepair's own tool-building functions
// into orchestrator.Config.GrantTools -- see that field's own doc
// comment for why this indirection exists instead of a method on
// model.CapabilityProvider. srcDir is cfg.upgradeSrcDir, reused rather
// than asking for a second -source-dir flag: it already names a checkout
// of grain's own source on every deployment v2/scripts/setup.sh builds,
// and self-debug wants read access to exactly that tree.
func grantTools(srcDir string) map[string]func(store *model.Store, taskID string) []mcp.Tool {
	return map[string]func(store *model.Store, taskID string) []mcp.Tool{
		selfdebug.CapabilityName: func(*model.Store, string) []mcp.Tool {
			if srcDir == "" {
				return nil
			}
			return selfdebug.SourceTools(srcDir)
		},
		selfrepair.CapabilityName: func(store *model.Store, taskID string) []mcp.Tool {
			return selfrepair.HostCommandTools(store, taskID, 0, 0)
		},
		bootstrap.CapabilityName: func(*model.Store, string) []mcp.Tool {
			return bootstrap.PlaybookTools()
		},
	}
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

// liveTranscriptDir returns the ui.LiveTranscript reader for
// transcriptDir. It can no longer be one framework's reader chosen at
// startup: a task carries its own model.Task.AgentFramework now, so two
// runs live in this same directory in two different formats
// (RunConfig.TranscriptPath's own doc comment), and picking by
// deployment default would leave every overriding task's live transcript
// blank until it finished.
//
// So the format is decided per file, by what is in it, rather than per
// deployment -- see liveTranscripts.Tail.
func liveTranscriptDir(transcriptDir string) ui.LiveTranscript {
	return liveTranscripts{
		dir:         transcriptDir,
		claude:      claude.LiveTranscriptDir{Dir: transcriptDir},
		antigravity: antigravity.LiveTranscriptDir{Dir: transcriptDir},
	}
}

// liveTranscripts reads a still-running run's transcript-in-progress
// with whichever framework's reader matches the file it finds.
type liveTranscripts struct {
	dir         string
	claude      claude.LiveTranscriptDir
	antigravity antigravity.LiveTranscriptDir
}

// Tail sniffs the file rather than being told which framework wrote it,
// the daemon having no per-run record of the framework to consult.
//
// Both formats are now NDJSON -- agent/claude mirrors claude's own
// --output-format stream-json, agent/antigravity mirrors agy's -- so "does
// it start with a brace", which was enough while the other framework tee'd
// a human-readable narrative, no longer separates them. The discriminator
// is the event key each vocabulary tags its lines with: claude's carry
// "type", agy's carry "event". Sniffing the first line that parses (not
// simply the first line) is what keeps a file caught mid-write readable.
//
// A file that is neither, or has no complete line yet, reads as the
// antigravity form -- whose PartialTranscript renders an empty string for
// it, the same "nothing to show yet" a caller already handles.
func (l liveTranscripts) Tail(runID string) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(l.dir, runID))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if transcriptIsClaude(string(data)) {
		return l.claude.Tail(runID)
	}
	return l.antigravity.Tail(runID)
}

// transcriptIsClaude reports whether a stream-json capture is
// agent/claude's rather than agent/antigravity's, by the key its events
// are tagged with. Unparseable lines are skipped rather than decided on:
// reading a file the framework is still appending to routinely catches a
// half-written one.
func transcriptIsClaude(data string) bool {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var tagged struct {
			Type  string `json:"type"`
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &tagged); err != nil {
			continue
		}
		if tagged.Event != "" {
			return false
		}
		if tagged.Type != "" {
			return true
		}
	}
	return false
}

// startGitProxy serves gitproxy.NewHandler on a random port, and returns
// the URL to point every slot's git credential helper at plus a shutdown
// func. Running it in-process rather than as a separate systemd unit (v1's
// shape, docs/design.md) is exactly bwsalmon/agents#254's "the MCP server
// just uses the local machine" simplification applied to the proxy too:
// one process, one machine, no unit to keep in sync with this one's own
// lifecycle.
//
// advertiseHost is empty for every backend that shares this process's own
// network namespace (HostSandboxes, and every existing deployment before
// bwsalmon/agents#567): the proxy binds loopback only, and the URL it
// hands back names that same loopback address, exactly as it always has.
// A kontur VM's guest runs in its own network namespace behind netshim's
// NAT (third_party/kontur/internal/netshim) with its own unrelated
// 127.0.0.1, so a deployment opting in with -kontur-sandboxes passes
// -kontur-git-proxy-host as advertiseHost instead: the proxy then binds
// every interface rather than just loopback, and hands back a URL naming
// advertiseHost (typically the docker bridge gateway address the guest's
// own outbound NAT routes through to reach this host) instead of the
// address it actually bound.
func startGitProxy(dataDir string, store *model.Store, githubHost string, insecureHTTP bool, advertiseHost string) (url string, stop func(context.Context) error, err error) {
	proxy, err := gitproxy.BuildProxy(gitproxy.BuildConfig{
		DataDir: dataDir, Store: store, ForwardHost: githubHost, ForwardTLS: !insecureHTTP,
	})
	if err != nil {
		return "", nil, err
	}
	bindHost := "127.0.0.1"
	if advertiseHost != "" {
		bindHost = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", bindHost+":0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: gitproxy.NewHandler(proxy)}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("grain daemon: git proxy: %v", err)
		}
	}()
	if advertiseHost == "" {
		return "http://" + ln.Addr().String(), srv.Shutdown, nil
	}
	return fmt.Sprintf("http://%s:%d", advertiseHost, ln.Addr().(*net.TCPAddr).Port), srv.Shutdown, nil
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
func startUIServer(cfg config, store *model.Store, transcriptDir string, sandboxes orchestrator.Sandboxes) (stop func(context.Context) error, err error) {
	// A second CredentialSet, loaded the same way BuildProxy (above) and
	// run's own githubClient each load their own: not hot-reloaded,
	// cheap to load again, and this is the one Settings checks
	// targetRepos against to flag bwsalmon/agents#427's drift before a
	// push ever reaches the proxy with a confusing 500.
	uiCredentials, err := gitproxy.LoadCredentialSet(filepath.Join(cfg.dataDir, "secrets", "github"))
	if err != nil {
		return nil, fmt.Errorf("loading GitHub credential ladder for the UI: %w", err)
	}
	uiCfg := ui.Config{
		Actor:        ui.DefaultActor(actorID(cfg.actor)),
		Capabilities: ui.DefaultCapabilities(),
		Secrets:      secrets.New(filepath.Join(cfg.dataDir, "secrets")),
		Reboot:       rebootHost(cfg.rebootCmd),
		TargetRepos:  cfg.targetRepos,
		Credentials:  uiCredentials,
		// "daemon" reads back this same process's own journal (it always
		// runs as grain-daemon.service -- scripts/setup.sh's own unit --
		// under a real deployment); "git-proxy-audit" reads the audit log
		// startGitProxy's own gitproxy.BuildProxy just wrote alongside it,
		// in the same DataDir; "config-sync" reads the rollout loop that
		// deployed this process in the first place -- terraform/gcp-v2/
		// files/config-sync.sh, installed as grain-v2-config-sync.service
		// by that same terraform's startup.sh. All three are colocated
		// with this process by construction under a real deployment (v2
		// runs the daemon directly on the host that config-sync watches,
		// unlike v1's nested guest -- deploy.sh's own ensure_ops_agent
		// comment) -- unlike Secrets/Credentials above, there is no case
		// here where this deployment has one but not the others
		// (bwsalmon/agents#444, bwsalmon/agents#542).
		Logs: map[string]ui.LogSource{
			"daemon":          systemlog.Journalctl{Unit: "grain-daemon.service"},
			"git-proxy-audit": systemlog.File{Path: filepath.Join(cfg.dataDir, "state", "git-proxy", "audit.log")},
			"config-sync":     systemlog.Journalctl{Unit: "grain-v2-config-sync.service"},
		},
		// liveTranscriptDir reads back whatever the Framework driving a
		// still-running attempt has mirrored so far into
		// transcriptDir/<runID> (transcriptDir's own doc comment on the
		// shared directory convention). antigravity.LiveTranscriptDir and
		// claude.LiveTranscriptDir read two different file formats
		// (RunConfig.TranscriptPath's own doc comment), and with a task
		// able to override this deployment's framework both formats can
		// be in this one directory at once -- so the reader wired here
		// picks between them per file rather than per deployment.
		LiveTranscripts: liveTranscriptDir(transcriptDir),
		// orchestrator.ChecksUnavailable reads process-lifetime state
		// RunCycle's own reconcile loop sets the first time GitHub 403s a
		// check-runs read against this deployment's credential -- see its
		// own doc comment and Config.AutoMergeDegraded's.
		AutoMergeDegraded: orchestrator.ChecksUnavailable,
		// sandboxHealthAdapter wraps whichever of orchestrator.
		// KonturSandboxes/HostSandboxes run() built as sandboxes (exactly
		// one, per run()'s own doc comment) -- both implement Health, just
		// not the interface ui.Config.Sandboxes actually names, since ui
		// does not import pkg/orchestrator (see ui/sandbox_health.go's own
		// doc comment). The sandbox health pane (bwsalmon/agents#536).
		Sandboxes: sandboxHealthAdapter{sandboxes},
		// hostStats reads this same process's own machine, not any one
		// sandbox -- see pkg/sysstat's own doc comment on why that's a
		// separate reading from Sandboxes above.
		HostStats: hostStats,
		// ReconcilerDown mirrors this same process's own package-level
		// reconcilerDown (daemon.go), the same way AutoMergeDegraded above
		// mirrors orchestrator.ChecksUnavailable -- bwsalmon/agents#576.
		ReconcilerDown: func() bool { return reconcilerDown.Load() },
	}
	if cfg.defaultTargetRepo != "" {
		repo, err := model.ParseRepo(cfg.defaultTargetRepo)
		if err != nil {
			return nil, fmt.Errorf("-default-target-repo: %w", err)
		}
		uiCfg.DefaultTarget = &repo
	}
	switch {
	case cfg.upgradeImage != "":
		// The container path: no checkout, no build, no install -- pull
		// the branch's published image, run it once to prove it works,
		// and point the unit's EnvironmentFile at it before restarting.
		// HealthCheckArgs is the same "schema-version" the build path
		// uses, run inside the pulled image (pkg/upgrade/image.go).
		uiCfg.Upgrader = upgrade.New(upgrade.Config{
			Image: &upgrade.ImageConfig{
				Repository: cfg.upgradeImage,
				RefFile:    cfg.upgradeImageRefFile,
				// Only for a deployment that actually runs sandbox
				// containers: it asks the newly pulled image which one
				// it expects (sandboximage.go) and pulls that too, so
				// the two halves of a kontur deployment upgrade
				// together. A host-directory deployment has no second
				// image to keep in step -- see SandboxImageArgs's own
				// doc comment.
				SandboxImageArgs: sandboxImageArgs(cfg.konturSandboxes),
			},
			HealthCheckArgs: []string{"schema-version"},
			RestartCmd:      cfg.upgradeRestartCmd,
			StatusFile:      filepath.Join(cfg.dataDir, "upgrade-status.json"),
		})
	case cfg.upgradeSrcDir != "":
		uiCfg.Upgrader = upgrade.New(upgrade.Config{
			SrcDir:      cfg.upgradeSrcDir,
			BuildCmd:    []string{"make", "container-build"},
			BuiltBinary: filepath.Join(cfg.upgradeSrcDir, "v2", "bin", "grain"),
			InstallPath: cfg.upgradeInstallPath,
			// "schema-version" (schemaversion.go) touches no store and
			// needs no config, so running the newly installed binary
			// with it is a cheap sanity check that the binary itself
			// isn't broken outright before RestartCmd ever cuts over to
			// it (bwsalmon/agents#418/#422).
			HealthCheckArgs: []string{"schema-version"},
			RestartCmd:      cfg.upgradeRestartCmd,
			StatusFile:      filepath.Join(cfg.dataDir, "upgrade-status.json"),
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

// sandboxImageArgs is upgrade.ImageConfig.SandboxImageArgs: the
// subcommand an upgrade runs inside a newly pulled image to learn which
// sandbox container to pull alongside it, or nil for a deployment with
// no sandbox container to keep in step.
func sandboxImageArgs(konturSandboxes bool) []string {
	if !konturSandboxes {
		return nil
	}
	return []string{"sandbox-image"}
}

// defaultRebootCmd is what the UI's reboot-host button runs absent a
// -reboot-cmd: `sudo systemctl reboot`, the exact command v1's
// mcp_server.py already ran for its own `reboot_controller` tool
// (grain/automation/mcp_server.py) and the one line scripts/setup.sh's
// sudoers drop-in used to grant $GRAIN_USER passwordless sudo for. Run
// as the plain, unprivileged user the daemon already runs as, not as
// root itself -- the same "only exactly this one command line"
// restriction v1 gave its own self-repair sudoers file.
var defaultRebootCmd = []string{"sudo", "systemctl", "reboot"}

// rebootHost builds startUIServer's ui.Config.Reboot out of -reboot-cmd.
//
// It is a flag rather than a constant because a daemon in a container
// has no host systemd to talk to: `systemctl reboot` inside the
// container would either fail outright or, worse, reach a systemd that
// isn't the host's. scripts/setup.sh's container unit points this at a
// `touch` of a file under the data dir instead, which a host-side
// systemd path unit watches and turns into the real reboot -- the same
// job the sudoers drop-in used to do, done by something that works
// across the container boundary.
func rebootHost(argv []string) func(context.Context) error {
	if len(argv) == 0 {
		argv = defaultRebootCmd
	}
	return func(ctx context.Context) error {
		return exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
	}
}

// sandboxHealthAdapter adapts orchestrator's own SandboxHealth (a core
// dispatch type) onto ui.SandboxSnapshot (a presentation DTO) field by
// field -- the one place both types are ever in scope together, so
// neither package needs to import the other (see ui/sandbox_health.go's
// own doc comment). inner is run()'s own sandboxes value; it may be nil
// (a test or a future backend with no Sandboxes yet) or simply not
// implement Health, either of which Health below reports as "nothing to
// show" rather than a panic.
type sandboxHealthAdapter struct {
	inner orchestrator.Sandboxes
}

func (a sandboxHealthAdapter) Health(ctx context.Context) []ui.SandboxSnapshot {
	reporter, ok := a.inner.(interface {
		Health(context.Context) []orchestrator.SandboxHealth
	})
	if !ok {
		return nil
	}
	sandboxes := reporter.Health(ctx)
	out := make([]ui.SandboxSnapshot, len(sandboxes))
	for i, s := range sandboxes {
		out[i] = ui.SandboxSnapshot{
			Sandbox:       s.Sandbox,
			Backend:       s.Backend,
			Name:          s.Name,
			Ready:         s.Ready,
			Error:         s.Error,
			LoadAverage:   s.LoadAverage,
			MemoryUsedMB:  s.MemoryUsedMB,
			MemoryTotalMB: s.MemoryTotalMB,
		}
	}
	return out
}

// hostStats is startUIServer's ui.Config.HostStats: this machine's own
// CPU-load/memory pressure, read straight out of /proc by pkg/sysstat --
// see that package's own doc comment for why this, not any one sandbox,
// is what it reports.
func hostStats() (ui.HostPressure, error) {
	snap, err := sysstat.Read()
	if err != nil {
		return ui.HostPressure{}, err
	}
	return ui.HostPressure{
		LoadAverage1:  snap.LoadAverage1,
		LoadAverage5:  snap.LoadAverage5,
		LoadAverage15: snap.LoadAverage15,
		MemoryUsedMB:  snap.MemUsedMB,
		MemoryTotalMB: snap.MemTotalMB,
	}, nil
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
