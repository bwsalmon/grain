package model

import (
	"strconv"
	"time"
)

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
	// MaxConcurrent is the size of the concurrency pool dispatch.Cycle
	// fills -- the same count -max-concurrent parses. bwsalmon/agents#461
	// replaced named slots (an operator-chosen list, each entry its own
	// sandbox directory or kontur VM name) with this plain count: SlotNames
	// turns it into the []string dispatch.Cycle and orchestrator.Deps
	// still take, since nothing below that layer needs to know a slot's
	// name was ever chosen rather than generated.
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
}

// SlotNames returns the n dispatch.Cycle slot identifiers a deployment
// configured for MaxConcurrent == n fills -- "1" through strconv.Itoa(n).
// A slot's name is otherwise meaningless (dispatch.Cycle only cares that
// each one is distinct), but orchestrator.HostSandboxes and
// orchestrator.KonturSandboxes turn it into a directory or VM name apiece,
// so callers wiring either one need a stable, generated set rather than
// picking their own the way -slots let an operator do before bwsalmon/
// agents#461.
func SlotNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = strconv.Itoa(i + 1)
	}
	return names
}
