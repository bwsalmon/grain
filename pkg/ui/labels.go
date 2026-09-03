// Package ui is a JSON API (and the static frontend it serves) for
// creating and managing grain tasks and their capability grants.
//
// It reads and writes model.Store directly. That is the whole of the
// change bwsalmon's "the input is via direct model updates" asked for:
// this package used to read and write GitHub issues, deriving a task's
// state from labels and keeping its declared fields (/repo, /base,
// /auto-merge) as directive lines in an issue body, because the
// store-backed intake path was not wired into anything that ran. GitHub
// was the record and this was a view onto it.
//
// Now the store is the record. A task is a row, its state comes from
// model.StateOf's own view rather than a second label-shaped derivation
// that had already drifted (this package used to call the same state
// "needs_approval" that the store calls "proposed", and had an
// "untracked" the store has no notion of), its capabilities are
// model.Grants, and its conversation is model.Comments. Nothing here
// talks to GitHub at all: the pull request a task's run produces is
// recorded on the task as a model.LinkFixes link, so rendering it needs
// no API call.
package ui

import (
	"context"

	"github.com/bwsalmon/grain/pkg/capability/githubtoken"
	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/metrics"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/secrets"
	"github.com/bwsalmon/grain/pkg/upgrade"
)

// Capability is one attachable capability a human toggles on a task --
// the CAPABILITY-tier rows of v1's labels.py _STYLES table that were
// genuinely human-driven. A deployment can start every new task with
// some of these already ticked (model.Config.DefaultCapabilities), and a
// repo can add more of its own for the tasks that target it
// (model.RepoConfig.DefaultCapabilities). Either changes what the picker
// opens as, not what it can do: every row here is still something a
// human turns on and off per task.
//
// Label is gone from this type. It named the GitHub label that used to
// carry the grant; a grant is a model.Grant row now, and ID is what it
// records. waiting_on_dependency_label stays excluded for the reason it
// always was: labels.py's own doc comment marks it as the one grain
// applies itself, never a human, so it does not belong in a picker a
// human drives.
type Capability struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Ready is CapabilityStatus.Ready for this same capability -- whether
	// this deployment has the settings and secrets a task granted it
	// would actually need -- carried on the picker's own row so ticking
	// one that cannot work says so here rather than at the far end of a
	// dispatch.
	//
	// That gap is what this field is for. The Settings pane has known
	// since bwsalmon/agents#611 that gemini-key on a deployment with no
	// GCP project, or no `gcp-key-minter` secret, is Not ready; the
	// picker a human actually attaches capabilities with knew nothing
	// about it and offered the row regardless. Ticking it filed an
	// ordinary grant, and the first sign of trouble was the task's own
	// run failing with "capability \"gemini-key\" refused: ..." some
	// minutes later -- with the fix being in Settings, two panes away
	// from where the mistake was made.
	//
	// A pointer, and omitted when nil, because "nobody computed this" is
	// a third answer distinct from ready and not-ready:
	// OfferedCapabilities is a static listing with no deployment behind
	// it, and a false there would libel every capability grain ships. Only
	// GET /api/config fills it in. The frontend reads `ready === false`
	// for the same reason CapabilitiesPanel reads `grantable === false`.
	Ready *bool `json:"ready,omitempty"`
	// Needs is CapabilityStatus.MissingConfig and MissingSecrets run
	// together -- every deployment setting and every secret this
	// capability is still short of, in the same human-facing words the
	// Capabilities tab uses, so the picker can name the gap rather than
	// only report that there is one. Empty whenever Ready is nil or true.
	Needs []string `json:"needs,omitempty"`
}

// OfferedCapabilities is every capability a human can attach to a task,
// and -- because grantsFor and SetCapability reject an id with no row
// here as "unknown capability" before a model.Grant is ever written --
// the only route by which any capability is ever granted at all. It
// started as labels.py's _STYLES table, human-toggled rows only; it is
// now kept in step with the set of providers grain ships instead, which
// is what capabilityCatalog (capability_status.go) enumerates and what
// TestOfferedCapabilitiesCoversEveryShippedCapability holds to.
//
// That drift is why this comment is longer than the table. gcp-key and
// github-sandbox both had providers cmd/grain/daemon.go registered and
// no row here, so no task on any deployment, however configured, could
// ask for either: gcp-key's symptom was that no sandbox ever got
// gcpkey.SandboxKeyPath, with nothing refused, logged or failed
// anywhere, because model.ResolveGrants was never reached (an
// ungrantable capability is rejected a step earlier than a
// misconfigured one). The reverse drift was here too: "scratch-repo" is
// v1's label name for what v2's provider calls github-sandbox, so
// ticking it wrote a grant no provider answers to and
// prepareCapabilities failed the dispatch outright. The row below
// carries the provider's own id.
//
// The name is Offered, not Default, because this is what a picker may
// show, not what a task starts with: the second question -- gcp-key,
// which v1 minted for every sandbox unconditionally
// (grain/automation/gcp_keys.py) -- is model.Config.DefaultCapabilities,
// a per-deployment subset of these ids that (*Client).CreateTask seeds
// every new task's Grants from, plus model.RepoConfig.
// DefaultCapabilities, the same thing per repo. A row here is what makes
// an id eligible for either set (UpdateSettings and
// SetRepoDefaultCapabilities both validate against this listing), so
// this stays the one gate every grant passes through.
//
// Seeding at creation rather than granting at dispatch is what settles
// the failure mode v1 needed a local `except` for. v1 minted per
// dispatch and swallowed the failure, so a broken minter degraded one
// sandbox silently; prepareCapabilities (pkg/orchestrator/run.go) treats
// a refused resolve or a failed materialize as no dispatch at all, and
// an always-grant set read at dispatch would have turned an expired
// minter credential into a standing veto on every task in the
// deployment. A seeded grant needs no separate policy for that: it is an
// ordinary grant on an ordinary task, so it fails that one task loudly
// -- and it can be taken off the task, or out of the default set, by
// whoever is looking at the failure.
func OfferedCapabilities() []Capability {
	return []Capability{
		{ID: "gcp-key", Name: "GCP key",
			Description: "Mint a short-lived GCP service-account key for this task and place it in its sandbox"},
		{ID: "gemini-key", Name: "Gemini key",
			Description: "Mint a short-lived Gemini API key for this task"},
		{ID: "self-debug", Name: "Self debug",
			Description: "Let this task read grain's own source checkout, to help it debug or explain grain's own behavior"},
		{ID: "self-repair", Name: "Self repair",
			Description: "Let this task run commands on grain's own host -- restart services, edit config, call the grain CLI -- each one needing a live reply in the task's chat before it runs"},
		{ID: "bootstrap-playbooks", Name: "Bootstrap playbooks",
			Description: "Let this task read grain's own bootstrap playbooks -- the runbooks for setting up GCP service accounts, the primary GitHub connection, CloudRun-based IAP access, and test repos -- so it can walk whoever is on the other end of this chat through one of them"},
		{ID: "github-sandbox", Name: "GitHub sandbox",
			Description: "Create a private, single-use GitHub sandbox repo for this task to work in instead of /repo"},
	}
}

// GitHubTokenCapabilities is one row per named GitHub token this
// deployment has configured beyond its default one
// (gitproxy.CredentialSet.ExtraNames), for a caller to append to
// OfferedCapabilities above -- grain/task-117.
//
// These are the one part of the picker that is not a fixed list: which
// tokens exist is an operator's own files under secrets/github, read
// once at startup like the rest of that ladder, so this is handed the
// names rather than reading anything itself. Everything downstream
// treats them as ordinary capabilities, which is the whole point of
// giving each token an id (model.GitCredentialCapability) instead of a
// second, token-shaped picker: they can be defaulted deployment-wide or
// per repo, attached and detached per task, and reported on Settings'
// Capabilities tab, with no code that knows about tokens in particular.
//
// A task holding two of them still only pushes with one -- the git proxy
// picks a winner (model's own gitCredentialOverride) rather than
// refusing -- so the picker deliberately does not try to make them
// mutually exclusive.
func GitHubTokenCapabilities(names []string) []Capability {
	out := make([]Capability, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		out = append(out, Capability{
			ID:          model.GitCredentialCapability(name),
			Name:        GitHubTokenDisplayName(name),
			Description: githubtoken.Description(name),
		})
	}
	return out
}

// GitHubTokenDisplayName is what a named token is called on screen, in
// both listings that show one: the per-task picker's own row
// (GitHubTokenCapabilities) and Settings' Capabilities tab
// (capabilityStatuses).
func GitHubTokenDisplayName(name string) string { return "GitHub token: " + name }

// Config is what a Client needs to know about the deployment it fronts.
//
// TaskRepo and the label taxonomy are both gone: there is no task repo to
// file issues in any more, and no labels to derive anything from. What
// replaces them is Actor -- who a task created here is attributed to,
// which a GitHub issue used to answer with its own opening account.
type Config struct {
	// Actor is the principal the CLI or UI acts as. A task filed here
	// records it as both origin and approval, the same way a human who
	// could apply the trigger label used to land a task queued rather
	// than proposed.
	Actor model.Principal
	// DefaultTarget is the repo a task with no explicit one targets,
	// mirroring orchestrator.Config.DefaultTarget so a single-repo
	// deployment need not repeat itself on every task.
	DefaultTarget *model.RepoRef
	// TargetRepos restricts which repos a task's Repo (explicit or
	// defaulted) may name -- model.Config's own field of the same name,
	// mirrored here the way DefaultTarget already is. Empty means
	// unrestricted. CreateTask enforces it.
	TargetRepos  []string
	Capabilities []Capability
	// Secrets is set only when this UI runs on the same host as the
	// server whose secrets directory it names -- nil means it does not,
	// and the secrets pane and its API routes report themselves
	// unavailable rather than erroring on every call (bwsalmon/agents#357).
	// It is write/list only in the sense that matters: nothing in this
	// package ever calls Store.Resolve, so a value, once set, is never
	// readable back through here -- only Set, DeleteKey, DeleteSecret and
	// List (which reports names and key names, never values).
	Secrets *secrets.Store
	// Reboot, when set, is what the UI's "reboot host" button
	// (bwsalmon/agents#395) calls to reboot the machine this deployment's
	// daemon is running on. nil means handleRebootHost reports the action
	// unavailable rather than erroring on every call -- the same
	// nil-means-unavailable contract Secrets already gives the secrets
	// pane, and the right default for `grain demo`'s throwaway UI, which
	// has no real machine behind it worth rebooting.
	Reboot func(ctx context.Context) error
	// Credentials is the same GitHub credential ladder (secrets/github/
	// credentials.json, loaded once at startup, not hot-reloaded) the
	// git proxy resolves pushes against -- given here so Settings can
	// flag a targetRepos entry the ladder has no owner/repo, owner/*, or
	// * pattern covering. nil (`grain demo`'s throwaway UI, or any UI
	// not colocated with the proxy that built it) means Settings reports
	// no such gaps rather than erroring, the same nil-means-unavailable
	// contract Secrets and Reboot already give. Without this, the only
	// way to learn targetRepos and the credential ladder have drifted
	// apart is a confusing 500 "no credential configured" from the git
	// proxy on the next push (bwsalmon/agents#427).
	Credentials *gitproxy.CredentialSet
	// Upgrader is set only when this deployment was told where its own
	// git checkout and build/install/restart mechanics live
	// (bwsalmon/agents#396, cmd/grain/daemon.go's -upgrade-src-dir) --
	// nil means it wasn't, and the upgrade pane and its API routes
	// report themselves unavailable rather than erroring on every call,
	// the same as a nil Secrets above.
	Upgrader Upgrader
	// Logs is the set of named log sources a debugging page in the UI can
	// tail -- "daemon" for grain daemon's own journal, and, on a
	// deployment colocated with one, the git proxy's audit log
	// (bwsalmon/agents#444). Keyed by the name a caller passes to
	// GET /api/logs/{source}. nil or empty means no sources are
	// configured, and the debugging page reports itself unavailable
	// rather than a page with nothing on it, the same nil-means-
	// unavailable contract Secrets, Reboot and Upgrader above already
	// give their own panes.
	Logs map[string]LogSource
	// LiveTranscripts is where Client.AttemptTranscript looks for a
	// still-running attempt's own transcript-in-progress, before falling
	// back to whatever Store.RunTranscript has recorded (nothing, until
	// the attempt finishes) -- the seam a deployment with filesystem
	// access to a live run's own transcript file plugs in
	// (pkg/agent/claude.LiveTranscriptDir implements it), mirroring Logs'
	// own nil-means-unavailable contract: nil here just means every
	// attempt is read the way bwsalmon/agents#446 originally left it,
	// nothing to show until it finishes (bwsalmon/agents#467).
	LiveTranscripts LiveTranscript
	// Sandboxes, when set, is what GET /api/sandboxes calls to report
	// every live run's own sandbox status -- cmd/grain/daemon.go's
	// sandboxHealthAdapter over whichever of orchestrator.KonturSandboxes/
	// HostSandboxes this deployment actually runs (run()'s own doc
	// comment: exactly one of the two). nil means this deployment's UI
	// was not handed one (`grain demo`'s throwaway UI, or any UI not
	// colocated with the orchestrator that owns a real sandbox pool), and
	// the sandbox health pane reports itself unavailable rather than
	// erroring on every call, the same nil-means-unavailable contract
	// Logs above already gives the debug section it now sits alongside
	// (bwsalmon/agents#536).
	Sandboxes SandboxHealth
	// HostStats, when set, is what GET /api/sandboxes' own host section
	// calls to report this deployment's daemon's own CPU-load/memory
	// pressure -- pkg/sysstat.Read wrapped to return HostPressure, in a
	// real deployment. A sandbox that looks stuck is often really the
	// host it runs on being starved, which is a question about this
	// process's own machine rather than about any one sandbox
	// (SandboxHealth above), so it is reported separately. nil means no
	// reading is available, the same nil-means-unavailable contract every
	// other optional field here already gives (bwsalmon/agents#536).
	HostStats func() (HostPressure, error)
	// HostTop, when set, is what GET /api/host/top calls for a `top`
	// snapshot of the processes on that same machine -- pkg/hosttop.Read,
	// in a real deployment. It is the per-process half of HostStats
	// above: a load average says the machine is starved, and the next
	// question is always which process is doing it, which no aggregate
	// reading can answer. Given as a func rather than read here so this
	// package keeps shelling out to nothing itself, the same way
	// Logs' own LogSource keeps journalctl out of it. nil means no
	// snapshot is available (`grain demo`'s throwaway UI, a deployment
	// whose image has no procps) and the Top tab says so rather than
	// erroring on every poll, the same nil-means-unavailable contract
	// every other optional field here already gives.
	HostTop func(ctx context.Context, lines int) ([]string, error)
	// AutoMergeDegraded, when set, is polled by GET /api/config to report
	// whether this deployment's GitHub credential can read pull request
	// check runs at all -- orchestrator.ChecksUnavailable's own doc
	// comment. A deployment where it cannot (a fine-grained PAT, e.g.
	// terraform/gcp's own staging credential, rather than a GitHub
	// App installation token) still accepts Submit and sets AutoMerge,
	// but never actually merges: PR health can never resolve past
	// "unknown", which orchestrator's own reconcile loop declines to
	// act on. Before this field existed that fact lived only in server
	// logs, so Submit looked like it silently did nothing
	// (bwsalmon/agents#483). nil (`grain demo`'s throwaway UI, or any
	// deployment not wired to orchestrator's own package-level state)
	// means AutoMergeDegraded always reports false, the same
	// nil-means-unavailable-feature default Reboot and Upgrader give
	// their own panes above -- except here false is the "nothing to
	// warn about" answer rather than "the feature is off."
	AutoMergeDegraded func() bool
	// ReconcilerDown, when set, is polled by GET /api/config to report
	// whether this deployment's own reconcile loop has died -- true once
	// cmd/grain/daemon.go's runDaemon has returned an error (or panicked)
	// and given up entirely, as opposed to a one-time setup step still
	// being retried. Before this existed, that fact lived only in a
	// single server log line: the UI/API server stays up on its own
	// (bwsalmon/agents#550, "make sure ui with logs stays up even if the
	// daemon has failed"), so nothing dispatching or reconciling tasks
	// was otherwise visible from outside the process at all
	// (bwsalmon/agents#576). nil (`grain demo`'s throwaway UI, or any
	// deployment not wired to cmd/grain's own package-level state) means
	// ReconcilerDown always reports false, the same nil-means-
	// unavailable-feature default AutoMergeDegraded above gives.
	ReconcilerDown func() bool
	// RunningConfig, when set, reports the store-backed configuration the
	// daemon this UI runs inside actually has in effect right now -- what
	// it read at startup, plus every later change it was able to apply
	// without being restarted (cmd/grain/daemon.go's liveConfig). Settings
	// compares it against what is stored to answer which of the
	// restart-only settings have been saved but are not running yet
	// (Settings.PendingRestart), so a change that will sit unapplied
	// until someone restarts the daemon says so at the moment it is made
	// rather than looking, like every other setting on the pane, as
	// though it had taken effect.
	//
	// nil (`grain demo`'s throwaway UI, or any UI not colocated with a
	// daemon whose configuration it could speak for) means PendingRestart
	// is always empty -- the same nil-means-unavailable contract
	// ReconcilerDown and AutoMergeDegraded above give, and the honest
	// answer where there is no running configuration to compare with.
	RunningConfig func() model.Config
	// PullRequests, when set, is what POST /api/tasks/{id}/pull-request
	// calls to open (or find) a task's own pull request and read its
	// checks -- cmd/grain/daemon.go's pullRequestOpener over
	// orchestrator.OpenPullRequestForTask and that daemon's GitHub
	// client. It is how a dispatched run opens its pull request before it
	// exits, through the open_pull_request tool its own MCP server
	// exposes: the mcpserver process holds no GitHub credential of its
	// own (deliberately -- pkg/gitproxy's whole shape is that a run's
	// route to GitHub is grain's, never the agent's), so it asks the
	// daemon over this same REST API the CLI already speaks.
	//
	// nil means this deployment's UI was not handed one (`grain demo`'s
	// throwaway UI, or any UI not colocated with a daemon that has a
	// GitHub client), and the route answers 404 rather than erroring --
	// the same nil-means-unavailable contract Reboot and Sandboxes above
	// already give.
	PullRequests PullRequests
	// PullRequestComments, when set, is what Close uses to say on a pull
	// request itself that grain has stopped watching it -- see
	// Client.noteOrphanedPullRequests, and model.OrphanedPullRequestNote
	// for what is said and why it is said in two places at once.
	//
	// nil means this deployment's UI was not handed one, exactly as for
	// PullRequests above, and is not an error here: the note still lands
	// on the task, and says in so many words that the pull request itself
	// was not told. A close never fails over this.
	PullRequestComments PullRequestComments
	// SandboxRecreate, when set, is what
	// POST /api/tasks/{id}/sandbox/recreate calls to destroy a live run's
	// sandbox and build an empty one in its place -- cmd/grain/daemon.go's
	// sandboxRecreateAdapter over the orchestrator.SandboxRecreations its
	// dispatched runs register themselves in.
	//
	// It is how a dispatched run rescues itself from a sandbox it cannot
	// repair from inside, through the recreate_sandbox tool its own MCP
	// server exposes: everything else that run has runs *in* the sandbox,
	// so destroying and rebuilding one is necessarily somebody else's
	// call to make -- the same hop, and the same reasoning, as
	// PullRequests above.
	//
	// nil means this deployment's UI was not handed one (`grain demo`'s
	// throwaway UI, or any UI not colocated with the daemon that owns the
	// runs), and the route answers 404 rather than erroring -- the same
	// nil-means-unavailable contract PullRequests and Sandboxes above
	// already give.
	SandboxRecreate SandboxRecreator
	// Cycles, when set, is what GET /api/metrics' own "cycles" section
	// reads to report how long this daemon's RunCycle ticks have been
	// taking -- cmd/grain/daemon.go's cycleTimesAdapter over the
	// orchestrator.CycleTimes its reconcile loop writes into. It is the
	// one part of a metrics report that is not derived from rows,
	// because a tick leaves none (see pkg/metrics' Cycles), so unlike
	// every other number there it has to be handed in by the process
	// that produced it.
	//
	// nil means this deployment's UI was not handed one (`grain demo`'s
	// throwaway UI, or any UI not colocated with a reconcile loop whose
	// ticks it could speak for), and the section reports itself
	// unavailable rather than an empty distribution that would read as
	// "no ticks happened" -- the same nil-means-unavailable contract
	// Sandboxes and PullRequests above already give.
	Cycles CycleTimes
}

// CycleTimes is implemented by whatever can report the RunCycle ticks a
// daemon has recently run -- cmd/grain/daemon.go's own cycleTimesAdapter
// over orchestrator.CycleTimes, in a real deployment. Named here and
// filled there for the same reason SandboxHealth is: this package does
// not import pkg/orchestrator (see sandbox_health.go's own doc comment),
// so cmd/grain is the one place both types are ever in scope.
//
// It is stated in pkg/metrics' own terms rather than in a third set of
// types, unlike SandboxHealth: this is not a shape the API renders
// directly, it is an *input* to the same metrics.Compute call the rest of
// the report already comes from, and pkg/ui already imports pkg/metrics
// to make that call.
type CycleTimes interface {
	CycleTimes() metrics.CycleHistory
}

// LiveTranscript is implemented by whatever can read back a still-running
// run's own transcript-in-progress by run ID -- pkg/agent/claude's own
// LiveTranscriptDir, structurally, the same decoupling LogSource already
// gives pkg/systemlog: this package names the seam, a framework package
// fills it, and neither imports the other.
type LiveTranscript interface {
	// Tail returns runID's transcript-in-progress. ok is false if
	// nothing has been recorded for that run yet (or ever) -- a caller
	// should fall back to the store the same way it would if
	// LiveTranscripts were nil.
	Tail(runID string) (text string, ok bool, err error)
}

// Upgrader is the subset of *upgrade.Upgrader the UI needs -- an
// interface so a test can fake an upgrade without a real git checkout,
// docker daemon, or restart command behind it.
type Upgrader interface {
	Start(branch string) error
	Status() (upgrade.Status, error)
}

// DefaultActor is the principal a deployment gets without saying
// otherwise. A human ID rather than an automation one: the thing on the
// other end of this package is a person at a terminal or a browser, and
// model.LandsQueued's own rule turns on that distinction.
func DefaultActor(id string) model.Principal {
	if id == "" {
		id = "operator"
	}
	return model.Principal{Kind: model.PrincipalHuman, ID: id}
}
