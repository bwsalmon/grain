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
	// Slots is the concurrency pool dispatch.Cycle fills -- the same
	// comma-separated list -slots parses.
	Slots []string
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
}
