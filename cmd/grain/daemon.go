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
// run, reached over SSH. -max-workers/-max-mergers cap how many runs
// are in flight at once either way, and a sandbox is built for each of them and
// destroyed with it; nothing
// above pkg/orchestrator.Deps needs to change to serve more than one.
//
// graind originally drove pkg/orchestrate, a package built independently
// of, and in parallel with, pkg/orchestrator (bwsalmon/agents#249) --
// bwsalmon/agents#263 reconciled the two, keeping pkg/orchestrator (issue
// intake, directive parsing, and PR-health sync were all already wired to
// dispatch.Cycle there) and porting pkg/orchestrate's own capability
// resolution/materialization and reconcile-loop shape onto it. See
// README.md for what that merge kept and dropped.
//
// Most of this file's own flags (-max-workers, -max-mergers, -poll-interval, -agent-framework,
// -gemini-model, -claude-model, -codex-model, -max-agent-turns, -github-host, -github-insecure-http, -gcp-project,
// -gcp-agent-service-account, -target-repos) are store-backed now
// (bwsalmon/agents#320):
// loadConfig writes them into model.Store's grain_config row the first
// time a deployment's store has none, and reads them back out of it on
// every start after that, so a UI or a CLI editing model.Config is what
// changes them from then on. Almost all of them change what this process
// is doing without waiting for a restart at all: liveConfig re-reads that
// row once per reconcile tick and hands each change to whatever it
// configures -- see its own doc comment for the full list, and for the
// two settings (-github-host, -github-insecure-http) that genuinely
// cannot be swapped under a live deployment and so are reported to the
// UI as needing a restart. What stays flags-only either has to
// be known before there is a store to read it from (-data-dir) or names
// secret material rather than being configuration itself
// (-gemini-api-key-file, -kontur-ssh-key, -claude-oauth-token-file,
// -openai-api-key-file) -- bwsalmon/agents#320's own "but not the
// secrets." -claude-path and -codex-path join them not because they are
// secret but because, like -kontur-ssh-key, they name something about
// *this host's* filesystem rather than the deployment's own behaviour.
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/agent/antigravity"
	"github.com/bwsalmon/grain/pkg/agent/claude"
	"github.com/bwsalmon/grain/pkg/agent/codex"
	"github.com/bwsalmon/grain/pkg/capability/bootstrap"
	"github.com/bwsalmon/grain/pkg/capability/gcpkey"
	"github.com/bwsalmon/grain/pkg/capability/geminikey"
	"github.com/bwsalmon/grain/pkg/capability/githubsandbox"
	"github.com/bwsalmon/grain/pkg/capability/githubtoken"
	"github.com/bwsalmon/grain/pkg/capability/selfdebug"
	"github.com/bwsalmon/grain/pkg/capability/selfrepair"
	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/hosttop"
	"github.com/bwsalmon/grain/pkg/kontur"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/metrics"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/orchestrator"
	"github.com/bwsalmon/grain/pkg/secrets"
	"github.com/bwsalmon/grain/pkg/sysstat"
	"github.com/bwsalmon/grain/pkg/systemlog"
	"github.com/bwsalmon/grain/pkg/ui"
	"github.com/bwsalmon/grain/pkg/upgrade"
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
	dockerRootDir := fs.String("docker-root-dir", "", "path to the local docker engine's own data root, for the UI's host disk figures only "+
		"(grain/task-148): the sandbox image and every kontur VM's disk overlay are written there rather than under -sandbox-dir, so on a "+
		"deployment that gives sandboxes a volume of their own it is a disk worth a figure. Empty asks `docker info` for it once at startup, "+
		"which is right whenever the daemon can reach the docker socket and sees that path where dockerd does; name it here when it cannot, "+
		"or when this process sees the same directory at a path of its own. A path this process cannot stat is logged once and left out, not "+
		"reported as a broken figure -- a containerised daemon is usually shown docker's socket and not the tree behind it")
	maxWorkers := fs.Int("max-workers", 1, "maximum number of ordinary tasks dispatched at once -- the worker half of the concurrency "+
		"dispatch.Cycle fills (model.Limits)"+seedOnly)
	maxMergers := fs.Int("max-mergers", model.DefaultMaxMergers, "capacity on top of -max-workers that only the merge queue's own "+
		"fix tasks may use -- so a pull request that will not land can be repaired without waiting out whatever ordinary work is "+
		"running, and so that repair can still take a free worker slot when there is one (model.Limits). 0 makes fix tasks contend "+
		"for -max-workers like anything else, which is what every deployment did before this flag existed"+seedOnly)
	// -max-concurrent is what -max-workers was called while it was the
	// whole limit. It stays accepted because a deployment's own systemd
	// unit outlives the build it was written for -- scripts/setup.sh
	// writes the flags in once and the UI's Upgrade button then replaces
	// only the binary -- so a rename that dropped the old spelling would
	// stop the daemon at exactly the moment nobody is watching a terminal.
	// Its zero default is how "not passed" is told from "passed as 0",
	// which flag.Int cannot say on its own.
	maxConcurrent := fs.Int("max-concurrent", 0, "former name of -max-workers, still accepted so an existing unit file keeps working; "+
		"pass one or the other, not both")
	pollInterval := fs.Duration("poll-interval", 30*time.Second, "how often to run a reconcile cycle"+seedOnly)

	uiAddr := fs.String("ui-addr", "127.0.0.1:8420", "address to serve the UI/API on, in-process, over this same store -- empty disables it")
	uiOpen := fs.Bool("ui-open", false, "open the UI in the system's default browser once it's listening")
	actor := fs.String("as", "", "principal the UI/API attributes tasks and comments it creates to (defaults to the OS user)")
	defaultTargetRepo := fs.String("default-target-repo", "", "owner/name a task created through the UI/API with no repo of its own targets")
	targetRepos := fs.String("target-repos", "", "comma-separated owner/name list a task's repo may name -- empty allows any"+seedOnly)

	agentFramework := fs.String("agent-framework", model.AgentFrameworkAntigravity,
		"which agent.Framework a run is driven by by default: \""+model.AgentFrameworkAntigravity+
			"\" (agent/antigravity, the Antigravity CLI's agy binary as a subprocess -- see -agy-path), \""+
			model.AgentFrameworkClaude+"\" (agent/claude, the real claude CLI as a subprocess -- see "+
			"-claude-path/-claude-oauth-token-file) or \""+model.AgentFrameworkCodex+
			"\" (agent/codex, OpenAI's codex CLI as a subprocess -- see -codex-path/-openai-api-key-file). \""+
			model.LegacyAgentFrameworkGemini+"\" is accepted as the "+
			"former spelling of "+model.AgentFrameworkAntigravity+". Seeds the store-backed setting the UI edits, "+
			"and a task can override it for its own dispatch"+seedOnly)
	geminiAPIKeyFile := fs.String("gemini-api-key-file", "", "file holding the Gemini API key the agent runs as. "+
		"Optional now that the key can be set from the UI instead (Settings -> Agent frameworks, stored as the "+
		"\""+secrets.GeminiAPIKeySecret+"\" secret): a key set there wins, and this file is what a deployment "+
		"seeded one with before that existed. With neither, a run driven by the gemini framework fails as "+
		"setup-failed saying so, rather than the daemon refusing to start")
	geminiModel := fs.String("gemini-model", antigravity.DefaultModel, "model the antigravity agent framework calls"+seedOnly)
	claudeModel := fs.String("claude-model", claude.DefaultModel, "model the claude agent framework calls"+seedOnly)
	codexModel := fs.String("codex-model", codex.DefaultModel, "model the codex agent framework calls"+seedOnly)
	maxAgentTurns := fs.Int("max-agent-turns", 0, "cap on model/tool round trips per run (0 = uncapped; runs are bounded by wall-clock runtime instead)"+seedOnly)

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
	codexPath := fs.String("codex-path", "", "path to the codex CLI binary agent/codex runs as a subprocess; "+
		"empty resolves \"codex\" against $PATH instead. Only used when the agent-framework setting is \""+
		model.AgentFrameworkCodex+"\"")
	openAIAPIKeyFile := fs.String("openai-api-key-file", "", "file holding the OpenAI API key the agent "+
		"authenticates as, passed to the codex subprocess as OPENAI_API_KEY. Optional, and the exact "+
		"counterpart of the two files above: the UI stores one as the "+
		"\""+secrets.OpenAIAPIKeySecret+"\" secret and that wins over this file")

	githubHost := fs.String("github-host", "github.com", "GitHub git host -- what the proxy forwards to and, via github.APIHost, where REST calls go; override to point at a mock for local testing"+seedOnly)
	githubInsecureHTTP := fs.Bool("github-insecure-http", false, "speak plain HTTP to -github-host instead of HTTPS (mock servers only)"+seedOnly)

	gcpProject := fs.String("gcp-project", "", "GCP project the gcp-key/gemini-key capabilities mint into; empty disables both"+seedOnly)
	gcpServiceAccountEmail := fs.String("gcp-agent-service-account", "", "the narrow agent service account gcp-key mints keys for"+seedOnly)

	// Upgrading (bwsalmon/agents#396, pkg/upgrade): the UI's own
	// "target a branch and click Upgrade" button. -upgrade-src-dir is the
	// opt in -- empty (the default) disables the whole feature and the
	// pane reports itself unavailable, same as -gcp-project disabling
	// gcp-key/gemini-key above. See scripts/setup.sh for how a
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
	// pkg/upgrade/image.go). Set, it replaces the build-a-binary path
	// above entirely: there is no toolchain on a host that runs grain
	// from an image, so "upgrade to branch X" means pulling the tag CI
	// published for X and pointing the unit's own image ref file at it.
	// Neither -upgrade-src-dir nor -upgrade-install-path is needed then,
	// since nothing here builds or installs a binary: an image deployment
	// passes neither, and the self-debug capability reads the source the
	// image carries rather than a checkout a flag names (see sourceDir).
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
	// systemd to talk to -- scripts/setup.sh points this at a file
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
		"path, *inside the VM's container*, of the private key `kontur exec` authenticates to the guest with. "+
			"Optional, and normally left unset: `kontur run` generates a keypair for each guest it boots and "+
			"hands the guest the public half on its kernel command line, so the default path already holds a "+
			"key that guest authorizes (bwsalmon/kontur's internal/guestkey). Set this only for a custom guest "+
			"image that authorizes a key of its own instead -- e.g. /images/kontur-exec-key for a key placed in "+
			"the directory -kontur-create-arg's own -images-hostpath mounts read-only at /images.")
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
			"-kontur-create-arg=22 to point at scripts/kontur/build-guest.sh's published output, already copied onto "+
			"this host under -images-hostpath's directory (see scripts/kontur/README.md, \"Building and "+
			"publishing\", and scripts/setup.sh's own ensure_kontur_images, which is what actually copies "+
			"it there for terraform/gcp -- -guest-port 22 is not optional: konturctl's own default is 80, "+
			"which silently refuses every connection to this image's actual sshd). Only used with "+
			"-kontur-sandboxes. Under -kontur-net nat, prefer -kontur-base-ip/-kontur-base-port over "+
			"putting -ip/-port here: they are appended last, so they win over this list.")
	konturNet := fs.String("kontur-net", kontur.NetModeFlat,
		"how a kontur VM reaches the network: \"flat\" (the default -- the guest is spliced onto the sandbox "+
			"container's own segment and takes over the address docker assigned it, so -kontur-base-ip/"+
			"-kontur-base-port are unnecessary and ignored) or \"nat\" (kontur's original mode: a private "+
			"subnet per namespace, with an -ip and a forwarded -port assigned per VM). Flat mode needs a guest "+
			"image built from kontur's own guest overlays, for the control link \"kontur exec\" arrives on -- "+
			"scripts/kontur/build-guest.sh produces one.")
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
			"own -cpus (only used with -kontur-sandboxes); 0 uses grain's own default, "+
			strconv.Itoa(kontur.DefaultCPUs)+". "+
			"Overridable per task from the UI/API (model.Task.SandboxCPUs)"+seedOnly)
	sandboxMemoryMB := fs.Int("sandbox-memory-mb", 0,
		"deployment-wide default guest memory, in MiB, for a kontur-managed sandbox VM, passed as `konturctl vm "+
			"create`'s own -memory-mb (only used with -kontur-sandboxes); 0 uses grain's own default, "+
			strconv.Itoa(kontur.DefaultMemoryMB)+". Overridable per task from the UI/API "+
			"(model.Task.SandboxMemoryMB)"+seedOnly)
	sandboxDiskGB := fs.Int("sandbox-disk-gb", 0,
		"deployment-wide default root disk size, in GiB, for a kontur-managed sandbox VM, passed as `konturctl "+
			"vm create`'s own -disk-size-mb (only used with -kontur-sandboxes); 0 uses grain's own default, "+
			strconv.Itoa(kontur.DefaultDiskGB)+" -- every VM is sized explicitly, rather than being left as "+
			"large as the guest image behind it the way it was before. "+
			"Overridable per task from the UI/API (model.Task.SandboxDiskGB)"+seedOnly)
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
		if *konturWorkspace == "" {
			fmt.Fprintln(os.Stderr, "grain daemon: -kontur-workspace is required with -kontur-sandboxes")
			os.Exit(2)
		}
		if *konturGitProxyHost == "" {
			fmt.Fprintln(os.Stderr, "grain daemon: -kontur-git-proxy-host is required with -kontur-sandboxes")
			os.Exit(2)
		}
	}
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })
	if passed["max-concurrent"] {
		if passed["max-workers"] {
			fmt.Fprintln(os.Stderr, "grain daemon: -max-concurrent is the former name of -max-workers; pass one or the other")
			os.Exit(2)
		}
		log.Printf("grain daemon: -max-concurrent is now called -max-workers; using -max-workers=%d", *maxConcurrent)
		*maxWorkers = *maxConcurrent
	}
	if *maxWorkers < 1 {
		fmt.Fprintln(os.Stderr, "grain daemon: -max-workers must be at least 1")
		os.Exit(2)
	}
	if *maxMergers < 0 {
		fmt.Fprintln(os.Stderr, "grain daemon: -max-mergers cannot be negative")
		os.Exit(2)
	}
	// NormalizeAgentFramework first, so the legacy "gemini" spelling a
	// deployment's own unit file may still pass keeps working across the
	// upgrade that replaced that framework with agent/antigravity.
	*agentFramework = model.NormalizeAgentFramework(*agentFramework)
	if !model.ValidAgentFramework(*agentFramework) {
		fmt.Fprintf(os.Stderr, "grain daemon: -agent-framework must be %s\n", model.AgentFrameworkNames())
		os.Exit(2)
	}
	var targetReposList []string
	if strings.TrimSpace(*targetRepos) != "" {
		targetReposList = strings.Split(*targetRepos, ",")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, config{
		dataDir: *dataDir, sandboxDir: *sandboxDir, dockerRootDir: *dockerRootDir,
		maxWorkers: *maxWorkers, maxMergers: *maxMergers,
		pollInterval: *pollInterval,
		uiAddr:       *uiAddr, uiOpen: *uiOpen, actor: *actor, defaultTargetRepo: *defaultTargetRepo,
		targetRepos:      targetReposList,
		agentFramework:   *agentFramework,
		geminiAPIKeyFile: *geminiAPIKeyFile, geminiModel: *geminiModel, maxAgentTurns: *maxAgentTurns,
		agyPath:    *agyPath,
		claudePath: *claudePath, claudeOAuthTokenFile: *claudeOAuthTokenFile, claudeModel: *claudeModel,
		codexPath: *codexPath, openAIAPIKeyFile: *openAIAPIKeyFile, codexModel: *codexModel,
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
		sandboxCPUs:        *sandboxCPUs, sandboxMemoryMB: *sandboxMemoryMB, sandboxDiskGB: *sandboxDiskGB,
	}); err != nil {
		log.Fatalf("grain daemon: %v", err)
	}
}

type config struct {
	dataDir string
	// maxWorkers and maxMergers are this deployment's concurrency, split
	// the way model.Limits is: how many ordinary runs may be live, and
	// how much further capacity only the merge queue's own fix tasks may
	// reach.
	maxWorkers   int
	maxMergers   int
	pollInterval time.Duration
	// sandboxDir roots orchestrator.HostSandboxes -- see -sandbox-dir's
	// own flag doc comment for why this is not just a subdirectory of
	// dataDir. Only consulted when konturSandboxes is false, the same
	// as every other non-kontur-only field would be if HostSandboxes had
	// more than this one.
	sandboxDir string
	// dockerRootDir is where the local docker engine keeps its data, if
	// this deployment would rather say than have hostDisks ask `docker
	// info` -- see -docker-root-dir's own flag doc comment. Nothing but
	// the UI's host disk figures reads it: grain never writes there
	// itself, dockerd does, on grain's behalf.
	dockerRootDir string

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
	// one of model.AgentFrameworks() -- and is
	// store-backed (model.Config.AgentFramework) the same way geminiModel
	// is: -agent-framework only seeds it the first time a deployment's
	// store has none; `grain settings` (or the Settings UI) is what
	// changes it after that. Only a fallback here: dispatchConfig
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
	// codexPath and openAIAPIKeyFile are agent/codex's own half of the
	// two lines above -- flags-only for the same two reasons, a path on
	// this host and secret material -- and codexModel is store-backed
	// like the two models beside it.
	codexPath        string
	openAIAPIKeyFile string
	codexModel       string

	githubHost         string
	githubInsecureHTTP bool

	gcpProject             string
	gcpServiceAccountEmail string

	// upgradeSrcDir, upgradeInstallPath and upgradeRestartCmd configure
	// pkg/upgrade.Upgrader (bwsalmon/agents#396); upgradeSrcDir empty
	// disables it, the same "empty disables" shape gcpProject uses for
	// gcp-key/gemini-key above. upgradeSrcDir is also sourceDir's
	// fallback for the self-debug capability on a deployment with no
	// image to carry its own source.
	upgradeSrcDir      string
	upgradeInstallPath string
	upgradeRestartCmd  []string
	// upgradeImage/upgradeImageRefFile select pkg/upgrade's image path
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
	// sandboxCPUs, sandboxMemoryMB and sandboxDiskGB are store-backed
	// (model.Config.SandboxCPUs/SandboxMemoryMB/SandboxDiskGB,
	// bwsalmon/agents#534 and grain/task-41),
	// like poll-interval and the rest of the seedOnly flags above --
	// only consulted with -kontur-sandboxes, the same as every
	// other kontur* field here.
	sandboxCPUs     int
	sandboxMemoryMB int
	sandboxDiskGB   int
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
	// adapter available as an opt in"). Either way the result is just
	// Deps.Sandboxes: the one thing runDaemon wants beyond that
	// interface -- the startup sweep for whatever a previous process
	// left behind -- both backends now implement, so it asks for it by
	// interface (orphanReaper) rather than by carrying a concrete
	// KonturSandboxes alongside.
	var sandboxes orchestrator.Sandboxes
	if cfg.konturSandboxes {
		konturSandboxes := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
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
			DefaultDiskGB:   cfg.sandboxDiskGB,
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

	// Every named GitHub token this deployment has beyond its default one
	// (grain/task-117), read once here and carried on live below so that
	// the two halves of this process -- the picker the UI offers and the
	// providers the reconcile loop resolves grants against -- are built
	// from one reading of the credential directory rather than two that
	// could disagree if a file appeared between them.
	githubTokens, err := gitHubTokenNames(cfg.dataDir)
	if err != nil {
		return err
	}

	// One liveConfig, shared by the two halves of this process: the
	// reconcile loop refreshes it once per tick and applies what changed
	// (its own doc comment), and the UI server reads it back to report
	// what this deployment is actually running with, as opposed to what
	// is merely stored. Built here because it needs both the config
	// loadConfig just resolved and the sandbox backend above.
	live := newLiveConfig(store, sandboxes, cfg, githubTokens)

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
		stopUI, err := startUIServer(cfg, store, transcriptDir, sandboxes, live)
		if err != nil {
			return fmt.Errorf("starting the UI/API server: %w", err)
		}
		defer stopUI(context.Background())

		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := runDaemon(ctx, cfg, store, sandboxes, transcriptDir, live); err != nil {
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

	return runDaemon(ctx, cfg, store, sandboxes, transcriptDir, live)
}

// orphanReaper is the startup sweep both sandbox backends implement:
// destroy whatever sandboxes a previous process of this deployment left
// behind (orchestrator.KonturSandboxes.ReapOrphans deletes VMs,
// orchestrator.HostSandboxes.ReapOrphans removes directories) and say
// how many. Declared here, where it is consumed, rather than in
// pkg/orchestrator: nothing in the orchestrator itself calls it -- a
// sweep is only ever correct at startup, before any run is live -- so it
// is this binary's requirement of a backend, not part of the Sandboxes
// contract RunCycle works through.
type orphanReaper interface {
	ReapOrphans(ctx context.Context) (int, error)
}

// runDaemon is everything that makes cfg's deployment actually dispatch
// and reconcile tasks: the git proxy, the per-dispatch agent framework
// factory (agentFrameworks), orphaned-run and orphaned-sandbox recovery, and
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
func runDaemon(ctx context.Context, cfg config, store *model.Store, sandboxes orchestrator.Sandboxes, transcriptDir string, live *liveConfig) (err error) {
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

	// Published for the UI/API server started above this one (see
	// livePullRequests): from here on, a dispatched run asking for its own
	// pull request through POST /api/tasks/{id}/pull-request reaches this
	// same client, and so opens exactly the pull request this loop's own
	// finish path would have opened for it.
	livePullRequests.Store(&pullRequestOpener{store: store, client: githubClient})

	registry := model.NewCapabilityRegistry(capabilityProviders(cfg, live.gitHubTokens())...)

	// Recovering any run a previous process left running (bwsalmon/agents#425)
	// has to happen here, once, before reconcile's first tick -- see
	// orchestrator.RecoverOrphanedRuns's own doc comment for why it is a
	// startup pass rather than something reconcile also runs on a timer.
	if err := orchestrator.RecoverOrphanedRuns(ctx, store, githubClient, time.Now().UTC()); err != nil {
		log.Printf("grain daemon: recovering orphaned runs: %v", err)
	}

	// The sandbox-side half of that same recovery, and at the same moment
	// for the same reason: a run's sandbox is destroyed when the run ends,
	// so at startup -- before this process has dispatched anything --
	// every sandbox this deployment owns belongs to a process that died
	// before it could do that. A kontur VM left running costs memory; a
	// host directory left behind costs a whole checkout of disk, and
	// enough of them fill the filesystem the next run's own sandbox has
	// to be created on (orchestrator.HostSandboxes.ReapOrphans). Both
	// backends implement the sweep, so this reaps whichever one is
	// configured.
	//
	// Logged rather than fatal: what cannot be reaped costs resources,
	// where refusing to start costs the whole deployment, and every run
	// this process dispatches builds its own sandbox regardless.
	if reaper, ok := sandboxes.(orphanReaper); ok {
		if reaped, err := reaper.ReapOrphans(ctx); err != nil {
			log.Printf("grain daemon: reaping orphaned sandboxes: %v", err)
		} else if reaped > 0 {
			log.Printf("grain daemon: reaped %d sandbox(es) left behind by a previous process", reaped)
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
			GrantTools:    grantTools(sourceDir(cfg.upgradeSrcDir)),
			// The same registry startUIServer above already handed the
			// UI, so a run that registers itself here is one
			// POST /api/tasks/{id}/sandbox/recreate can actually find.
			SandboxRecreations: sandboxRecreations,
			// One gate for the whole deployment: the first run to meet
			// the agent's own usage limit cancels the rest and stops
			// this loop dispatching until the provider's window resets
			// (orchestrator.Pause). The same object startUIServer above
			// already handed the UI, so the banner an operator sees --
			// and the lift button on it -- is about the gate this loop
			// actually consults (agentPause's own doc comment).
			Pause: agentPause,
		},
		MintSandboxToken:   tokens.EnsureToken,
		RevokeSandboxToken: tokens.Revoke,
		MaxWorkers:         cfg.maxWorkers,
		MaxMergers:         cfg.maxMergers,
		// A run outlives the cycle that started it (orchestrator.Deps.Runs):
		// without this the loop below could not tick again -- and so could
		// not dispatch into the rest of -max-workers, nor sync a single
		// pull request -- until every agent a cycle started had finished.
		Runs: inFlight,
		// The same ring startUIServer above already handed the UI, so
		// what GET /api/metrics reports about this deployment's tick is
		// what this loop actually measured (cycleTimes' own doc comment).
		CycleTimes: cycleTimes,
	}
	log.Printf("grain daemon: reconciling every %s across %d worker run(s) plus %d reserved for the merge queue "+
		"-- all re-read from the store each tick, so changing any of them in Settings needs no restart",
		cfg.pollInterval, cfg.maxWorkers, cfg.maxMergers)
	reconcile(ctx, deps, live)
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
// secrets.GeminiAPIKeySecret/ClaudeOAuthTokenSecret/OpenAIAPIKeySecret
// entries this reads),
// so the key a run authenticates with is whatever is in the secrets
// database at the moment it is dispatched -- not whatever a file held
// when this process started.
//
// The cost is one client construction per dispatch instead of one per
// process, which is nothing next to the run itself: every constructor
// resolves a binary and a credential and builds a struct.
func agentFrameworks(cfg config, store *model.Store, secretStore *secrets.Store) func(context.Context, string) (agent.Framework, error) {
	return func(ctx context.Context, framework string) (agent.Framework, error) {
		// Re-read here, not closed over from startup: which framework a
		// run is driven by *and* which model that framework is asked for
		// are both live choices an operator makes in Settings, and both
		// are consumed right here, one row read before a run that will
		// cost minutes.
		live := dispatchConfig(ctx, store, cfg)
		if framework == "" {
			framework = live.defaultAgentFramework()
		}
		// Normalized so a task or a config row still carrying the legacy
		// "gemini" spelling dispatches onto the framework that name now
		// means, rather than falling into the error below.
		switch model.NormalizeAgentFrameworkName(framework) {
		case model.AgentFrameworkClaude:
			return buildClaudeFramework(ctx, live, secretStore)
		case model.AgentFrameworkCodex:
			return buildCodexFramework(ctx, live, secretStore)
		case model.AgentFrameworkAntigravity:
			return buildAntigravityFramework(ctx, live, secretStore)
		default:
			// Unreachable through the UI (ui.UpdateSettings and
			// ui.CreateTask both validate against the same vocabulary),
			// so this is a store written by hand or by a newer build --
			// worth naming rather than silently running the default.
			return nil, fmt.Errorf("unknown agent framework %q: expected %s",
				framework, model.AgentFrameworkNames())
		}
	}
}

// dispatchConfig is cfg with every store-backed setting a dispatch
// actually consults folded in from grain_config as it stands right now
// -- which agent.Framework to drive a run with, and which model that
// framework is asked for.
//
// Read per dispatch rather than cached at startup for the same reason
// the framework itself is built per dispatch (agentFrameworks' own doc
// comment): these are live choices an operator makes in Settings and
// expects the next run to honour. A store that cannot be read, or has no
// row yet, leaves cfg exactly as the flags parsed it -- which is the
// value that seeded that row in the first place.
func dispatchConfig(ctx context.Context, store *model.Store, cfg config) config {
	if store == nil {
		return cfg
	}
	stored, err := store.GetConfig(ctx)
	if err != nil || stored == nil {
		return cfg
	}
	return cfg.withLiveModelConfig(*stored)
}

// defaultAgentFramework is the deployment-wide setting a task that named
// no framework of its own is dispatched with: this config's own agent
// framework with model.Config.AgentFramework's "empty means antigravity"
// applied. Called on a dispatchConfig result, so what it answers is the
// setting as it stands now rather than as it stood at startup.
func (c config) defaultAgentFramework() string {
	return model.NormalizeAgentFramework(c.agentFramework)
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
				"the deployment image carries one (Dockerfile), so this is either an image "+
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
		// The controller's own GitHub credential ladder, so a run can
		// call pull_request_status and see CI's verdict on the commits
		// it pushed. Not a per-run secret and not a sandbox one: the
		// forked mcpserver reading it runs here, on the controller.
		antigravity.WithGitHubAccess(cfg.dataDir, cfg.githubHost, cfg.githubInsecureHTTP),
	}
	if cfg.konturSandboxes {
		// Only meaningful with -kontur-sandboxes, exactly as for
		// agent/claude: a run dispatched onto a plain
		// orchestrator.HostSandboxes directory reaches it through
		// RunConfig.SandboxRoot instead.
		opts = append(opts, antigravity.WithKonturSSH(cfg.konturSSHUser, cfg.konturExecKey, cfg.konturWorkspace))
	}
	if url := daemonServerURL(cfg.uiAddr); url != "" {
		opts = append(opts, antigravity.WithGrainServer(url))
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
			// (Dockerfile), so on a real deployment this means an
			// image built without it -- not a host to go install
			// anything on.
			return nil, fmt.Errorf("the claude CLI is not installed: %w -- "+
				"the deployment image carries one (Dockerfile), so this is either an image "+
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
		// The controller's own GitHub credential ladder -- see the
		// identical line in buildAntigravityFramework.
		claude.WithGitHubAccess(cfg.dataDir, cfg.githubHost, cfg.githubInsecureHTTP),
	}
	if cfg.konturSandboxes {
		// Only meaningful with -kontur-sandboxes: a run dispatched
		// onto a plain orchestrator.HostSandboxes directory reaches it
		// through RunConfig.SandboxRoot instead (RunDispatch, via
		// cycle.go's own rootedSandbox/vmNamedSandbox split), which
		// needs none of this.
		opts = append(opts, claude.WithKonturSSH(cfg.konturSSHUser, cfg.konturExecKey, cfg.konturWorkspace))
	}
	if url := daemonServerURL(cfg.uiAddr); url != "" {
		opts = append(opts, claude.WithGrainServer(url))
	}
	return claude.New(claudePath, grainBinaryPath, opts...), nil
}

// buildCodexFramework builds the framework a run driven by agent/codex
// uses: OpenAI's codex CLI as a subprocess, authenticating with an
// OpenAI API key. Same shape as the two above -- resolve the CLI,
// resolve the credential, fail with something an operator can act on if
// either is missing -- because a deployment hits exactly the same two
// ways of not being set up.
func buildCodexFramework(ctx context.Context, cfg config, secretStore *secrets.Store) (agent.Framework, error) {
	codexPath := cfg.codexPath
	if codexPath == "" {
		resolved, err := exec.LookPath("codex")
		if err != nil {
			// Named as an install, not as a lookup failure -- see
			// buildClaudeFramework's own comment on why a bare
			// "executable file not found in $PATH" reads as grain being
			// broken to the operator who has just switched frameworks in
			// Settings.
			return nil, fmt.Errorf("the codex CLI is not installed: %w -- "+
				"the deployment image carries one (Dockerfile), so this is either an image "+
				"built without it or a grain running outside one; deploy an image that has it, "+
				"or point -codex-path at an existing copy", err)
		}
		codexPath = resolved
	}
	// Checked here, so a task dispatched with no key ends as a
	// setup-failed run naming what is missing (orchestrator.runOne's own
	// guard) rather than as a codex subprocess failing to authenticate
	// later with a message that says nothing about where grain reads
	// credentials from.
	apiKey, err := agentCredential(ctx, secretStore, secrets.OpenAIAPIKeySecret, cfg.openAIAPIKeyFile)
	if err != nil {
		return nil, fmt.Errorf("reading the OpenAI API key: %w", err)
	}
	if apiKey == "" {
		return nil, errors.New("no OpenAI API key is configured: set one in the UI " +
			"(Settings -> Agent frameworks), or point -openai-api-key-file at a file holding one")
	}
	grainBinaryPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving this binary's own path, for codex's MCP config: %w", err)
	}
	// Resolved again inside the Run rather than closing over the key
	// just read, exactly as the two frameworks above do: a key replaced
	// in Settings between this construction and the subprocess actually
	// starting is the one that should be used, and re-reading it is one
	// SQLite row.
	opts := []codex.Option{
		codex.WithAPIKeyFunc(func(ctx context.Context) (string, error) {
			return agentCredential(ctx, secretStore, secrets.OpenAIAPIKeySecret, cfg.openAIAPIKeyFile)
		}),
		codex.WithModel(cfg.codexModel),
		// The controller's own GitHub credential ladder -- see the
		// identical line in buildAntigravityFramework.
		codex.WithGitHubAccess(cfg.dataDir, cfg.githubHost, cfg.githubInsecureHTTP),
	}
	if cfg.konturSandboxes {
		opts = append(opts, codex.WithKonturSSH(cfg.konturSSHUser, cfg.konturExecKey, cfg.konturWorkspace))
	}
	if url := daemonServerURL(cfg.uiAddr); url != "" {
		opts = append(opts, codex.WithGrainServer(url))
	}
	return codex.New(codexPath, grainBinaryPath, opts...), nil
}

// daemonServerURL is the base URL this same process's own UI/API server
// is reachable at, for a run's forked mcpserver to ask it to open that
// run's pull request (agent/claude's and agent/antigravity's
// WithGrainServer). It is derived from -ui-addr rather than configured
// separately: the two are the same server, and a second flag naming the
// same thing is a second thing to get wrong.
//
// A host-less address (":8420", what a deployment serving on every
// interface passes) becomes loopback, which is both correct and the only
// address worth using here: the asker is a process on this very host.
//
// "" -- no UI/API server at all (-ui-addr emptied out), or one bound to
// port 0, whose real port is only known to the listener rather than to
// this string -- means runs simply get no open_pull_request tool, which
// is the same deployment they had before it existed.
func daemonServerURL(uiAddr string) string {
	if uiAddr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(uiAddr)
	if err != nil || port == "" || port == "0" {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
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

// livePullRequests holds the GitHub-backed pull request opener runDaemon
// builds, for the UI/API server that was started before it to reach.
//
// The two halves of this process come up in that order on purpose: the
// UI/API server starts as early as the store allows, precisely so a
// deployment whose reconcile loop never comes up is still visible and
// operable (bwsalmon/agents#576, and run()'s own comment on the
// ordering). But the GitHub client belongs to runDaemon, below it, so
// POST /api/tasks/{id}/pull-request has nothing to call until runDaemon
// gets there. Set once, from runDaemon, and read through pullRequestGate
// -- the same shape reconcilerDown above already uses to let these two
// halves see one fact about each other without either owning the other.
//
// A run's mcpserver reaching this before it is set gets an honest "not
// ready yet" rather than a nil dereference; in practice nothing can,
// since no run exists to ask until the loop that dispatches runs is
// already going.
var livePullRequests atomic.Pointer[pullRequestOpener]

// cycleTimes is this process's record of its own RunCycle ticks -- how
// long each took, and how far into each one the dispatch decision was
// reached (orchestrator.CycleTimes). GET /api/metrics serves it as the
// "cycles" section beside the runs one, which is where it answers the
// question that report otherwise only raises: a queue wait looks
// identical whether the deployment was at max_concurrent or whether
// there was room all along and the tick was slow to get to dispatch.
//
// Package-level for the same reason reconcilerDown and livePullRequests
// above are: the UI/API server starts before runDaemon builds its Deps,
// so the two halves need one thing they can both name. Unlike those two
// it needs no gate -- it is a value, allocated at process start, and
// reading it before the reconcile loop has ticked reports no ticks,
// which is exactly true of a deployment whose loop has not started (or
// has died: see reconcilerDown).
var cycleTimes = orchestrator.NewCycleTimes(orchestrator.DefaultCycleHistory)

// sandboxRecreations is the set of live runs that can be asked to throw
// their sandbox away and start again in a clean one -- the daemon side
// of pkg/mcp's recreate_sandbox, reached over
// POST /api/tasks/{id}/sandbox/recreate.
//
// Package-level for the reason cycleTimes above is, and with the same
// consequence: the UI/API server starts before runDaemon builds its
// Deps, so the two halves need one object they can both name. It needs
// no gate the way livePullRequests does, because it is not a client to
// be built later but a registry that is empty until runs put themselves
// in it -- and a request arriving before the reconcile loop has started
// finds no live run, which is exactly true of a deployment that has
// dispatched nothing.
var sandboxRecreations = orchestrator.NewSandboxRecreations()

// agentPause is the deployment-wide gate an agent's own usage limit
// closes: while it is shut, the reconcile loop dispatches nothing and
// every run in flight has been cancelled, because each of them would
// spend the same exhausted credential (orchestrator.Pause).
//
// Package-level for the reason cycleTimes and sandboxRecreations above
// are: the UI/API server starts before runDaemon builds its Deps, and
// both halves need to name the same gate -- the loop to consult and
// close it, the server to report it on GET /api/config and
// GET /api/pause and to lift it on DELETE /api/pause. It needs no gate
// of its own like livePullRequests: the zero value is a usable, unpaused
// Pause, and a UI reading it before the reconcile loop exists reports
// nothing paused, which is exactly true of a deployment that has
// dispatched nothing.
var agentPause = &orchestrator.Pause{}

// pullRequestOpener is ui.Config.PullRequests over
// orchestrator.OpenPullRequestForTask: the one place this deployment's
// store and its GitHub client are both in scope for a request that
// arrived over the UI/API. Its caller is a dispatched run's own mcpserver
// (cmd/grain/mcpserver.go's daemonPullRequests), which holds no GitHub
// credential itself.
//
// It converts orchestrator.PullRequestStatus into ui.PullRequestStatus
// field for field, for the same reason sandboxHealthAdapter converts
// orchestrator.SandboxHealth: pkg/ui does not import pkg/orchestrator,
// and this file is where both types are in scope.
type pullRequestOpener struct {
	store  *model.Store
	client github.Client
}

func (o *pullRequestOpener) OpenForTask(ctx context.Context, taskID string) (ui.PullRequestStatus, error) {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return ui.PullRequestStatus{}, err
	}
	if task == nil {
		return ui.PullRequestStatus{}, fmt.Errorf("no task %s", taskID)
	}
	status, err := orchestrator.OpenPullRequestForTask(ctx, o.store, o.client, *task)
	if err != nil {
		return ui.PullRequestStatus{}, err
	}
	out := ui.PullRequestStatus{
		Number:          status.PullRequest.Number,
		URL:             status.PullRequest.HTMLURL,
		ChecksAvailable: status.ChecksKnown,
		ChecksError:     status.ChecksError,
	}
	if task.Target != nil {
		out.Repo = task.Target.String()
	}
	for _, c := range status.Checks {
		check := ui.CheckStatus{Name: c.Name, Status: c.Status}
		if c.Conclusion != nil {
			check.Conclusion = *c.Conclusion
		}
		out.Checks = append(out.Checks, check)
	}
	return out, nil
}

// sandboxRecreateAdapter is ui.Config.SandboxRecreate over
// orchestrator.SandboxRecreations, converting the result field for field
// for the reason pullRequestOpener above converts its own: pkg/ui does
// not import pkg/orchestrator, and this file is where both types are in
// scope.
//
// Unlike pullRequestGate below it needs no gate. The registry it wraps
// is allocated at process start (sandboxRecreations' own doc comment)
// and is simply empty until runs register themselves in it, so a request
// that arrives before -- or after -- the reconcile loop is running gets
// the honest "no live run on this daemon" rather than a nil dereference
// or a made-up failure.
type sandboxRecreateAdapter struct {
	recreations *orchestrator.SandboxRecreations
}

func (a sandboxRecreateAdapter) RecreateForTask(ctx context.Context, taskID string) (ui.SandboxRecreation, error) {
	recreation, err := a.recreations.Recreate(ctx, taskID)
	if err != nil {
		return ui.SandboxRecreation{}, err
	}
	return ui.SandboxRecreation{
		Sandbox:     recreation.Sandbox,
		CheckoutDir: recreation.CheckoutDir,
		Restored:    recreation.Restored,
		Warnings:    recreation.Warnings,
	}, nil
}

// Comment is ui.Config.PullRequestComments over the same client: what
// closing a task says on the pull request that close has orphaned (see
// model.OrphanedPullRequestNote). Here rather than on a type of its own
// because it needs exactly what OpenForTask already holds, and the same
// gate below already answers "has this daemon got a GitHub client yet?"
// for both.
//
// ref arrives as a task_link target ("owner/name#123") because that is
// the only spelling pkg/ui has for a pull request; parsing it back is
// this side's job for the same reason converting the status shape is.
func (o *pullRequestOpener) Comment(ctx context.Context, ref, body string) error {
	pr, err := model.ParsePullRequestRef(ref)
	if err != nil {
		return err
	}
	_, err = o.client.CreateComment(pr.Repo.Owner, pr.Repo.Name, pr.Number, body)
	return err
}

// Close is ui.Config.PullRequestCloser over the same client, and the one
// place in this daemon where a pull request is closed rather than merged
// -- reached only when whoever closed the task asked for it in the same
// request (ui.CloseOptions.ClosePullRequest). It parses ref back the way
// Comment above does, for the same reason.
//
// It closes the pull request and touches nothing else: the branch and
// every commit on it stay where they are, and reopening the pull request
// on GitHub restores it whole.
func (o *pullRequestOpener) Close(ctx context.Context, ref string) error {
	pr, err := model.ParsePullRequestRef(ref)
	if err != nil {
		return err
	}
	return o.client.ClosePullRequest(pr.Repo.Owner, pr.Repo.Name, pr.Number)
}

// pullRequestGate is the ui.Config.PullRequests the UI/API server is
// given: whatever livePullRequests holds by the time a request actually
// arrives, or a plain refusal until runDaemon has put one there.
type pullRequestGate struct{}

func (pullRequestGate) OpenForTask(ctx context.Context, taskID string) (ui.PullRequestStatus, error) {
	opener := livePullRequests.Load()
	if opener == nil {
		return ui.PullRequestStatus{}, errors.New(
			"this daemon has no GitHub client yet, so it cannot open a pull request: " +
				"its reconcile loop has not started (or has failed -- check the daemon log)")
	}
	return opener.OpenForTask(ctx, taskID)
}

// Comment is the same gate for ui.Config.PullRequestComments. Its refusal
// is not thrown away: it ends up quoted in the note grain leaves on the
// task, which is exactly where somebody wondering why the pull request
// was never told should read it.
func (pullRequestGate) Comment(ctx context.Context, ref, body string) error {
	opener := livePullRequests.Load()
	if opener == nil {
		return errors.New(
			"this daemon has no GitHub client yet: " +
				"its reconcile loop has not started (or has failed -- check the daemon log)")
	}
	return opener.Comment(ctx, ref, body)
}

// Close is the same gate for ui.Config.PullRequestCloser. Refusing is the
// right answer rather than a shame here: a daemon with no GitHub client
// cannot close a pull request, and the alternative to saying so is a
// close that silently left one open. The refusal ends up quoted in the
// note on the task, where whoever ticked the box will read it.
func (pullRequestGate) Close(ctx context.Context, ref string) error {
	opener := livePullRequests.Load()
	if opener == nil {
		return errors.New(
			"this daemon has no GitHub client yet: " +
				"its reconcile loop has not started (or has failed -- check the daemon log)")
	}
	return opener.Close(ctx, ref)
}

// reapInterval is how often reconcile calls reapCapabilities -- not
// configurable, since nothing about it needs to race a deployment's own
// -poll-interval: it only has to run comfortably more often than the
// shortest ReapAfter/maxLease cutoff any registered
// model.Reaper carries (24 hours, for all three of gcpkey.Reap,
// githubsandbox.Provider.Reap and geminikey.Capability.Reap) for "clean
// up after N hours if leaked" to actually hold within roughly that
// bound, not "eventually".
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
// bwsalmon/agents#354 asked for it on.
//
// geminikey.Capability is the third, and joined the sweep later
// (grain/task-140): its backstop was a package-level DeleteExpired no
// binary called, so a Gemini key minted for a run whose controller died
// between the mint and the store write was never deleted by anything --
// revokeAll covers only the leases grain still has a record of, which is
// exactly what that case has lost. Its Reap is project-wide rather than
// scoped to one resource's owner the way the other two are, because an
// API key hangs off no service account for a listing to scope to: two
// deployments minting into one GCP project reap each other's leaked keys
// (never each other's live ones, and never the daemon's own operating
// key). See geminikey.Capability.Reap for why that is the accepted
// trade and what avoids it.
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

// reconcile calls orchestrator.RunCycle every poll interval, and
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
//
// Each tick measures itself into deps.CycleTimes (the package-level
// cycleTimes ring, which GET /api/metrics reads back): "waits for one to
// return before the next interval starts" is exactly why a tick's own
// duration is part of how long a queued task waits, and nothing outside
// a test could see it.
//
// The interval, and everything else in live, is re-read from the store
// once per tick rather than fixed for the life of the process: this loop
// is the deployment's own heartbeat, so it is also the natural place to
// notice a setting has changed and hand the change to whatever it
// configures (liveConfig.refresh). deps is the loop's own copy, mutated
// in place by that refresh -- each cycle and each dispatch goroutine
// takes its own copy of it, so nothing already running sees it change
// underneath.
func reconcile(ctx context.Context, deps orchestrator.Deps, live *liveConfig) {
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
	ticker := time.NewTicker(live.current().pollInterval)
	defer ticker.Stop()
	reapTicker := time.NewTicker(reapInterval)
	defer reapTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Before the cycle, so a settings change made since the last
			// tick is in effect for the dispatches this one decides on
			// rather than for the one after it.
			if live.refresh(ctx, &deps) {
				ticker.Reset(live.current().pollInterval)
			}
			tick()
		case <-reapTicker.C:
			reap()
		}
	}
}

// defaultShaper is the sandbox-backend half of a changed sandbox-cpus or
// sandbox-memory-mb: orchestrator.KonturSandboxes implements it, and
// orchestrator.HostSandboxes deliberately does not -- a local directory
// has no CPU or memory shape to default, which is the same reason those
// two settings are documented as meaningless under it. Declared here,
// where it is consumed, rather than in pkg/orchestrator, exactly as
// orphanReaper above is.
type defaultShaper interface {
	SetDefaultShape(orchestrator.Shape)
}

// liveConfig is the store-backed configuration this process actually has
// in effect: what loadConfig read at startup, plus every later change to
// it that a *running* daemon can apply on its own.
//
// bwsalmon/agents#320 left every setting but max-workers needing a
// restart -- loadConfig read grain_config once and nothing ever re-read
// it -- which is the wrong shape for a pane whose whole promise is that
// changing a value changes what the deployment does. refresh is called
// once per reconcile tick and either applies a change itself or leaves
// it to whichever piece already re-reads the store for it:
//
//	poll-interval                     this loop's own ticker (refresh)
//	gcp-project, gcp-agent-service-account
//	                                  the capability registry the next cycle
//	                                  resolves a task's grants against (refresh)
//	sandbox-cpus, sandbox-memory-mb   the default shape the next sandbox is
//	                                  built at (refresh, via defaultShaper)
//	max-workers, max-mergers, max-agent-turns
//	                                  orchestrator.RunCycle's own per-cycle re-read
//	agent-framework, gemini-model, claude-model, codex-model
//	                                  dispatchConfig's own per-dispatch re-read
//	prompt-extension                  the deployment-wide standing instructions
//	                                  RunCycle refreshes every cycle
//	                                  (orchestrator.resolvePromptExtension)
//	target-repos, default-capabilities, environment-name,
//	newest-first, show-closed-by-default,
//	approved-by-default, auto-merge-by-default
//	                                  pkg/ui, which reads grain_config per request
//
// That list is prose, and prose about a growing struct goes stale: pkg/ui's
// own settings_restart_drift_test.go is the check, requiring every
// ui.UpdateSettingsRequest field to be accounted for as one of these or as
// restart-only.
//
// What is left is github-host and github-insecure-http, which are baked
// into the git proxy's forwarder, the GitHub REST transport and the
// github-sandbox capability provider when this process starts -- each of
// them read without synchronisation by whatever request is already in
// flight, so swapping one under a live deployment would be a data race
// rather than a setting change. Those two keep their startup value here
// (config.withLiveModelConfig), which is what makes current() an honest
// answer to "what is this process running with" rather than a copy of
// what is merely stored -- the comparison the Settings pane makes to say
// a change has been saved but not yet applied (ui.Settings.PendingRestart,
// whose own restartOnlySettings list is the other end of this one).
type liveConfig struct {
	store *model.Store
	// sandboxes is this deployment's sandbox backend, asked to adopt a
	// changed default VM shape when it is one that can (defaultShaper).
	// nil, or a backend that is not one, simply means that pair of
	// settings has nothing to apply to.
	sandboxes orchestrator.Sandboxes
	// githubTokens is every named GitHub token this deployment has
	// beyond its default one (gitproxy.CredentialSet.ExtraNames), each of
	// which is a capability of its own (pkg/capability/githubtoken).
	//
	// Fixed for this process's lifetime, unlike everything below it: the
	// credential ladder these names come from is loaded once at startup
	// and is not hot-reloaded (pkg/gitproxy/credentials.go's own doc
	// comment), so adding a token means restarting the daemon the same
	// way changing the default one already does. It is held here anyway
	// because refresh rebuilds the capability registry from scratch, and
	// a rebuild that forgot these would quietly drop every named token's
	// provider the first time somebody changed the GCP project.
	githubTokens []string

	mu      sync.Mutex
	applied config
}

// newLiveConfig starts a liveConfig off at what loadConfig resolved --
// the configuration this process is about to run with, plus the named
// GitHub tokens (githubTokens above) it was started with.
func newLiveConfig(store *model.Store, sandboxes orchestrator.Sandboxes, cfg config, githubTokens []string) *liveConfig {
	return &liveConfig{store: store, sandboxes: sandboxes, githubTokens: githubTokens, applied: cfg}
}

// gitHubTokens is the named GitHub tokens this process started with --
// see the field's own doc comment on why they need no lock: they are set
// once, before either half of this process is running, and never
// change.
func (l *liveConfig) gitHubTokens() []string { return l.githubTokens }

// current is the configuration in effect right now. Safe to call from
// any goroutine -- the UI server calls it to answer GET /api/settings
// while the reconcile loop is refreshing it.
func (l *liveConfig) current() config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.applied
}

// modelConfig is current() in the shape pkg/ui compares the stored row
// against (ui.Config.RunningConfig).
func (l *liveConfig) modelConfig() model.Config { return l.current().toModelConfig() }

// refresh re-reads grain_config and applies whatever changed to the
// pieces of this running daemon that can take it, mutating deps in place
// for the ones a cycle carries. It reports whether the poll interval
// itself changed, which is the one thing its caller has to act on rather
// than something applied here.
//
// A store that cannot be read is logged and otherwise ignored: keeping
// the configuration already in effect is the right answer to a momentary
// read failure, and the next tick tries again.
func (l *liveConfig) refresh(ctx context.Context, deps *orchestrator.Deps) (pollIntervalChanged bool) {
	if l.store == nil {
		return false
	}
	stored, err := l.store.GetConfig(ctx)
	if err != nil {
		log.Printf("grain daemon: re-reading stored configuration: %v", err)
		return false
	}
	if stored == nil {
		return false
	}

	l.mu.Lock()
	was := l.applied
	now := was.withLiveModelConfig(*stored)
	l.applied = now
	l.mu.Unlock()

	changes := now.changesFrom(was)
	if len(changes) == 0 {
		return false
	}
	log.Printf("grain daemon: applying changed settings without a restart: %s", strings.Join(changes, ", "))

	if now.gcpProject != was.gcpProject || now.gcpServiceAccountEmail != was.gcpServiceAccountEmail {
		// Rebuilt rather than edited: capabilityProviders is the one
		// place that decides which providers a given configuration has,
		// and a dispatch goroutine still holding the old registry keeps
		// resolving against it (deps is copied per cycle and per
		// dispatch) rather than seeing one change underneath it.
		deps.Config.Capabilities = model.NewCapabilityRegistry(capabilityProviders(now, l.githubTokens)...)
	}
	if now.sandboxCPUs != was.sandboxCPUs || now.sandboxMemoryMB != was.sandboxMemoryMB || now.sandboxDiskGB != was.sandboxDiskGB {
		if shaper, ok := l.sandboxes.(defaultShaper); ok {
			shaper.SetDefaultShape(orchestrator.Shape{CPUs: now.sandboxCPUs, MemoryMB: now.sandboxMemoryMB, DiskGB: now.sandboxDiskGB})
		}
	}
	return now.pollInterval != was.pollInterval
}

// changesFrom names every store-backed setting that differs between this
// config and the one previously in effect, in the same "-flag: old ->
// new" vocabulary logStoreOverrides uses -- so one log line matches what
// an operator just changed in Settings.
//
// github-host and github-insecure-http are deliberately absent: they can
// never differ here, since withLiveModelConfig never adopts them.
func (c config) changesFrom(prev config) []string {
	var changes []string
	note := func(name string, from, to any) {
		if fmt.Sprint(from) != fmt.Sprint(to) {
			changes = append(changes, fmt.Sprintf("%s %v -> %v", name, from, to))
		}
	}
	note("poll-interval", prev.pollInterval, c.pollInterval)
	note("max-workers", prev.maxWorkers, c.maxWorkers)
	note("max-mergers", prev.maxMergers, c.maxMergers)
	note("agent-framework", prev.agentFramework, c.agentFramework)
	note("gemini-model", prev.geminiModel, c.geminiModel)
	note("claude-model", prev.claudeModel, c.claudeModel)
	note("codex-model", prev.codexModel, c.codexModel)
	note("max-agent-turns", prev.maxAgentTurns, c.maxAgentTurns)
	note("gcp-project", prev.gcpProject, c.gcpProject)
	note("gcp-agent-service-account", prev.gcpServiceAccountEmail, c.gcpServiceAccountEmail)
	note("target-repos", prev.targetRepos, c.targetRepos)
	note("sandbox-cpus", prev.sandboxCPUs, c.sandboxCPUs)
	note("sandbox-memory-mb", prev.sandboxMemoryMB, c.sandboxMemoryMB)
	note("sandbox-disk-gb", prev.sandboxDiskGB, c.sandboxDiskGB)
	return changes
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
// This reads grain_config exactly once, at startup, and is only the
// starting point: liveConfig re-reads the same row once per reconcile
// tick and applies whatever a running daemon can apply (its own doc
// comment lists which settings that is, and which two are left needing a
// restart), so almost every field below now matters only as the value a
// fresh database is seeded with, or as the fallback for a row that has
// no value of its own for it.
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
// operator re-running scripts/setup.sh (or `grain daemon` by hand) with
// a changed flag sees no effect and no explanation until they find `grain
// settings`, the actual way to change a seeded value (bwsalmon/agents#574).
func (c config) logStoreOverrides(mc model.Config) {
	warn := func(flag string, flagVal, storedVal any) {
		if fmt.Sprint(flagVal) != fmt.Sprint(storedVal) {
			log.Printf("loadConfig: ignoring -%s=%v, stored config already has %v -- use 'grain settings' to change it", flag, flagVal, storedVal)
		}
	}
	warn("poll-interval", c.pollInterval, mc.PollInterval)
	warn("max-workers", c.maxWorkers, mc.MaxWorkers)
	warn("max-mergers", c.maxMergers, mc.MaxMergers)
	warn("agent-framework", c.agentFramework, mc.AgentFramework)
	warn("gemini-model", c.geminiModel, mc.GeminiModel)
	warn("claude-model", c.claudeModel, mc.ClaudeModel)
	warn("codex-model", c.codexModel, mc.CodexModel)
	warn("max-agent-turns", c.maxAgentTurns, mc.MaxAgentTurns)
	warn("github-host", c.githubHost, mc.GitHubHost)
	warn("github-insecure-http", c.githubInsecureHTTP, mc.GitHubInsecureHTTP)
	warn("gcp-project", c.gcpProject, mc.GCPProject)
	warn("gcp-agent-service-account", c.gcpServiceAccountEmail, mc.GCPServiceAccountEmail)
	warn("target-repos", c.targetRepos, mc.TargetRepos)
	warn("sandbox-cpus", c.sandboxCPUs, mc.SandboxCPUs)
	warn("sandbox-memory-mb", c.sandboxMemoryMB, mc.SandboxMemoryMB)
	warn("sandbox-disk-gb", c.sandboxDiskGB, mc.SandboxDiskGB)
}

// toModelConfig is the flag-parsed subset of config that mirrors
// model.Config -- the seed loadConfig writes when a deployment has never
// stored one.
//
// It starts from model.DefaultConfig rather than a zero model.Config:
// the settings with no flag behind them here (ApprovedByDefault and
// AutoMergeByDefault, whose default is on) still have to be seeded as
// what a deployment that has never chosen them runs, and PutConfig binds
// every column, so anything left at its Go zero value here is stored as a
// deliberate-looking value nobody chose.
func (c config) toModelConfig() model.Config {
	mc := model.DefaultConfig()
	mc.PollInterval, mc.MaxWorkers, mc.MaxMergers = c.pollInterval, c.maxWorkers, c.maxMergers
	mc.AgentFramework = c.agentFramework
	mc.GeminiModel, mc.ClaudeModel, mc.CodexModel = c.geminiModel, c.claudeModel, c.codexModel
	mc.MaxAgentTurns = c.maxAgentTurns
	mc.GitHubHost, mc.GitHubInsecureHTTP = c.githubHost, c.githubInsecureHTTP
	mc.GCPProject, mc.GCPServiceAccountEmail = c.gcpProject, c.gcpServiceAccountEmail
	mc.TargetRepos = c.targetRepos
	mc.SandboxCPUs, mc.SandboxMemoryMB, mc.SandboxDiskGB = c.sandboxCPUs, c.sandboxMemoryMB, c.sandboxDiskGB
	return mc
}

// withLiveModelConfig is withModelConfig restricted to what a *running*
// daemon can adopt: github-host and github-insecure-http keep the value
// this process started with (liveConfig's own doc comment for why they
// cannot change under a live deployment), so what this returns stays a
// true description of what the process is running rather than of what is
// merely stored.
//
// A stored field with no usable value is skipped rather than adopted: a
// row written before a field existed reads back as that field's zero
// value, and "" is not a model to call, nor is a zero poll interval one
// a ticker can be built from. ui.UpdateSettings refuses to store such a
// value in the first place, so this only ever guards a row written by
// hand, by an older build, or by a migration that added a column.
func (c config) withLiveModelConfig(mc model.Config) config {
	live := c.withModelConfig(mc)
	live.githubHost, live.githubInsecureHTTP = c.githubHost, c.githubInsecureHTTP
	if mc.PollInterval <= 0 {
		live.pollInterval = c.pollInterval
	}
	if mc.MaxWorkers < 1 {
		live.maxWorkers = c.maxWorkers
	}
	if mc.MaxMergers < 0 {
		live.maxMergers = c.maxMergers
	}
	if mc.AgentFramework == "" {
		live.agentFramework = c.agentFramework
	}
	if strings.TrimSpace(mc.GeminiModel) == "" {
		live.geminiModel = c.geminiModel
	}
	if strings.TrimSpace(mc.ClaudeModel) == "" {
		live.claudeModel = c.claudeModel
	}
	// Every deployment upgrading across the column that holds this one
	// reads it back empty (model.Config.CodexModel), so unlike the two
	// above this branch is the ordinary case rather than the guard
	// against a hand-written row: -codex-model's own default is what a
	// codex run uses until an operator names a model in Settings.
	if strings.TrimSpace(mc.CodexModel) == "" {
		live.codexModel = c.codexModel
	}
	return live
}

// withModelConfig returns c with every store-backed field replaced by
// mc's -- everything loadConfig reads back out of grain_config once a
// row exists.
func (c config) withModelConfig(mc model.Config) config {
	c.pollInterval = mc.PollInterval
	c.maxWorkers = mc.MaxWorkers
	c.maxMergers = mc.MaxMergers
	c.agentFramework = mc.AgentFramework
	c.geminiModel = mc.GeminiModel
	c.claudeModel = mc.ClaudeModel
	c.codexModel = mc.CodexModel
	c.maxAgentTurns = mc.MaxAgentTurns
	c.githubHost = mc.GitHubHost
	c.githubInsecureHTTP = mc.GitHubInsecureHTTP
	c.gcpProject = mc.GCPProject
	c.gcpServiceAccountEmail = mc.GCPServiceAccountEmail
	c.targetRepos = mc.TargetRepos
	c.sandboxCPUs = mc.SandboxCPUs
	c.sandboxMemoryMB = mc.SandboxMemoryMB
	c.sandboxDiskGB = mc.SandboxDiskGB
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
//
// githubTokens is this deployment's named GitHub tokens beyond its
// default one (gitHubTokenNames below), one provider each.
func capabilityProviders(cfg config, githubTokens []string) []model.CapabilityProvider {
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
	// One per named GitHub token beyond this deployment's default one
	// (grain/task-117). Registered last, and unconditionally on the names
	// being there: a token is a capability because an operator's own file
	// under secrets/github says so, not because anything in this build
	// enumerates it -- so without these, granting one would be refused as
	// a capability no provider is registered for (model.ResolveGrants),
	// and the run would never dispatch.
	providers = append(providers, githubtoken.Providers(githubTokens)...)
	return providers
}

// gitHubTokenNames is every named GitHub token dataDir's credential
// ladder holds beyond the deployment default -- the names
// capabilityProviders above turns into providers and startUIServer into
// picker rows.
//
// A fourth load of the same ladder (runDaemon's own git proxy, its
// GitHub REST client and the UI server each build one too, all of them
// cheap and none of them hot-reloaded -- see startUIServer's own comment
// on why a second copy is fine): run() needs these names before either
// of those exists, and one reading shared by both halves of the process
// is what keeps the picker and the registry offering the same set.
func gitHubTokenNames(dataDir string) ([]string, error) {
	credentials, err := gitproxy.LoadCredentialSet(filepath.Join(dataDir, "secrets", "github"))
	if err != nil {
		return nil, fmt.Errorf("loading GitHub credential ladder for named-token capabilities: %w", err)
	}
	return credentials.ExtraNames(), nil
}

// defaultSourceDir is where the deployment image carries the source it
// was built from (Dockerfile's own COPY --from=build /src-export). Keep
// the two in step.
const defaultSourceDir = "/usr/local/share/grain/src"

// sourceDir is the tree the self-debug capability reads.
//
// The image's own copy wins over anything a flag names, which is the
// opposite of the usual precedence and is the whole point: read_grain_source
// answers "what is the binary I am running made of", and only the tree
// baked in beside that binary is guaranteed to be the commit it was built
// from. The flag names a host checkout that tracks a *branch* -- an
// upgrade repoints the image without touching it, so it drifts, and a
// rollback leaves it strictly newer than the code actually running.
//
// The flag remains the answer for a deployment that is not a container:
// a source-built install has no /usr/local/share/grain/src, and its
// checkout genuinely is what it is running.
func sourceDir(flagged string) string {
	if info, err := os.Stat(defaultSourceDir); err == nil && info.IsDir() {
		return defaultSourceDir
	}
	return flagged
}

// grantTools wires selfdebug/selfrepair's own tool-building functions
// into orchestrator.Config.GrantTools -- see that field's own doc
// comment for why this indirection exists instead of a method on
// model.CapabilityProvider. srcDir is whatever sourceDir resolved: the
// image's baked-in copy of grain's own source, or the checkout
// -upgrade-src-dir names on a deployment that has no such image.
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
		codex:       codex.LiveTranscriptDir{Dir: transcriptDir},
	}
}

// liveTranscripts reads a still-running run's transcript-in-progress
// with whichever framework's reader matches the file it finds.
type liveTranscripts struct {
	dir         string
	claude      claude.LiveTranscriptDir
	antigravity antigravity.LiveTranscriptDir
	codex       codex.LiveTranscriptDir
}

// Tail sniffs the file rather than being told which framework wrote it,
// the daemon having no per-run record of the framework to consult.
//
// All three formats are NDJSON -- each framework mirrors its own CLI's
// event stream -- so "does it start with a brace", which was enough
// while one framework tee'd a human-readable narrative, separates
// nothing. transcriptFramework is the discriminator, on the key and the
// event names each vocabulary uses.
//
// A file none of them claims, or one with no complete line yet, reads as
// the antigravity form -- whose PartialTranscript renders an empty
// string for it, the same "nothing to show yet" a caller already
// handles.
func (l liveTranscripts) Tail(runID string) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(l.dir, runID))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	switch transcriptFramework(string(data)) {
	case model.AgentFrameworkClaude:
		return l.claude.Tail(runID)
	case model.AgentFrameworkCodex:
		return l.codex.Tail(runID)
	default:
		return l.antigravity.Tail(runID)
	}
}

// transcriptFramework names which framework wrote a capture, by the
// shape of the first line of it that parses -- not simply the first
// line, which is what keeps a file caught mid-write readable.
//
// The three vocabularies are told apart by two facts. agy tags its
// events with "event" where the other two use "type"; and codex's
// "type" is a dotted name from its own thread/item vocabulary
// ("item.completed", "turn.failed") or, on an older codex, a line whose
// payload is nested under "msg" -- where claude's is a bare word
// ("assistant", "result"). A line that is none of these leaves the
// question open and the next line is tried, since a framework whose
// stream this code does not recognize is better rendered as nothing than
// as somebody else's events.
func transcriptFramework(data string) string {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var tagged struct {
			Type  string          `json:"type"`
			Event string          `json:"event"`
			Msg   json.RawMessage `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &tagged); err != nil {
			continue
		}
		switch {
		case tagged.Event != "":
			return model.AgentFrameworkAntigravity
		case len(tagged.Msg) > 0:
			return model.AgentFrameworkCodex
		case strings.Contains(tagged.Type, "."):
			return model.AgentFrameworkCodex
		case tagged.Type != "":
			return model.AgentFrameworkClaude
		}
	}
	return ""
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
func startUIServer(cfg config, store *model.Store, transcriptDir string, sandboxes orchestrator.Sandboxes, live *liveConfig) (stop func(context.Context) error, err error) {
	// A second CredentialSet, loaded the same way BuildProxy (above) and
	// run's own githubClient each load their own: not hot-reloaded,
	// cheap to load again, and this is the one Settings checks
	// targetRepos against to flag bwsalmon/agents#427's drift before a
	// push ever reaches the proxy with a confusing 500.
	uiCredentials, err := gitproxy.LoadCredentialSet(filepath.Join(cfg.dataDir, "secrets", "github"))
	if err != nil {
		return nil, fmt.Errorf("loading GitHub credential ladder for the UI: %w", err)
	}
	disks := hostDisks(cfg)
	uiCfg := ui.Config{
		Actor: ui.DefaultActor(actorID(cfg.actor)),
		// The fixed set grain ships providers for, plus one row per named
		// GitHub token this deployment has beyond its default one
		// (grain/task-117) -- the same names capabilityProviders builds
		// the matching providers from, so every id the picker offers is
		// one a dispatch can actually resolve.
		Capabilities: append(ui.OfferedCapabilities(), ui.GitHubTokenCapabilities(live.gitHubTokens())...),
		Secrets:      secrets.New(filepath.Join(cfg.dataDir, "secrets")),
		Reboot:       rebootHost(cfg.rebootCmd),
		TargetRepos:  cfg.targetRepos,
		Credentials:  uiCredentials,
		// "daemon" reads back this same process's own journal (it always
		// runs as grain-daemon.service -- scripts/setup.sh's own unit --
		// under a real deployment); "git-proxy-audit" reads the audit log
		// startGitProxy's own gitproxy.BuildProxy just wrote alongside it,
		// in the same DataDir; "config-sync" reads the rollout loop that
		// deployed this process in the first place -- terraform/gcp/
		// files/config-sync.sh, installed as grain-config-sync.service
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
			"config-sync":     systemlog.Journalctl{Unit: "grain-config-sync.service"},
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
		// separate reading from Sandboxes above. It takes a list of
		// paths because a disk figure has to name a filesystem, and this
		// deployment has more than one worth naming (hostDisks' own doc
		// comment on which). Resolved once, out here, rather than per
		// poll: which filesystems they are is fixed for the life of the
		// process, and finding docker's own asks its engine.
		HostStats: func() (ui.HostPressure, error) { return hostStats(disks) },
		// hostTop is the per-process half of that same reading -- which
		// processes are spending the machine HostStats above says is
		// busy (pkg/hosttop's own doc comment). Same machine, and in a
		// container deployment the same PID namespace this process is
		// in: `docker run` is given no --pid=host (scripts/setup.sh's
		// own docker_run_args), so what this lists is the daemon and
		// everything it forked, which is where daemon-side load comes
		// from. A sandbox's own processes belong to its VM and are not
		// in it -- that is what the sandbox rows beside this report.
		HostTop: hostTop,
		// ReconcilerDown mirrors this same process's own package-level
		// reconcilerDown (daemon.go), the same way AutoMergeDegraded above
		// mirrors orchestrator.ChecksUnavailable -- bwsalmon/agents#576.
		ReconcilerDown: func() bool { return reconcilerDown.Load() },
		// RunningConfig is what this process actually has in effect right
		// now (liveConfig's own doc comment), which the Settings pane
		// compares the stored row against to say which of the two
		// restart-only settings have been saved but not yet applied.
		RunningConfig: live.modelConfig,
		// PullRequests is how a dispatched run opens its own pull request
		// before it exits (ui.Config.PullRequests' own doc comment): its
		// mcpserver asks this API, which asks the daemon's GitHub client.
		// The gate, rather than that client directly, because this server
		// starts before runDaemon has built one -- deliberately, so the
		// UI survives a reconcile loop that never comes up at all -- and
		// livePullRequests is what closes that gap once it does.
		PullRequests: pullRequestGate{},
		// PullRequestComments is how closing a task says so on the pull
		// request that close just orphaned (ui.Config.PullRequestComments'
		// own doc comment). Behind the same gate, and for the same reason
		// -- and where PullRequests turns a missing client into a refused
		// request, this one turns it into a line in the note left on the
		// task saying the pull request itself was not told.
		PullRequestComments: pullRequestGate{},
		// PullRequestCloser is the other half of that same close, and the
		// only route in a grain deployment by which a pull request is
		// ever closed rather than merged: a human who ticked the box
		// beside Close asking for this task's pull request to be closed
		// with it (ui.CloseOptions.ClosePullRequest). Behind the same
		// gate as the two above. A deployment that wanted its UI able to
		// say things on GitHub but never to shut anything would leave
		// exactly this field nil, which is why it is a field of its own.
		PullRequestCloser: pullRequestGate{},
		// SandboxRecreate is how a dispatched run gets out of a sandbox
		// it has broken beyond what it can fix from inside one
		// (ui.Config.SandboxRecreate's own doc comment): its mcpserver
		// asks this API, which asks the registry the run put itself in
		// when it was dispatched. No gate, unlike PullRequests above --
		// see sandboxRecreations' own doc comment.
		SandboxRecreate: sandboxRecreateAdapter{sandboxRecreations},
		// The reconcile loop's own tick, for GET /api/metrics' "cycles"
		// section. No gate needed, unlike PullRequests above: cycleTimes
		// is allocated at process start and runDaemon writes into that
		// same ring once it gets there (cycleTimes' own doc comment).
		Cycles: cycleTimesAdapter{cycleTimes},
		// The dispatch gate an agent's usage limit closes, for the
		// banner that says why a queue of ready tasks has nothing
		// running and for the operator who wants it open again
		// (ui.Config.AgentPause's own doc comment). No adapter: unlike
		// Sandboxes above there is no shape to convert, and
		// *orchestrator.Pause satisfies ui.AgentPause as it stands. No
		// gate either, for the reason Cycles above needs none --
		// agentPause is allocated at process start, and reading it
		// before runDaemon gets there reports nothing paused.
		AgentPause: agentPause,
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
			BuiltBinary: filepath.Join(cfg.upgradeSrcDir, "bin", "grain"),
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
			DiskUsedMB:    s.DiskUsedMB,
			DiskTotalMB:   s.DiskTotalMB,
		}
	}
	return out
}

// cycleTimesAdapter adapts orchestrator's own CycleTiming (what a cycle
// measured about itself) onto metrics.CycleSample (what pkg/metrics
// summarises), field for field -- the one place both types are ever in
// scope, so neither package needs to import the other, exactly as
// sandboxHealthAdapter above does for the sandbox pane.
//
// It converts eagerly rather than handing over the ring: a report is a
// read of a bounded slice a few hundred entries long, taken once per
// GET /api/metrics, and copying it is what keeps the reconcile loop's
// own record out of reach of everything downstream of it.
type cycleTimesAdapter struct {
	inner *orchestrator.CycleTimes
}

func (a cycleTimesAdapter) CycleTimes() metrics.CycleHistory {
	recent, observed := a.inner.History()
	out := metrics.CycleHistory{
		Observed: observed,
		Samples:  make([]metrics.CycleSample, 0, len(recent)),
	}
	for _, c := range recent {
		sample := metrics.CycleSample{
			Start:        c.Start,
			Duration:     c.Duration,
			DispatchWait: c.DispatchWait,
			Reconcilers:  make([]metrics.ReconcilerSample, 0, len(c.Reconcilers)),
		}
		for _, r := range c.Reconcilers {
			sample.Reconcilers = append(sample.Reconcilers, metrics.ReconcilerSample{
				Name:     r.Name,
				Wait:     r.Wait,
				Duration: r.Duration,
				Failed:   r.Failed,
			})
		}
		out.Samples = append(out.Samples, sample)
	}
	return out
}

// hostDisk is one filesystem hostStats reports a figure for: a path to
// take the reading through, and the word the UI shows for what of the
// daemon's own state lives on it (ui.DiskUsage.Holds).
type hostDisk struct {
	holds string
	path  string
}

// hostDisks lists those filesystems, in the order the pane shows them.
//
// None of these is "/": the daemon runs in a container whose root
// filesystem is an image layer nobody's runs fill up. What fills is
// whichever volume the state below sits on --
//
//	store       -data-dir, the store and the secrets. Small, and on
//	            terraform/gcp a 20 GB disk of its own.
//	sandboxes   -sandbox-dir, orchestrator.HostSandboxes' per-run
//	            checkouts. Empty (and so absent here) under
//	            -kontur-sandboxes, which keeps no checkout on this host.
//	docker      docker's data root, which is where the sandbox image and
//	            every kontur VM's qcow2 overlay actually land -- konturctl
//	            creates a VM's writable root inside its own container
//	            (bwsalmon/kontur#37), so nothing grain writes itself ever
//	            passes through a path grain names.
//
// -- and only the last two of those grow with what a run does. Reporting
// -data-dir alone, as this did while all three shared one volume, showed
// a near-empty disk reading as healthy at exactly the moment a runaway
// build was filling the 100 GB one beside it (grain/task-148, after
// grain/task-134 gave sandboxes that volume).
//
// Two of these are usually one filesystem: terraform/gcp bind-mounts the
// sandbox volume's own sandboxes/ onto -sandbox-dir and points dockerd's
// data root at its docker/, so those two entries answer identically.
// hostStats folds them together rather than drawing the same bar twice.
func hostDisks(cfg config) []hostDisk {
	disks := []hostDisk{{holds: "store", path: cfg.dataDir}}
	if cfg.sandboxDir != "" {
		disks = append(disks, hostDisk{holds: "sandboxes", path: cfg.sandboxDir})
	}
	if root := dockerRootDir(cfg.dockerRootDir); root != "" {
		// Read once, here, rather than left to show as a permanently
		// broken row on every poll. A containerised daemon is normally
		// not shown docker's data root at all -- scripts/setup.sh mounts
		// the socket, not the tree behind it -- and on terraform/gcp that
		// root is on the very volume the sandbox entry above already
		// reports, reached through the bind mount onto -sandbox-dir. So a
		// path dockerd names but this process cannot stat is the ordinary
		// case rather than a fault, and it costs no figure the pane does
		// not already have.
		if _, _, err := sysstat.DiskUsage(root); err != nil {
			log.Printf("grain daemon: docker's data root %s is not readable from here (%v); the host status pane reports no docker disk of its own", root, err)
		} else {
			disks = append(disks, hostDisk{holds: "docker", path: root})
		}
	}
	return disks
}

// dockerRootTimeout bounds the one `docker info` dockerRootDir runs. It
// is asked once per process, at startup, so this only ever costs a
// deployment whose docker socket is mounted but whose engine is wedged --
// and then it costs it once, not once per poll.
const dockerRootTimeout = 10 * time.Second

// dockerRootDir resolves docker's data root for hostDisks above:
// whatever -docker-root-dir said, or else what the local engine says
// about itself. Empty means "no docker disk figure", which is what a
// deployment with no docker socket mounted (scripts/setup.sh only mounts
// it for kontur and the Upgrade button) or no engine running gets.
//
// Asked once and logged once rather than polled: dockerd's data root is
// fixed for the life of the daemon it belongs to, and a deployment that
// moves it restarts both.
func dockerRootDir(configured string) string {
	if configured != "" {
		return configured
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerRootTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info", "-f", "{{.DockerRootDir}}").Output()
	if err != nil {
		log.Printf("grain daemon: asking docker where its data root is: %v (the host status pane reports no docker disk; pass -docker-root-dir to say)", err)
		return ""
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		log.Printf("grain daemon: `docker info` named no data root; the host status pane reports no docker disk")
		return ""
	}
	log.Printf("grain daemon: docker's data root is %s; reporting its disk in the host status pane", root)
	return root
}

// hostStats is startUIServer's ui.Config.HostStats: this machine's own
// CPU-load/memory pressure, read straight out of /proc by pkg/sysstat,
// plus one usage figure per filesystem in disks -- see that package's own
// doc comment for why this, not any one sandbox, is what it reports, and
// hostDisks above for which filesystems those are.
//
// A failing disk reading is carried on the disk it belongs to rather
// than failing the whole call: load and memory came from a different
// file and are still good, and the other disks' readings are unaffected
// by one path that has stopped answering. hostDisks has already dropped
// the paths that were never readable to begin with, so a failure here
// means a filesystem that answered at startup and does not now -- a
// volume that went away, which is worth saying rather than worth
// suppressing.
func hostStats(disks []hostDisk) (ui.HostPressure, error) {
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
		Disks:         diskUsage(disks),
	}, nil
}

// diskUsage reads each of disks' own filesystem, folding entries that
// turn out to be the same filesystem into one -- st_dev, not the path,
// decides that (sysstat.FilesystemID's own doc comment on why a path
// comparison cannot: the sandbox volume reaches -sandbox-dir through a
// bind mount that shares no prefix with docker's root beside it).
//
// The first path onto a given filesystem keeps the entry and every later
// one only adds its word to Holds, so the order hostDisks chose is the
// order the pane reads in: the store, then the disk a run fills.
//
// A filesystem whose identity cannot be read but whose usage can is left
// unfolded rather than dropped: two identical rows say too much, and no
// row at all says too little.
func diskUsage(disks []hostDisk) []ui.DiskUsage {
	var out []ui.DiskUsage
	byFilesystem := map[uint64]int{}
	for _, d := range disks {
		totalMB, usedMB, err := sysstat.DiskUsage(d.path)
		if err != nil {
			out = append(out, ui.DiskUsage{Holds: []string{d.holds}, Path: d.path, Error: err.Error()})
			continue
		}
		if id, idErr := sysstat.FilesystemID(d.path); idErr == nil {
			if i, ok := byFilesystem[id]; ok {
				out[i].Holds = append(out[i].Holds, d.holds)
				continue
			}
			byFilesystem[id] = len(out)
		}
		out = append(out, ui.DiskUsage{Holds: []string{d.holds}, Path: d.path, UsedMB: usedMB, TotalMB: totalMB})
	}
	return out
}

// hostTopTimeout bounds one `top` run. Its own sampling interval is a
// fraction of a second (pkg/hosttop), so anything near this means top is
// stuck rather than slow -- and the Debug pane polls, so a request left
// hanging would pile a second one on top of it every few seconds.
const hostTopTimeout = 15 * time.Second

// hostTop is startUIServer's ui.Config.HostTop: a `top` snapshot of this
// same process's own machine, for the Debug overlay's Top tab.
//
// The timeout is added here rather than inside pkg/hosttop for the same
// reason hostStats above takes its path as an argument: the package
// reads what it is asked to read, and how long this deployment is
// willing to wait for it is the daemon's own policy. The request's ctx
// still bounds it from the other end -- whichever fires first kills top.
func hostTop(ctx context.Context, lines int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, hostTopTimeout)
	defer cancel()
	return hosttop.Read(ctx, lines)
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
