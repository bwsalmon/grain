package model

import "time"

// Config is a deployment's tunable, non-secret configuration -- the
// knobs bwsalmon/agents#320 asked to move off the daemon's own flags and
// into the store, so a UI or a CLI can change them without a redeploy.
//
// What stays a flag, deliberately, is anything a deployment needs before
// it can even reach the database this names, or that names secret
// material rather than being one: -data-dir, -gemini-api-key-file and
// -kontur-ssh-key chief among them. Config is everything left over on
// cmd/grain's "daemon"
// subcommand's own flag set -- see daemon.go's own doc comment on how
// the two combine.
//
// GetConfig/PutConfig are its persistence, one row, keyed the same way
// grain_write and grain_schema are: there is exactly one of these per
// deployment. GetConfig returns a nil Config, with no error, for a fresh
// database with no row yet -- a caller seeding one for the first time
// (daemon.go's run, on a database it just opened) tells that apart from
// "the row says every field is a zero value" this way, where a bare
// zero-value Config could not.
type Config struct {
	// PollInterval is how often pkg/orchestrator's RunCycle runs. Read
	// back out of this row once per tick, so changing it retimes the
	// loop that is already running rather than the next one
	// (cmd/grain/daemon.go's liveConfig).
	PollInterval time.Duration
	// MaxWorkers is how many runs of ordinary work dispatch.Cycle lets be
	// in flight at once -- the same count -max-workers parses.
	//
	// bwsalmon/agents#461 replaced named slots (an operator-chosen list,
	// each entry its own sandbox directory or kontur VM name) with this
	// plain count, but kept generating identifiers from it, so a "slot"
	// still existed as the thing a sandbox was named after and reused
	// under. With a sandbox created and deleted per run, there is nothing
	// left for such an identifier to name: this is now read as a limit
	// and nothing else, by dispatch.Cycle counting live runs against it
	// and by nobody else at all.
	//
	// It was called MaxConcurrent, and was the whole limit, until
	// grain/task-63 split the merge queue's own repair runs out from
	// under it -- see MaxMergers below and Limits, which is the pair of
	// them as everything that enforces a limit reads them. Its column was
	// renamed with it, max_concurrent to max_workers, by
	// Store.ensureConfigWorkerMergerColumns.
	MaxWorkers int
	// MaxMergers is capacity on top of MaxWorkers that only the merge
	// queue's own fix tasks may reach (model.Limits' own doc comment has
	// why they are worth reserving, and exactly how the two numbers
	// combine: workers never exceed MaxWorkers, nothing exceeds
	// MaxWorkers+MaxMergers, and a merger may take a free worker slot
	// while the reverse never happens).
	//
	// 0 is a meaningful value rather than an unset one: mergers then
	// contend for MaxWorkers alongside everything else, exactly as every
	// deployment behaved before this field existed. DefaultConfig's 1 is
	// what a deployment that has never chosen gets instead -- one run of
	// headroom is enough to keep a queue head's repair from waiting out
	// whatever else is running, and cheap enough that a single-worker
	// deployment can afford it.
	MaxMergers int
	// AgentFramework selects which agent.Framework a run is meant to be
	// driven by -- AgentFrameworkAntigravity (agent/antigravity, the
	// Antigravity CLI's `agy` binary as a subprocess) or
	// AgentFrameworkClaude (agent/claude, the real `claude` CLI as a
	// subprocess). Empty reads back as AgentFrameworkAntigravity
	// (ui.UpdateSettings' own default).
	//
	// It is the deployment-wide default, not the last word: a task
	// carries its own Task.AgentFramework, and a non-empty one overrides
	// this for that task's dispatch alone. cmd/grain/daemon.go's
	// agentFrameworks re-reads this row on every dispatch, so a change
	// made here takes effect on the next run rather than at the next
	// restart.
	//
	// The value "gemini" is this field's own legacy spelling, from when
	// the default was a home-grown in-process Gemini API loop
	// (pkg/agent/gemini, removed when agent/antigravity replaced it).
	// NormalizeAgentFramework folds it into AgentFrameworkAntigravity so
	// a deployment upgraded mid-flight -- a stored row, a config file or
	// a -agent-framework flag still saying "gemini" -- keeps running
	// rather than failing validation on a word that no longer names
	// anything. TEXT rather than a constrained type,
	// enum-vocabulary-in-Go rather than in the schema (schema.go's own
	// doc comment on why), validated by ui.UpdateSettings instead.
	AgentFramework string
	// GeminiModel is the model agent/antigravity's own agy is asked for --
	// named for the Gemini family it still runs, and kept under this name
	// because it is a persisted column (schema.go's grain_config.
	// gemini_model) that renaming would cost a migration for nothing.
	//
	// Read when a run's framework is built, which is once per dispatch
	// (cmd/grain/daemon.go's dispatchConfig, alongside AgentFramework
	// above), so a model changed here is the one the next run calls.
	GeminiModel string
	// ClaudeModel is agent/claude's own counterpart to GeminiModel: the
	// model the real `claude` CLI is asked for, required the same way and
	// for the same reason (ui.UpdateSettings). Read by whichever
	// framework a run is actually dispatched onto, per dispatch
	// (cmd/grain/daemon.go's agentFrameworks/dispatchConfig); a
	// deployment that never runs agent/claude simply leaves this unread.
	ClaudeModel string
	// MaxAgentTurns caps model/tool round trips per run; 0 leaves the
	// agent framework's own default in place, which for both frameworks
	// is no cap at all (agent/claude's defaultMaxTurns has why). A run's
	// real ceiling is orchestrator.Config.MaxRunRuntime. Re-read by
	// orchestrator.RunCycle every cycle, the same as MaxWorkers, so a
	// changed cap reaches the next run dispatched.
	MaxAgentTurns int
	// GitHubHost is the GitHub API host -- overridable to point at a mock
	// for local testing.
	//
	// This, and GitHubInsecureHTTP below, are the only two settings here
	// that a running daemon cannot adopt: both are baked into the git
	// proxy's forwarder, the GitHub REST transport and the
	// github-sandbox capability provider at startup, each read
	// unsynchronised by requests already in flight. ui.Settings reports
	// them as restartRequired for exactly that reason, and reports a
	// change to one as pending until the daemon is restarted --
	// ui.restartOnlySettings is the list, cmd/grain/daemon.go's
	// liveConfig the other end of it.
	GitHubHost string
	// GitHubInsecureHTTP speaks plain HTTP to GitHubHost instead of
	// HTTPS -- mock servers only. Needs a restart, like GitHubHost above.
	GitHubInsecureHTTP bool
	// GCPProject is the GCP project the gcp-key/gemini-key capabilities
	// mint into; empty disables both. Changing it rebuilds the
	// model.CapabilityRegistry the next cycle resolves a task's grants
	// against (cmd/grain/daemon.go's liveConfig), so configuring a
	// project is what enables those two capabilities -- not the restart
	// that used to be needed after it.
	GCPProject string
	// GCPServiceAccountEmail is the narrow agent service account gcp-key
	// mints keys for. Applied with GCPProject above, and the same way.
	GCPServiceAccountEmail string
	// TargetRepos restricts which repos a task's Repo may name, mirroring
	// v1's terraform/gcp/variables.tf target_repos -- "the allowlist the
	// git proxy enforces, so a task naming anything else is parked with a
	// comment rather than dispatched." Empty means unrestricted, the same
	// "leave empty for a single-repo deployment" default that variable
	// documents. Enforced in (*ui.Client).CreateTask, the one place a
	// task's target repo is resolved now that a task has no directive
	// line for it to be parsed from (see orchestrator.ParseDirectives'
	// own doc comment).
	TargetRepos []string
	// NewestFirst switches the backlog's default order (bwsalmon/
	// agents#476): false, the default, keeps grain's original shape --
	// Store.OrderKeyForNewTask files a new task behind everything already
	// queued, so it is dispatched last, and the task list shows it at the
	// top regardless. true files a new task ahead of everything queued
	// instead, so it dispatches next, and the task list's default sort
	// flips to match -- top-to-bottom is dispatch order either way, this
	// only decides which end a new task joins.
	NewestFirst bool
	// SandboxCPUs and SandboxMemoryMB (bwsalmon/agents#534) are the
	// deployment-wide default VM shape orchestrator.KonturConfig passes
	// `konturctl vm create` as `-cpus`/`-memory-mb` when creating a
	// slot's VM -- an alternative, store-backed way to set what an
	// operator could already pass by hand as -kontur-create-arg=-cpus
	// -kontur-create-arg=<n>, surfaced in the settings UI instead of a
	// daemon flag. Zero, the default for both, leaves bwsalmon/kontur's
	// own `konturctl vm create` default in place (2 vCPUs, 2048 MiB --
	// third_party/kontur/internal/staticpod/spec.go's own Defaults) and
	// omits the corresponding flag entirely, rather than passing a
	// literal "-cpus 0" that VMSpec.Validate would refuse. Both are
	// meaningless under the default orchestrator.HostSandboxes backend
	// (local directories have no CPU/memory shape of their own) and
	// simply go unread there, the same way the kontur* daemon flags do.
	//
	// A change to either reaches the backend while the daemon runs
	// (orchestrator.KonturSandboxes.SetDefaultShape, called by
	// cmd/grain/daemon.go's liveConfig), so it applies to the next
	// sandbox built rather than to the next process. Sandboxes already
	// running keep the size they were created at -- a VM's shape is
	// decided once, when it is created.
	SandboxCPUs int
	// SandboxMemoryMB is SandboxCPUs' memory counterpart, in MiB.
	SandboxMemoryMB int
	// SandboxDiskGB is the third dimension of that same shape, in GiB:
	// how large a root disk `konturctl vm create` gives the VM, passed as
	// `-disk-size-gb`. Zero means the same thing the other two mean --
	// pass no flag, and take whatever a VM would get anyway, which for
	// disk is the size of the guest image the overlay is backed by
	// (scripts/kontur/README.md: kontur's own `guest-image` stage sizes
	// disk.img to the rootfs plus 20% headroom, so there is no fixed
	// number to name as its default the way there is for CPUs and
	// memory).
	//
	// Unlike CPUs and memory, this one needs something of the guest as
	// well as of the hypervisor: a bigger virtual disk is empty space
	// past the end of the filesystem packed into the image until
	// something grows that filesystem onto it, which
	// scripts/kontur/guest-setup.sh's grain-growfs unit does on each
	// boot.
	SandboxDiskGB int
	// ShowClosedByDefault is the deployment-wide default for whether a
	// task list starts out showing closed tasks (bwsalmon/agents#537):
	// false, the default, matches "hide closed tasks by default" --
	// TaskList.jsx's own "Show closed tasks" toggle starts unchecked, and
	// a viewer flips it on to see them. true starts that toggle checked
	// instead. Either way it is only ever the list's *starting* state --
	// like NewestFirst, nothing here forces it to stay that way, and
	// nothing in the daemon's own dispatch loop reads it.
	ShowClosedByDefault bool
	// ApprovedByDefault is the deployment-wide default for whether a new
	// task's "Queue immediately" checkbox starts checked (bwsalmon/
	// agents#612): true, the default (DefaultConfig), starts it checked,
	// so filing a task queues it for dispatch rather than parking it for
	// an Approve nobody was waiting to give -- a deployment that approves
	// nearly everything it files should not have to say so twice. false
	// starts it unchecked instead, grain's original shape, where a task
	// files as a proposal until someone approves it.
	//
	// Unlike NewestFirst and ShowClosedByDefault, whose default is also
	// their Go zero value, false here is a zero value that does not mean
	// "nobody has chosen". Nothing has to tell those two apart from the
	// field alone: every path that builds a Config with no stored row
	// behind it goes through DefaultConfig, and the store's own column
	// defaults say the same thing in SQL.
	//
	// Like NewestFirst and ShowClosedByDefault, this only ever seeds the
	// form's *starting* state -- whoever is filing a task can still
	// uncheck it, and CreateTaskRequest.Approved is what actually decides
	// each task's own Approval, not this.
	ApprovedByDefault bool
	// AutoMergeByDefault is ApprovedByDefault's counterpart for
	// NewTaskOverlay.jsx's "Auto-merge once checks pass" checkbox
	// (bwsalmon/agents#612): true, the default (DefaultConfig), starts a
	// new task's Task.AutoMerge on, so a run whose checks pass lands
	// without a human clicking Merge on it; false keeps it off unless
	// someone checks it. Same "starting state only" caveat as
	// ApprovedByDefault -- CreateTaskRequest.AutoMerge, not this, is what
	// a filed task actually gets.
	AutoMergeByDefault bool
	// EnvironmentName is what this deployment is called, for whoever is
	// looking at it: "staging", "dev", a hostname, anything. Empty, the
	// default, means an unnamed deployment and the UI shows nothing at
	// all -- the shape grain has always had, and the right one for an
	// operator running a single deployment who has nothing to tell it
	// apart from.
	//
	// It names nothing the daemon itself does: no dispatch, no sandbox,
	// no credential is chosen by it, and nothing outside the UI reads it.
	// It exists for the one failure a single-operator cluster invites --
	// approving, merging or rebooting on the deployment you thought was
	// the other one -- which is a question of what the screen says, not
	// of what the daemon enforces. A deployment that should refuse to
	// touch a repo wants TargetRepos above; this is a label.
	//
	// Free text, deliberately, rather than an enum of "staging"/"prod":
	// what environments a deployment sits among is the operator's own
	// vocabulary, and grain has no list of them to validate against.
	// ui.UpdateSettings bounds its length so it can be rendered somewhere
	// prominent without a paragraph pasted in taking the pane over, and
	// trims it so a stray space is not the difference between named and
	// unnamed.
	EnvironmentName string
	// DefaultCapabilities is the set of capability ids a new task is
	// filed holding, by id -- the deployment-wide answer to "what should
	// every task here start out able to do", and the reason gcp-key need
	// not be ticked by hand on every task that wants a service-account
	// key in its sandbox (grain/task-14, following task-10's picker
	// rows).
	//
	// It seeds a task's own Grants at creation (ui.CreateTask, which
	// records each as GrantByDefault) rather than being read again at
	// dispatch. That is the whole difference between this and v1's
	// unconditional per-dispatch mint: a seeded grant is on the task,
	// visible in its capability list, and detachable through the same
	// picker as any other, so whoever files a task can drop one they do
	// not want and an operator can take one off a task that is failing on
	// it. The cost of that choice is that turning an entry off here does
	// not disarm the tasks already holding it -- they keep the grant they
	// were filed with, which is the same thing that makes it modifiable
	// in the first place.
	//
	// Distinct, too, from docs/data-model.md's "Attaching capabilities to
	// repos and folders" -- those offers are floors, unioned in at
	// resolution and not droppable by the task. This is a seed.
	//
	// RepoConfig.DefaultCapabilities is the per-repo layer of this same
	// seed (grain/task-24), and composes with it exactly that way: more
	// ids in the set a new task starts with, unioned deployment-first in
	// ui.(*Client).defaultCapabilities. This layer is the one that
	// applies wherever a task points, including a task with no repo at
	// all; a repo can add to it and never subtract from it.
	//
	// An id here that this build's picker does not offer is skipped at
	// creation rather than failing it -- ui.UpdateSettings validates the
	// set on the way in, so that can only be a build that has since
	// retired a capability, and a stale settings row must not become a
	// deployment that can file no tasks at all.
	DefaultCapabilities []string
}

// DefaultConfig is the configuration a deployment that has never chosen
// one runs: every field's own zero value, except the three whose default
// is not it.
//
// Every path that builds a Config with nothing stored behind it starts
// here -- cmd/grain/daemon.go's flag seed for a fresh database,
// ui.UpdateSettings' first save, and the two API responses that have to
// describe a deployment whose grain_config row does not exist yet
// (ui.GetSettings, ui's handleConfig) -- so "on unless somebody turned it
// off" is one fact in one place rather than a literal true repeated at
// each of them. schema.go's grain_config DDL, and the store migrations
// that add these columns to a database predating them, carry the same
// defaults in SQL for the rows this constructor never touches.
func DefaultConfig() Config {
	return Config{
		ApprovedByDefault:  true,
		AutoMergeByDefault: true,
		MaxMergers:         DefaultMaxMergers,
	}
}

// DefaultMaxMergers is Config.MaxMergers for a deployment that has never
// chosen one -- named here rather than repeated at each of the three
// places that have to agree on it: DefaultConfig above, cmd/grain
// daemon's own -max-mergers flag default, and schema.go's grain_config
// DDL (which is also what Store.ensureConfigWorkerMergerColumns
// backfills a database predating the column with).
const DefaultMaxMergers = 1

// AgentFramework's own vocabulary -- the two agent.Framework
// implementations pkg/agent has today (pkg/agent/antigravity,
// pkg/agent/claude). Named here, not in either of those packages, so
// this file can reference them without pkg/model depending on pkg/agent
// or either of its implementations.
const (
	AgentFrameworkAntigravity = "antigravity"
	AgentFrameworkClaude      = "claude"

	// LegacyAgentFrameworkGemini is what AgentFrameworkAntigravity used
	// to be called, back when it named a home-grown in-process Gemini
	// API loop rather than the Antigravity CLI that replaced it. It is
	// not a framework anyone can select any more -- only a value
	// NormalizeAgentFramework still recognizes on the way in, so an
	// upgrade does not strand a deployment on a word with no
	// implementation behind it.
	LegacyAgentFrameworkGemini = "gemini"
)

// NormalizeAgentFrameworkName maps the legacy "gemini" spelling onto the
// framework it now names, and leaves everything else -- including "" --
// exactly as it found it. This is the form the per-task override wants
// (model.Task.AgentFramework), where "" is a meaningful value in its own
// right: "no override, use whatever the deployment is set to". Turning
// that into a framework name would silently pin every task ever created
// to one.
func NormalizeAgentFrameworkName(v string) string {
	if v == LegacyAgentFrameworkGemini {
		return AgentFrameworkAntigravity
	}
	return v
}

// NormalizeAgentFramework is NormalizeAgentFrameworkName plus the
// deployment-wide reading of "": there is no "no framework" for a
// deployment, so an unset Config.AgentFramework is AgentFrameworkAntigravity.
// Everything else is returned unchanged, including a value that names
// nothing -- the caller (ui.UpdateSettings, or daemon.go's own flag
// check) is the one to reject that.
//
// Every place that reads Config.AgentFramework goes through one of these
// two, so "which spellings are accepted, and what does empty mean here"
// is answered once rather than re-derived at each site.
func NormalizeAgentFramework(v string) string {
	if v == "" {
		return AgentFrameworkAntigravity
	}
	return NormalizeAgentFrameworkName(v)
}
