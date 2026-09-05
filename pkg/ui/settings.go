package ui

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwsalmon/grain/pkg/agent/antigravity"
	"github.com/bwsalmon/grain/pkg/kontur"
	"github.com/bwsalmon/grain/pkg/model"
)

// Settings is the JSON shape of a deployment's store-backed
// configuration (model.Config, bwsalmon/agents#320) -- the knobs
// cmd/grain's "daemon" subcommand reads out of grain_config instead of
// only from its own flags (daemon.go's loadConfig). This package is what
// lets a UI or a CLI actually be the thing that changes them, the same
// "the store is the record" shape pkg/ui already gives tasks.
//
// Changing one takes effect on the running deployment, without a
// restart, for everything but the two settings RestartRequired names:
// the daemon re-reads this row once per reconcile tick and applies what
// changed (cmd/grain/daemon.go's liveConfig). RestartRequired and
// PendingRestart below are how the exceptions say so, at the field, and
// how a change to one that is saved but not yet running is reported as
// exactly that rather than silently looking applied.
//
// Configured is false when nothing has written a row yet -- before any
// daemon has ever started against this store -- so a caller can tell
// that apart from "every field happens to be a zero value" the same way
// Store.GetConfig's own nil does.
type Settings struct {
	Configured bool `json:"configured"`
	// PollInterval is a Go duration string ("30s"), the same syntax
	// -poll-interval itself parses.
	PollInterval string `json:"pollInterval"`
	// MaxWorkers and MaxMergers are model.Config's own fields of the same
	// name (grain/task-63): how many runs of ordinary work may be in
	// flight at once, and how much further capacity only the merge
	// queue's own fix tasks may reach. MaxWorkers was called
	// maxConcurrent, and was the whole limit, until the two were split --
	// see model.Limits for the rule they make together, in particular why
	// a merger may take a worker's free slot and never the reverse.
	MaxWorkers  int    `json:"maxWorkers"`
	MaxMergers  int    `json:"maxMergers"`
	GeminiModel string `json:"geminiModel"`
	// GeminiEffort is the reasoning effort agy is asked for beside
	// GeminiModel -- model.Config.GeminiEffort, one of GeminiEfforts
	// below. It is a field of its own rather than part of the model name
	// because agy takes it as a flag of its own (--effort), and because
	// the two are chosen separately: the same model runs shallow or deep.
	GeminiEffort string `json:"geminiEffort"`
	// GeminiEfforts is the vocabulary GeminiEffort is saved from --
	// agent/antigravity.Efforts, agy's own low/medium/high -- reported
	// here so a settings pane can offer the choices instead of asking
	// somebody to remember three words a save would reject.
	//
	// Which of them a given model actually offers is that model's own
	// business and only agy knows it (Gemini 3.1 Pro has no medium), so
	// this is the widest list, not a promise about the model named beside
	// it.
	GeminiEfforts          []string `json:"geminiEfforts"`
	ClaudeModel            string   `json:"claudeModel"`
	CodexModel             string   `json:"codexModel"`
	MaxAgentTurns          int      `json:"maxAgentTurns"`
	GitHubHost             string   `json:"githubHost"`
	GitHubInsecureHTTP     bool     `json:"githubInsecureHttp"`
	GCPProject             string   `json:"gcpProject"`
	GCPServiceAccountEmail string   `json:"gcpServiceAccountEmail"`
	// TargetRepos restricts which repos a task's Repo may name -- empty
	// means unrestricted. model.Config's own field of the same name.
	TargetRepos []string `json:"targetRepos"`
	// TargetReposMissingCredentials is every TargetRepos entry that
	// Config.Credentials -- the same ladder the git proxy resolves
	// pushes against -- has no exact, owner/*, or * pattern covering.
	// A task targeting one of these is dispatched (it passed
	// targetAllowed) but every push its sandbox makes fails at the git
	// proxy with a 500 "no credential configured," so this is the
	// widened-targetRepos/stale-credentials.json drift
	// (bwsalmon/agents#427) surfaced at the moment it's introduced
	// rather than only discoverable later as that proxy failure. Always
	// empty when Config.Credentials is nil (nothing to check against),
	// which reads from JSON exactly like "no gaps found" -- there is
	// nothing actionable to tell those two cases apart on.
	TargetReposMissingCredentials []string `json:"targetReposMissingCredentials,omitempty"`
	// NewestFirst is model.Config's own field of the same name
	// (bwsalmon/agents#476): false, the default, files a new task at the
	// back of the dispatch queue, which is the bottom of the task list;
	// true files it at the front instead, which is the top of the same
	// list -- the list being dispatch order, not a second ordering of its
	// own (ui.Client.ListTasks).
	NewestFirst bool `json:"newestFirst"`
	// SandboxCPUs, SandboxMemoryMB and SandboxDiskGB
	// (bwsalmon/agents#534, grain/task-41) are the deployment-wide
	// default shape a kontur-managed sandbox VM is created with --
	// model.Config's own fields of the same name. Zero (the default for
	// all three) means "use grain's own default shape" (the three
	// *Default fields below) rather than a deliberately tiny VM.
	// Meaningless, and simply unused,
	// under a deployment running the default local-directory sandboxing
	// (no -kontur-sandboxes); GetSettings still reports whatever is
	// stored either way, the same as every other kontur* setting here.
	SandboxCPUs     int `json:"sandboxCpus"`
	SandboxMemoryMB int `json:"sandboxMemoryMb"`
	SandboxDiskGB   int `json:"sandboxDiskGb"`
	// SandboxCPUsDefault, SandboxMemoryMBDefault and SandboxDiskGBDefault
	// are grain's own default VM shape (kontur.DefaultCPUs/
	// DefaultMemoryMB/DefaultDiskGB) -- the shape actually in effect
	// whenever the stored value above is 0, surfaced so a caller can show
	// that real current shape instead of a bare, misleadingly literal 0
	// (bwsalmon/agents#610). Constant, never read from or written to the
	// store.
	//
	// Disk gained one here with the rest: it used to have no default to
	// name, because an unset disk was however large the guest image a
	// deployment built happened to be. grain names all three sizes now
	// and passes all three on every create
	// (orchestrator.KonturConfig.createArgs), so an unset disk has a
	// number to show like the other two.
	SandboxCPUsDefault     int `json:"sandboxCpusDefault"`
	SandboxMemoryMBDefault int `json:"sandboxMemoryMbDefault"`
	SandboxDiskGBDefault   int `json:"sandboxDiskGbDefault"`
	// ShowClosedByDefault is model.Config's own field of the same name
	// (bwsalmon/agents#537): the deployment-wide default for whether a
	// task list's own "Show closed tasks" toggle starts checked. Also
	// mirrored onto GET /api/config (handleConfig, tasks.go) so a list
	// has it to seed that toggle with before Settings has ever been
	// opened this session.
	ShowClosedByDefault bool `json:"showClosedByDefault"`
	// EnvironmentName is model.Config's own field of the same name: what
	// this deployment is called on screen ("staging", "dev", whatever an
	// operator running more than one of these needs to tell them apart).
	// Empty is an unnamed deployment, and the UI shows nothing for it.
	// Also mirrored onto GET /api/config (configResponse, tasks.go) the
	// same way ShowClosedByDefault is, since the frontend needs it on
	// first paint rather than only once Settings has been opened.
	//
	// Deliberately not omitempty, like DefaultCapabilities and
	// PendingRestart above: the frontend merges an update response over
	// the settings it already has, so a name cleared back to nothing has
	// to arrive as present-and-empty rather than as an absent key leaving
	// the old one on screen.
	EnvironmentName string `json:"environmentName"`
	// TimeZone is model.Config's own field of the same name: the IANA
	// zone this deployment keeps its wall clock in. Never reported empty
	// -- a row that stores nothing reads back as model.DefaultTimeZone
	// (model.TimeZoneOrDefault) -- so a pane can show the clock actually
	// in effect rather than a blank field meaning "some default
	// somewhere". Also mirrored onto GET /api/config, which is where the
	// frontend reads it for formatting every timestamp it prints.
	TimeZone string `json:"timeZone"`
	// PromptExtension is model.Config's own field of the same name
	// (grain/task-114): the standing instructions every run dispatched
	// here is given on top of the prompt grain builds for it. Empty adds
	// nothing. Deployment-wide only -- a repo adds its own on the repos
	// pane (RepoDefaults.PromptExtension) and a task overrides both on
	// the new-task form, neither of which is folded in here, for the same
	// reason DefaultCapabilities does not fold in the per-repo layer: a
	// field that mixed the two would describe a deployment-wide setting
	// that only some runs actually get.
	//
	// Deliberately not omitempty, like EnvironmentName above and for the
	// same reason: the frontend merges an update response over the
	// settings it already has, so text cleared back to nothing has to
	// arrive as present-and-empty rather than as an absent key leaving
	// the old value on screen.
	PromptExtension string `json:"promptExtension"`
	// Capabilities is every capability grain ships a provider for, with
	// this deployment's own readiness computed against it -- capability_
	// status.go's own CapabilityStatus, bwsalmon/agents#611. Always
	// populated, even before Settings has ever been saved (GetSettings),
	// since self-debug/self-repair/bootstrap-playbooks need no
	// configuration to be ready and an operator setting up a fresh
	// deployment still benefits from seeing that
	// gcp-key/gemini-key/github-sandbox are not yet.
	Capabilities []CapabilityStatus `json:"capabilities"`
	// DefaultCapabilities is model.Config's own field of the same name:
	// the capability ids every task filed on this deployment starts out
	// holding, unless whoever files it says otherwise. Reported as
	// stored, including an id this build no longer offers -- CreateTask
	// skips such an entry rather than failing (that field's own doc
	// comment), and an operator can only clear one they can see.
	//
	// Each entry is also flagged on the Capabilities list above
	// (CapabilityStatus.Default), so the pane that says whether a
	// capability is ready and grantable says in the same view whether
	// every new task is being filed with it -- "not ready" matters a
	// great deal more for a capability every task holds than for one
	// nobody has ticked.
	//
	// Deployment-wide only. A repo can default capabilities of its own on
	// top of these (model.RepoConfig.DefaultCapabilities, edited on the
	// repos pane), which is reported here as the repos each capability is
	// defaulted on (CapabilityStatus.DefaultRepos) rather than folded
	// into this list -- a set that mixed the two would describe a
	// deployment-wide default that only some tasks actually get.
	//
	// Deliberately not omitempty, for the same reason PendingRestart is
	// not: the frontend merges an update response over the settings it
	// already has, so a set cleared back to nothing has to arrive as
	// present-and-null rather than as an absent key leaving the old one
	// on screen.
	DefaultCapabilities []string `json:"defaultCapabilities"`
	// ApprovedByDefault and AutoMergeByDefault are model.Config's own
	// fields of the same name (bwsalmon/agents#612): deployment-wide
	// defaults for whether a new task's "Queue immediately" and
	// "Auto-merge once checks pass" checkboxes start checked in
	// NewTaskOverlay.jsx. Also mirrored onto GET /api/config
	// (configResponse, tasks.go) the same way ShowClosedByDefault is, so
	// that form has them to seed its checkboxes with before Settings has
	// ever been opened this session.
	//
	// Both are true on a deployment that has stored no configuration at
	// all, not false: unlike ShowClosedByDefault, off is not what "never
	// chosen" means here (model.DefaultConfig), and a pane describing an
	// unconfigured deployment should say what filing a task there would
	// actually do.
	ApprovedByDefault  bool `json:"approvedByDefault"`
	AutoMergeByDefault bool `json:"autoMergeByDefault"`
	// AgentFramework is model.Config's own field of the same name
	// (bwsalmon/agents#609): one of model.AgentFrameworks() --
	// "antigravity", "claude" or "codex" -- which agent.Framework
	// implementation a run is meant to be driven by.
	// Never empty coming out of here -- GetSettings/UpdateSettings both
	// default it to "antigravity" the same way Config.AgentFramework's
	// own doc comment says an empty stored value reads back. The legacy
	// "gemini" spelling is accepted on the way in and normalized to
	// "antigravity"; it is never written back out. It is the
	// deployment-wide default only: a task's own agentFramework
	// overrides it for that task's dispatch, and the credentials below
	// are what whichever one runs actually needs.
	AgentFramework string `json:"agentFramework"`
	// AgentKeysEnabled mirrors secretsResponse's own Enabled: whether
	// this UI has a secrets store to write an agent credential into at
	// all. False (a UI with no colocated store) is what tells the pane
	// to explain that rather than offer two fields whose every use
	// would 404.
	AgentKeysEnabled bool `json:"agentKeysEnabled"`
	// These report whether each framework's own credential is actually
	// in this deployment's secrets database -- agent_keys.go's own
	// agentKeysSet, mirrored onto Settings so the pane that picks a
	// framework can say, in the same view, whether the one being picked
	// can run at all. Presence only, never the value; all false for a UI
	// with no secrets store (Config.Secrets nil), the same
	// nil-means-unavailable reading every other check built on it takes.
	GeminiAPIKeySet     bool `json:"geminiApiKeySet"`
	ClaudeOAuthTokenSet bool `json:"claudeOAuthTokenSet"`
	OpenAIAPIKeySet     bool `json:"openaiApiKeySet"`
	// RestartRequired names every setting on this pane that a running
	// daemon cannot adopt on its own, by the JSON field it is called
	// above -- restartOnlySettings' own list. Constant, reported even on
	// a deployment that has never saved settings at all, since the point
	// of it is to annotate a field *before* anyone changes it: a pane
	// that only said so afterwards would be telling an operator about a
	// restart they have already earned.
	//
	// Everything not named here takes effect without a restart, which is
	// grain's default and the reason this list is worth reporting at all
	// (cmd/grain/daemon.go's liveConfig has the full accounting of what
	// applies each of the rest, and when).
	RestartRequired []string `json:"restartRequired"`
	// PendingRestart is the subset of RestartRequired whose stored value
	// differs from what the daemon is actually running with -- saved, but
	// not in effect until someone restarts it. Always empty on a UI with
	// no Config.RunningConfig to compare against (that field's own doc
	// comment), which is the same answer as "nothing is pending" because
	// there is nothing actionable to tell those two apart on.
	//
	// Deliberately not omitempty: the frontend merges an update response
	// over the settings it already has, so this has to be present-and-
	// null when a change is undone rather than absent and leaving a stale
	// warning on screen.
	PendingRestart []string `json:"pendingRestart"`
}

// maxEnvironmentNameLen bounds Settings.EnvironmentName, in runes. The
// name is rendered as a badge in the sidebar and appended to the
// browser tab's title, neither of which has room for prose: this is
// generously above every real answer ("staging", "dev", "bwsalmon-prod")
// and far below a paste that would take the chrome over.
const maxEnvironmentNameLen = 32

// restartOnlySetting is one setting a running daemon cannot pick up on
// its own, and how to tell whether a stored value for it differs from
// the one actually in effect.
type restartOnlySetting struct {
	// Key is the Settings JSON field this names, which is also the key
	// the frontend annotates its own input with.
	Key string
	// Differs answers whether stored and running disagree about this one
	// setting. A func per setting rather than one reflect-based
	// comparison of the whole model.Config: the point is to name the
	// two fields that cannot be applied live, and a comparison that
	// derived that list from the type would silently start reporting
	// every field anyone adds later.
	Differs func(stored, running model.Config) bool
}

// restartOnlySettings is every setting the Settings pane offers that a
// running daemon genuinely cannot adopt without being restarted.
//
// Both entries are the GitHub host override: it is baked into the git
// proxy's forwarder,
// the GitHub REST transport, and the github-sandbox capability provider
// when the daemon starts, each of which reads it unsynchronised from
// whatever request is already in flight -- so swapping one under a live
// deployment would be a data race rather than a setting change. They are
// also, not coincidentally, the two settings a real deployment never
// touches: an override that exists to point a local test at a mock
// GitHub (cmd/grain/daemon.go's -github-host/-github-insecure-http).
//
// Everything else on the pane is applied while the daemon runs -- see
// cmd/grain/daemon.go's liveConfig for which piece picks up which
// setting, and when. Adding a setting to this list is what makes the UI
// annotate it, so this and that are the two ends of one contract:
// nothing may be applied live and listed here, and nothing may be listed
// nowhere and left needing a restart in silence.
//
// The second half of that is what settings_restart_drift_test.go holds
// this list to: every UpdateSettingsRequest field has to be named here or
// named in that file's own settingsAppliedLive list, along with the piece
// that applies it, so a setting cannot be added without somebody deciding
// which of the two it is.
var restartOnlySettings = []restartOnlySetting{
	{
		Key:     "githubHost",
		Differs: func(stored, running model.Config) bool { return stored.GitHubHost != running.GitHubHost },
	},
	{
		Key: "githubInsecureHttp",
		Differs: func(stored, running model.Config) bool {
			return stored.GitHubInsecureHTTP != running.GitHubInsecureHTTP
		},
	},
}

// restartRequiredKeys is restartOnlySettings' own key list -- Settings.
// RestartRequired, built fresh each time so a caller cannot alias the
// package's own slice.
func restartRequiredKeys() []string {
	keys := make([]string, 0, len(restartOnlySettings))
	for _, s := range restartOnlySettings {
		keys = append(keys, s.Key)
	}
	return keys
}

// pendingRestart is Settings.PendingRestart's own computation: every
// restart-only setting whose stored value is not the one this
// deployment's daemon is running with. nil -- no comparison available,
// or nothing differing -- reads back as "nothing pending" either way.
func (c *Client) pendingRestart(stored model.Config) []string {
	if c.Config.RunningConfig == nil {
		return nil
	}
	running := c.Config.RunningConfig()
	var pending []string
	for _, s := range restartOnlySettings {
		if s.Differs(stored, running) {
			pending = append(pending, s.Key)
		}
	}
	return pending
}

// settingsFrom is cfg, plus every repo that adds defaults of its own,
// as the wire shape this pane reads. repoConfigs is passed in rather
// than read here so this stays a pure projection of what its two callers
// have already loaded -- the same reason it takes cfg rather than
// re-reading the config row.
func (c *Client) settingsFrom(cfg model.Config, repoConfigs []model.RepoConfig) Settings {
	agentFramework := model.NormalizeAgentFramework(cfg.AgentFramework)
	geminiKeySet, claudeTokenSet, openaiKeySet := c.agentKeysSet()
	return Settings{
		Configured:                    true,
		PollInterval:                  cfg.PollInterval.String(),
		MaxWorkers:                    cfg.MaxWorkers,
		MaxMergers:                    cfg.MaxMergers,
		GeminiModel:                   cfg.GeminiModel,
		GeminiEffort:                  cfg.GeminiEffort,
		GeminiEfforts:                 antigravity.Efforts(),
		ClaudeModel:                   cfg.ClaudeModel,
		CodexModel:                    cfg.CodexModel,
		MaxAgentTurns:                 cfg.MaxAgentTurns,
		GitHubHost:                    cfg.GitHubHost,
		GitHubInsecureHTTP:            cfg.GitHubInsecureHTTP,
		GCPProject:                    cfg.GCPProject,
		GCPServiceAccountEmail:        cfg.GCPServiceAccountEmail,
		TargetRepos:                   cfg.TargetRepos,
		TargetReposMissingCredentials: c.targetReposMissingCredentials(cfg.TargetRepos),
		NewestFirst:                   cfg.NewestFirst,
		SandboxCPUs:                   cfg.SandboxCPUs,
		SandboxMemoryMB:               cfg.SandboxMemoryMB,
		SandboxDiskGB:                 cfg.SandboxDiskGB,
		SandboxCPUsDefault:            kontur.DefaultCPUs,
		SandboxMemoryMBDefault:        kontur.DefaultMemoryMB,
		SandboxDiskGBDefault:          kontur.DefaultDiskGB,
		ShowClosedByDefault:           cfg.ShowClosedByDefault,
		EnvironmentName:               cfg.EnvironmentName,
		TimeZone:                      model.TimeZoneOrDefault(cfg.TimeZone),
		PromptExtension:               cfg.PromptExtension,
		Capabilities:                  c.capabilityStatuses(cfg, repoConfigs),
		DefaultCapabilities:           cfg.DefaultCapabilities,
		ApprovedByDefault:             cfg.ApprovedByDefault,
		AutoMergeByDefault:            cfg.AutoMergeByDefault,
		AgentFramework:                agentFramework,
		AgentKeysEnabled:              c.Config.Secrets != nil,
		GeminiAPIKeySet:               geminiKeySet,
		ClaudeOAuthTokenSet:           claudeTokenSet,
		OpenAIAPIKeySet:               openaiKeySet,
		RestartRequired:               restartRequiredKeys(),
		PendingRestart:                c.pendingRestart(cfg),
	}
}

// targetReposMissingCredentials is TargetReposMissingCredentials's own
// computation: every entry of targetRepos that c.Config.Credentials.Select
// has no ladder entry for. A malformed entry can't reach here --
// UpdateSettings already rejected it with model.ParseRepo before it was
// ever stored -- so ParseRepo failing is silently skipped rather than
// treated as a gap.
func (c *Client) targetReposMissingCredentials(targetRepos []string) []string {
	if c.Config.Credentials == nil || len(targetRepos) == 0 {
		return nil
	}
	var missing []string
	for _, r := range targetRepos {
		repo, err := model.ParseRepo(r)
		if err != nil {
			continue
		}
		if _, ok := c.Config.Credentials.Select(repo.Owner, repo.Name); !ok {
			missing = append(missing, r)
		}
	}
	return missing
}

// GetSettings reads the deployment's stored configuration. A zero
// Settings with Configured false, and no error, means nothing has
// written one yet.
func (c *Client) GetSettings(ctx context.Context) (Settings, error) {
	cfg, err := c.Store.GetConfig(ctx)
	if err != nil {
		return Settings{}, err
	}
	// Read in both branches below: a repo can be given defaults of its
	// own before this deployment has ever saved a settings row (nothing
	// in SetRepoDefaultCapabilities requires one), and a Capabilities tab
	// that hid those until someone pressed Save on an unrelated pane
	// would be describing a deployment that does not exist.
	repoConfigs, err := c.Store.ListRepoConfigs(ctx)
	if err != nil {
		return Settings{}, err
	}
	if cfg == nil {
		// Key presence is reported here too, not only once something has
		// been saved: pasting a credential in is exactly what an
		// operator does on a deployment that has never had its settings
		// saved at all.
		geminiKeySet, claudeTokenSet, openaiKeySet := c.agentKeysSet()
		// The two task defaults are reported as model.DefaultConfig has
		// them rather than as the zero values around them: what a pane
		// showing an unconfigured deployment should check those two boxes
		// as is what filing a task there would actually do, and with no
		// row yet that is the built-in default rather than off.
		def := model.DefaultConfig()
		return Settings{
			SandboxCPUsDefault:     kontur.DefaultCPUs,
			SandboxMemoryMBDefault: kontur.DefaultMemoryMB,
			SandboxDiskGBDefault:   kontur.DefaultDiskGB,
			Capabilities:           c.capabilityStatuses(model.Config{}, repoConfigs),
			AgentKeysEnabled:       c.Config.Secrets != nil,
			GeminiAPIKeySet:        geminiKeySet,
			ClaudeOAuthTokenSet:    claudeTokenSet,
			OpenAIAPIKeySet:        openaiKeySet,
			ApprovedByDefault:      def.ApprovedByDefault,
			AutoMergeByDefault:     def.AutoMergeByDefault,
			// Same reading as the two above: what a pane showing an
			// unconfigured deployment should say the clock is, is the
			// clock a firing would actually be timed against.
			TimeZone: def.TimeZone,
			// Reported before anything has been saved for the same
			// reason it is reported at all: the annotation belongs on
			// the field from the first time it is looked at. Nothing can
			// be pending yet -- there is no stored value to have
			// diverged from what is running.
			RestartRequired: restartRequiredKeys(),
		}, nil
	}
	return c.settingsFrom(*cfg, repoConfigs), nil
}

// UpdateSettingsRequest is Settings' editable fields -- nil means "leave
// this one alone", the same convention UpdateTaskRequest uses. Applying
// one is a read-modify-write against whatever is already stored (or the
// zero model.Config, the first time), inside no transaction of its own:
// Store.PutConfig replaces the single grain_config row wholesale, so two
// requests racing each other is the same "last write wins on this field
// set" grain_write already accepts for every other mutation -- and
// bwsalmon/agents#320 explicitly did not ask for anything more graceful
// than that yet.
type UpdateSettingsRequest struct {
	PollInterval *string `json:"pollInterval"`
	MaxWorkers   *int    `json:"maxWorkers"`
	MaxMergers   *int    `json:"maxMergers"`
	GeminiModel  *string `json:"geminiModel"`
	// GeminiEffort is validated against agent/antigravity.Efforts rather
	// than merely stored: a value agy does not know fails the run it is
	// dispatched into before that run starts ("--model gemini-3.1-pro
	// requires --effort (available: low, high)"), and a settings pane is
	// where that can still be said to somebody who can fix it. Empty is
	// refused for the same reason geminiModel's own empty is -- there is
	// no "unset it" here, only "leave the field out of the request".
	GeminiEffort           *string   `json:"geminiEffort"`
	ClaudeModel            *string   `json:"claudeModel"`
	CodexModel             *string   `json:"codexModel"`
	MaxAgentTurns          *int      `json:"maxAgentTurns"`
	GitHubHost             *string   `json:"githubHost"`
	GitHubInsecureHTTP     *bool     `json:"githubInsecureHttp"`
	GCPProject             *string   `json:"gcpProject"`
	GCPServiceAccountEmail *string   `json:"gcpServiceAccountEmail"`
	TargetRepos            *[]string `json:"targetRepos"`
	NewestFirst            *bool     `json:"newestFirst"`
	SandboxCPUs            *int      `json:"sandboxCpus"`
	SandboxMemoryMB        *int      `json:"sandboxMemoryMb"`
	SandboxDiskGB          *int      `json:"sandboxDiskGb"`
	ShowClosedByDefault    *bool     `json:"showClosedByDefault"`
	EnvironmentName        *string   `json:"environmentName"`
	// TimeZone is an IANA zone name ("America/Los_Angeles"), validated
	// against the zone database this build carries (model.ValidTimeZone)
	// rather than merely stored: a name nothing resolves would leave the
	// deployment quietly running on some fallback clock while the pane
	// showed the typo. Empty is accepted and means model.DefaultTimeZone
	// -- "put it back to the default" is a thing a cleared field should
	// be able to say, unlike geminiModel's own empty.
	TimeZone           *string `json:"timeZone"`
	ApprovedByDefault  *bool   `json:"approvedByDefault"`
	AutoMergeByDefault *bool   `json:"autoMergeByDefault"`
	AgentFramework     *string `json:"agentFramework"`
	// PromptExtension replaces this deployment's standing instructions
	// wholesale -- there is one block of text, so a present value is
	// exactly what every run will be told from now on, and an empty one
	// turns the feature back off. Trimmed on the way in, so a box someone
	// cleared to a stray newline stores as the nothing it looks like.
	PromptExtension *string `json:"promptExtension"`
	// DefaultCapabilities replaces the whole default set, the same way
	// TargetRepos above replaces the whole allowlist: a present list is
	// exactly the ids every new task will be filed with, and an empty one
	// turns the feature off. Every id must have a row in
	// OfferedCapabilities -- defaulting a capability no task could be
	// granted by hand would be a setting that fails silently at every
	// filing.
	DefaultCapabilities *[]string `json:"defaultCapabilities"`
}

// UpdateSettings applies req on top of whatever is currently stored (the
// zero model.Config if nothing is yet) and writes the result back
// wholesale.
//
// The first time settings are ever saved, PollInterval, MaxWorkers,
// GeminiModel, ClaudeModel and GitHubHost are required: leaving one of
// them out would otherwise write a zero value that reads back later as a
// deliberate setting rather than as "never configured" -- Configured
// already tells a caller that much on the way in, so writing a config
// that could not be told apart from one somebody actually chose is worse
// than asking for the field up front. MaxMergers, MaxAgentTurns,
// GitHubInsecureHTTP, GCPProject, GCPServiceAccountEmail, TargetRepos
// and EnvironmentName have real, meaningful zero values ("mergers take
// worker capacity", the framework's own default, HTTPS, "no GCP
// capability configured", "unrestricted", and "this deployment has no
// name" respectively -- daemon.go's own flag defaults), so nothing here
// demands them. A first save that never mentions MaxMergers stores
// model.DefaultConfig's own value for it rather than that zero, the same
// as every other field whose default is not its zero value.
func (c *Client) UpdateSettings(ctx context.Context, req UpdateSettingsRequest) (Settings, error) {
	current, err := c.Store.GetConfig(ctx)
	if err != nil {
		return Settings{}, err
	}
	// model.DefaultConfig, not a zero model.Config, is what a first save
	// applies req on top of: PutConfig binds every column, so a setting
	// whose default is not its zero value (ApprovedByDefault and
	// AutoMergeByDefault, both on) would otherwise be written off by a
	// request that never mentioned it.
	cfg := model.DefaultConfig()
	firstTime := current == nil
	if current != nil {
		cfg = *current
	}

	if req.PollInterval != nil {
		d, err := time.ParseDuration(*req.PollInterval)
		if err != nil {
			return Settings{}, validationErrorf("pollInterval: %v", err)
		}
		if d <= 0 {
			return Settings{}, validationErrorf("pollInterval must be positive")
		}
		cfg.PollInterval = d
	}
	if req.MaxWorkers != nil {
		if *req.MaxWorkers < 1 {
			return Settings{}, validationErrorf("maxWorkers must be at least 1")
		}
		cfg.MaxWorkers = *req.MaxWorkers
	}
	if req.MaxMergers != nil {
		// 0 is allowed where maxWorkers' own 0 is not, and means
		// something rather than nothing: mergers then contend for worker
		// capacity alongside every other task, which is what a deployment
		// filed before this setting existed was doing
		// (model.Config.MaxMergers).
		if *req.MaxMergers < 0 {
			return Settings{}, validationErrorf("maxMergers cannot be negative")
		}
		cfg.MaxMergers = *req.MaxMergers
	}
	if req.GeminiModel != nil {
		if strings.TrimSpace(*req.GeminiModel) == "" {
			return Settings{}, validationErrorf("geminiModel cannot be empty")
		}
		cfg.GeminiModel = *req.GeminiModel
	}
	if req.GeminiEffort != nil {
		effort := strings.TrimSpace(*req.GeminiEffort)
		if !slices.Contains(antigravity.Efforts(), effort) {
			return Settings{}, validationErrorf("geminiEffort must be one of %s",
				strings.Join(antigravity.Efforts(), ", "))
		}
		cfg.GeminiEffort = effort
	}
	if req.ClaudeModel != nil {
		if strings.TrimSpace(*req.ClaudeModel) == "" {
			return Settings{}, validationErrorf("claudeModel cannot be empty")
		}
		cfg.ClaudeModel = *req.ClaudeModel
	}
	if req.CodexModel != nil {
		if strings.TrimSpace(*req.CodexModel) == "" {
			return Settings{}, validationErrorf("codexModel cannot be empty")
		}
		cfg.CodexModel = *req.CodexModel
	}
	if req.MaxAgentTurns != nil {
		if *req.MaxAgentTurns < 0 {
			return Settings{}, validationErrorf("maxAgentTurns cannot be negative")
		}
		cfg.MaxAgentTurns = *req.MaxAgentTurns
	}
	if req.GitHubHost != nil {
		if strings.TrimSpace(*req.GitHubHost) == "" {
			return Settings{}, validationErrorf("githubHost cannot be empty")
		}
		cfg.GitHubHost = *req.GitHubHost
	}
	if req.GitHubInsecureHTTP != nil {
		cfg.GitHubInsecureHTTP = *req.GitHubInsecureHTTP
	}
	if req.GCPProject != nil {
		cfg.GCPProject = *req.GCPProject
	}
	if req.GCPServiceAccountEmail != nil {
		cfg.GCPServiceAccountEmail = *req.GCPServiceAccountEmail
	}
	if req.TargetRepos != nil {
		for _, r := range *req.TargetRepos {
			if _, err := model.ParseRepo(r); err != nil {
				return Settings{}, validationErrorf("targetRepos: %v", err)
			}
		}
		cfg.TargetRepos = *req.TargetRepos
	}
	if req.DefaultCapabilities != nil {
		// Rejected here, not skipped the way CreateTask skips a stored id
		// with no row: this is somebody choosing the set, and a choice
		// that cannot take effect should be refused while whoever made it
		// is still looking at it. Duplicates are dropped rather than
		// refused -- a picker can produce one, and a set is what this is.
		seen := map[string]bool{}
		ids := make([]string, 0, len(*req.DefaultCapabilities))
		for _, id := range *req.DefaultCapabilities {
			if _, ok := c.capabilityByID(id); !ok {
				return Settings{}, validationErrorf("defaultCapabilities: unknown capability %s", id)
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
		cfg.DefaultCapabilities = ids
	}
	if req.NewestFirst != nil {
		cfg.NewestFirst = *req.NewestFirst
	}
	if req.SandboxCPUs != nil {
		// Bounds mirror bwsalmon/kontur's own staticpod.VMSpec.Validate
		// ("cpus must be at least 1") -- 0 is the one value this rejects
		// that Validate would not, since 0 means "unset" here rather
		// than a literal request for a zero-vCPU VM.
		if *req.SandboxCPUs != 0 && *req.SandboxCPUs < 1 {
			return Settings{}, validationErrorf("sandboxCpus must be 0 (unset) or at least 1")
		}
		cfg.SandboxCPUs = *req.SandboxCPUs
	}
	if req.SandboxMemoryMB != nil {
		// Mirrors staticpod.VMSpec.Validate's own "memory-mb must be at
		// least 128" for the same reason SandboxCPUs' check does.
		if *req.SandboxMemoryMB != 0 && *req.SandboxMemoryMB < 128 {
			return Settings{}, validationErrorf("sandboxMemoryMb must be 0 (unset) or at least 128")
		}
		cfg.SandboxMemoryMB = *req.SandboxMemoryMB
	}
	if req.SandboxDiskGB != nil {
		// No bound from bwsalmon/kontur to mirror here, unlike the two
		// above: konturctl checks a disk size against the guest image the
		// overlay reads through to rather than against a floor of its
		// own, so the only value this has to reject is a negative one,
		// and 0 keeps meaning "unset" the same way it does everywhere
		// else in this shape.
		if *req.SandboxDiskGB < 0 {
			return Settings{}, validationErrorf("sandboxDiskGb must be 0 (unset) or at least 1")
		}
		cfg.SandboxDiskGB = *req.SandboxDiskGB
	}
	if req.ShowClosedByDefault != nil {
		cfg.ShowClosedByDefault = *req.ShowClosedByDefault
	}
	if req.EnvironmentName != nil {
		// Trimmed, so "  " and "" both store as the unnamed deployment
		// they mean rather than as a name made of spaces that the UI
		// would render as an empty badge.
		name := strings.TrimSpace(*req.EnvironmentName)
		if utf8.RuneCountInString(name) > maxEnvironmentNameLen {
			return Settings{}, validationErrorf("environmentName cannot be longer than %d characters", maxEnvironmentNameLen)
		}
		// A control character here would be pasted straight into the
		// badge the frontend renders; a newline in particular turns one
		// line of chrome into two. There is no environment anyone names
		// with one, so this is a refusal rather than a silent strip.
		if strings.ContainsFunc(name, func(r rune) bool { return r == '\n' || r == '\r' || r == '\t' }) {
			return Settings{}, validationErrorf("environmentName cannot contain line breaks or tabs")
		}
		cfg.EnvironmentName = name
	}
	if req.TimeZone != nil {
		// Trimmed for the same reason environmentName above is: a name
		// pasted with a stray space is the zone whoever pasted it meant,
		// and " " is a cleared field rather than a zone.
		zone := strings.TrimSpace(*req.TimeZone)
		if !model.ValidTimeZone(zone) {
			return Settings{}, validationErrorf("timeZone must be an IANA time zone name, e.g. %s", model.DefaultTimeZone)
		}
		cfg.TimeZone = model.TimeZoneOrDefault(zone)
	}
	if req.PromptExtension != nil {
		// Trimmed and nothing else: this is prose handed to an agent,
		// exactly like a task's own description, so there is no shape to
		// validate it against -- and unlike EnvironmentName, which is
		// rendered into a badge, there is nowhere here a line break would
		// break anything. Trimming is what keeps "cleared" and "a
		// newline" the same setting (model.PromptExtensionFor trims
		// again on the way out, for a row this never saw).
		cfg.PromptExtension = strings.TrimSpace(*req.PromptExtension)
	}
	if req.ApprovedByDefault != nil {
		cfg.ApprovedByDefault = *req.ApprovedByDefault
	}
	if req.AutoMergeByDefault != nil {
		cfg.AutoMergeByDefault = *req.AutoMergeByDefault
	}
	if req.AgentFramework != nil {
		// NormalizeAgentFramework first, so the legacy "gemini" spelling
		// a stored row or an older client may still send is stored as
		// the framework it now means rather than rejected as a word with
		// no implementation behind it.
		normalized := model.NormalizeAgentFramework(*req.AgentFramework)
		if !model.ValidAgentFramework(normalized) {
			return Settings{}, validationErrorf("agentFramework must be %s", model.AgentFrameworkNames())
		}
		cfg.AgentFramework = normalized
	}
	// AgentFramework's own meaningful zero value is "antigravity", not
	// "" -- model.Config.AgentFramework's own doc comment -- so a first
	// save that never mentions it still stores something every
	// agent.Framework switch can match on, the same as every settings row
	// Store.PutConfig has ever written from before this field existed.
	cfg.AgentFramework = model.NormalizeAgentFramework(cfg.AgentFramework)

	if firstTime {
		if cfg.PollInterval <= 0 {
			return Settings{}, validationErrorf("pollInterval is required the first time settings are saved")
		}
		if cfg.MaxWorkers < 1 {
			return Settings{}, validationErrorf("maxWorkers is required the first time settings are saved")
		}
		if strings.TrimSpace(cfg.GeminiModel) == "" {
			return Settings{}, validationErrorf("geminiModel is required the first time settings are saved")
		}
		if strings.TrimSpace(cfg.ClaudeModel) == "" {
			return Settings{}, validationErrorf("claudeModel is required the first time settings are saved")
		}
		if strings.TrimSpace(cfg.GitHubHost) == "" {
			return Settings{}, validationErrorf("githubHost is required the first time settings are saved")
		}
	}

	if err := c.Store.PutConfig(ctx, cfg); err != nil {
		return Settings{}, err
	}
	// Config.TargetRepos is also read in-process, unguarded by the store
	// (targetAllowed in CreateTask, handleConfig's own response) -- keep
	// it in step with what was just written so neither goes stale until
	// this server restarts. Every other Config field stays exactly as
	// NewClient set it; TargetRepos is the only one a running server ever
	// changes.
	c.setTargetRepos(cfg.TargetRepos)
	repoConfigs, err := c.Store.ListRepoConfigs(ctx)
	if err != nil {
		return Settings{}, err
	}
	return c.settingsFrom(cfg, repoConfigs), nil
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.tasks.GetSettings(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req UpdateSettingsRequest
	if !readJSON(w, r, &req) {
		return
	}
	settings, err := s.tasks.UpdateSettings(r.Context(), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
