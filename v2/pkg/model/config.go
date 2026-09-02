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
	// PollInterval is how often pkg/orchestrator's RunCycle runs.
	PollInterval time.Duration
	// MaxConcurrent is how many runs dispatch.Cycle lets be in flight at
	// once -- the same count -max-concurrent parses.
	//
	// bwsalmon/agents#461 replaced named slots (an operator-chosen list,
	// each entry its own sandbox directory or kontur VM name) with this
	// plain count, but kept generating identifiers from it, so a "slot"
	// still existed as the thing a sandbox was named after and reused
	// under. With a sandbox created and deleted per run, there is nothing
	// left for such an identifier to name: this is now read as a limit
	// and nothing else, by dispatch.Cycle counting live runs against it
	// and by nobody else at all.
	MaxConcurrent int
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
	// GeminiModel is the model the agent framework calls -- named for
	// the Gemini family agent/antigravity's own agy still runs, and kept
	// under this name because it is a persisted column (schema.go's
	// grain_config.gemini_model) that renaming would cost a migration
	// for nothing.
	GeminiModel string
	// MaxAgentTurns caps model/tool round trips per run; 0 leaves the
	// agent framework's own default in place.
	MaxAgentTurns int
	// GitHubHost is the GitHub API host -- overridable to point at a mock
	// for local testing.
	GitHubHost string
	// GitHubInsecureHTTP speaks plain HTTP to GitHubHost instead of
	// HTTPS -- mock servers only.
	GitHubInsecureHTTP bool
	// GCPProject is the GCP project the gcp-key/gemini-key capabilities
	// mint into; empty disables both.
	GCPProject string
	// GCPServiceAccountEmail is the narrow agent service account gcp-key
	// mints keys for.
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
	SandboxCPUs int
	// SandboxMemoryMB is SandboxCPUs' memory counterpart, in MiB.
	SandboxMemoryMB int
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
	// agents#612): false, the default, matches grain's original shape --
	// NewTaskOverlay.jsx's own checkbox starts unchecked, so a task files
	// as a proposal needing Approve unless someone checks it. true starts
	// it checked instead, so filing a task takes one fewer click on a
	// deployment that approves nearly everything anyway. Like
	// NewestFirst and ShowClosedByDefault, this only ever seeds the
	// form's *starting* state -- whoever is filing a task can still
	// uncheck it, and CreateTaskRequest.Approved is what actually decides
	// each task's own Approval, not this.
	ApprovedByDefault bool
	// AutoMergeByDefault is ApprovedByDefault's counterpart for
	// NewTaskOverlay.jsx's "Auto-merge once checks pass" checkbox
	// (bwsalmon/agents#612): false, the default, keeps a new task's
	// Task.AutoMerge off unless someone checks it. true starts it checked
	// instead. Same "starting state only" caveat as ApprovedByDefault --
	// CreateTaskRequest.AutoMerge, not this, is what a filed task actually
	// gets.
	AutoMergeByDefault bool
}

// AgentFramework's own vocabulary -- the two agent.Framework
// implementations v2/pkg/agent has today (v2/pkg/agent/antigravity,
// v2/pkg/agent/claude). Named here, not in either of those packages, so
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
