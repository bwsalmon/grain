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
	// GeminiModel is the Gemini model the agent framework calls.
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
	// `konturctl vm create` as `-cpus`/`-memory-mb` when creating a run's
	// sandbox VM -- an alternative, store-backed way to set what an
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
}
