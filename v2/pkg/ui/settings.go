package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/kontur"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Settings is the JSON shape of a deployment's store-backed
// configuration (model.Config, bwsalmon/agents#320) -- the knobs
// cmd/grain's "daemon" subcommand reads out of grain_config at startup
// instead of only from its own flags (daemon.go's loadConfig). This
// package is what lets a UI or a CLI actually be the thing that changes
// them, the same "the store is the record" shape pkg/ui already gives
// tasks.
//
// Configured is false when nothing has written a row yet -- before any
// daemon has ever started against this store -- so a caller can tell
// that apart from "every field happens to be a zero value" the same way
// Store.GetConfig's own nil does.
type Settings struct {
	Configured bool `json:"configured"`
	// PollInterval is a Go duration string ("30s"), the same syntax
	// -poll-interval itself parses.
	PollInterval           string `json:"pollInterval"`
	MaxConcurrent          int    `json:"maxConcurrent"`
	GeminiModel            string `json:"geminiModel"`
	MaxAgentTurns          int    `json:"maxAgentTurns"`
	GitHubHost             string `json:"githubHost"`
	GitHubInsecureHTTP     bool   `json:"githubInsecureHttp"`
	GCPProject             string `json:"gcpProject"`
	GCPServiceAccountEmail string `json:"gcpServiceAccountEmail"`
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
	// (bwsalmon/agents#476): false, the default, keeps a new task's
	// place at the back of the dispatch queue even though it always
	// shows up first in the task list; true moves it to the front of
	// both instead.
	NewestFirst bool `json:"newestFirst"`
	// SandboxCPUs and SandboxMemoryMB (bwsalmon/agents#534) are the
	// deployment-wide default shape a kontur-managed sandbox VM is
	// created with -- model.Config's own fields of the same name. Zero
	// (the default for both) means "use bwsalmon/kontur's own default"
	// rather than a deliberately tiny VM. Meaningless, and simply unused,
	// under a deployment running the default local-directory sandboxing
	// (no -kontur-sandboxes); GetSettings still reports whatever is
	// stored either way, the same as every other kontur* setting here.
	SandboxCPUs     int `json:"sandboxCpus"`
	SandboxMemoryMB int `json:"sandboxMemoryMb"`
	// SandboxCPUsDefault and SandboxMemoryMBDefault are bwsalmon/kontur's
	// own default VM shape (kontur.DefaultCPUs/DefaultMemoryMB) -- the
	// shape actually in effect whenever SandboxCPUs/SandboxMemoryMB above
	// is 0, surfaced so a caller can show that real current shape instead
	// of a bare, misleadingly literal 0 (bwsalmon/agents#610). Constant,
	// never read from or written to the store.
	SandboxCPUsDefault     int `json:"sandboxCpusDefault"`
	SandboxMemoryMBDefault int `json:"sandboxMemoryMbDefault"`
	// ShowClosedByDefault is model.Config's own field of the same name
	// (bwsalmon/agents#537): the deployment-wide default for whether a
	// task list's own "Show closed tasks" toggle starts checked. Also
	// mirrored onto GET /api/config (handleConfig, tasks.go) so a list
	// has it to seed that toggle with before Settings has ever been
	// opened this session.
	ShowClosedByDefault bool `json:"showClosedByDefault"`
	// Capabilities is every capability grain ships a provider for, with
	// this deployment's own readiness computed against it -- capability_
	// status.go's own CapabilityStatus, bwsalmon/agents#611. Always
	// populated, even before Settings has ever been saved (GetSettings),
	// since self-debug/self-repair/bootstrap-playbooks need no
	// configuration to be ready and an operator setting up a fresh
	// deployment still benefits from seeing that
	// gcp-key/gemini-key/github-sandbox are not yet.
	Capabilities []CapabilityStatus `json:"capabilities"`
	// ApprovedByDefault and AutoMergeByDefault are model.Config's own
	// fields of the same name (bwsalmon/agents#612): deployment-wide
	// defaults for whether a new task's "Queue immediately" and
	// "Auto-merge once checks pass" checkboxes start checked in
	// NewTaskOverlay.jsx. Also mirrored onto GET /api/config
	// (configResponse, tasks.go) the same way ShowClosedByDefault is, so
	// that form has them to seed its checkboxes with before Settings has
	// ever been opened this session.
	ApprovedByDefault  bool `json:"approvedByDefault"`
	AutoMergeByDefault bool `json:"autoMergeByDefault"`
	// AgentFramework is model.Config's own field of the same name
	// (bwsalmon/agents#609): "gemini" or "claude"
	// (model.AgentFrameworkGemini/AgentFrameworkClaude), which
	// agent.Framework implementation a run is meant to be driven by.
	// Never empty coming out of here -- GetSettings/UpdateSettings both
	// default it to "gemini" the same way Config.AgentFramework's own doc
	// comment says an empty stored value reads back. Setting this to
	// "claude" does not yet change what a run actually does -- see that
	// field's own doc comment.
	AgentFramework string `json:"agentFramework"`
}

func (c *Client) settingsFrom(cfg model.Config) Settings {
	agentFramework := cfg.AgentFramework
	if agentFramework == "" {
		agentFramework = model.AgentFrameworkGemini
	}
	return Settings{
		Configured:                    true,
		PollInterval:                  cfg.PollInterval.String(),
		MaxConcurrent:                 cfg.MaxConcurrent,
		GeminiModel:                   cfg.GeminiModel,
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
		SandboxCPUsDefault:            kontur.DefaultCPUs,
		SandboxMemoryMBDefault:        kontur.DefaultMemoryMB,
		ShowClosedByDefault:           cfg.ShowClosedByDefault,
		Capabilities:                  c.capabilityStatuses(cfg),
		ApprovedByDefault:             cfg.ApprovedByDefault,
		AutoMergeByDefault:            cfg.AutoMergeByDefault,
		AgentFramework:                agentFramework,
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
	if cfg == nil {
		return Settings{
			SandboxCPUsDefault:     kontur.DefaultCPUs,
			SandboxMemoryMBDefault: kontur.DefaultMemoryMB,
			Capabilities:           c.capabilityStatuses(model.Config{}),
		}, nil
	}
	return c.settingsFrom(*cfg), nil
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
	PollInterval           *string   `json:"pollInterval"`
	MaxConcurrent          *int      `json:"maxConcurrent"`
	GeminiModel            *string   `json:"geminiModel"`
	MaxAgentTurns          *int      `json:"maxAgentTurns"`
	GitHubHost             *string   `json:"githubHost"`
	GitHubInsecureHTTP     *bool     `json:"githubInsecureHttp"`
	GCPProject             *string   `json:"gcpProject"`
	GCPServiceAccountEmail *string   `json:"gcpServiceAccountEmail"`
	TargetRepos            *[]string `json:"targetRepos"`
	NewestFirst            *bool     `json:"newestFirst"`
	SandboxCPUs            *int      `json:"sandboxCpus"`
	SandboxMemoryMB        *int      `json:"sandboxMemoryMb"`
	ShowClosedByDefault    *bool     `json:"showClosedByDefault"`
	ApprovedByDefault      *bool     `json:"approvedByDefault"`
	AutoMergeByDefault     *bool     `json:"autoMergeByDefault"`
	AgentFramework         *string   `json:"agentFramework"`
}

// UpdateSettings applies req on top of whatever is currently stored (the
// zero model.Config if nothing is yet) and writes the result back
// wholesale.
//
// The first time settings are ever saved, PollInterval, MaxConcurrent,
// GeminiModel and GitHubHost are required: leaving one of them out would
// otherwise write a zero value that reads back later as a deliberate
// setting rather than as "never configured" -- Configured already tells
// a caller that much on the way in, so writing a config that could not
// be told apart from one somebody actually chose is worse than asking
// for the field up front. MaxAgentTurns, GitHubInsecureHTTP,
// GCPProject, GCPServiceAccountEmail and TargetRepos have real,
// meaningful zero values (the framework's own default, HTTPS, "no GCP
// capability configured", and "unrestricted" respectively -- daemon.go's
// own flag defaults), so nothing here demands them.
func (c *Client) UpdateSettings(ctx context.Context, req UpdateSettingsRequest) (Settings, error) {
	current, err := c.Store.GetConfig(ctx)
	if err != nil {
		return Settings{}, err
	}
	var cfg model.Config
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
	if req.MaxConcurrent != nil {
		if *req.MaxConcurrent < 1 {
			return Settings{}, validationErrorf("maxConcurrent must be at least 1")
		}
		cfg.MaxConcurrent = *req.MaxConcurrent
	}
	if req.GeminiModel != nil {
		if strings.TrimSpace(*req.GeminiModel) == "" {
			return Settings{}, validationErrorf("geminiModel cannot be empty")
		}
		cfg.GeminiModel = *req.GeminiModel
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
	if req.ShowClosedByDefault != nil {
		cfg.ShowClosedByDefault = *req.ShowClosedByDefault
	}
	if req.ApprovedByDefault != nil {
		cfg.ApprovedByDefault = *req.ApprovedByDefault
	}
	if req.AutoMergeByDefault != nil {
		cfg.AutoMergeByDefault = *req.AutoMergeByDefault
	}
	if req.AgentFramework != nil {
		switch *req.AgentFramework {
		case model.AgentFrameworkGemini, model.AgentFrameworkClaude:
			cfg.AgentFramework = *req.AgentFramework
		default:
			return Settings{}, validationErrorf("agentFramework must be %q or %q", model.AgentFrameworkGemini, model.AgentFrameworkClaude)
		}
	}
	// AgentFramework's own meaningful zero value is "gemini", not "" --
	// model.Config.AgentFramework's own doc comment -- so a first save
	// that never mentions it still stores something every agent.Framework
	// switch can match on, the same as every settings row Store.PutConfig
	// has ever written from before this field existed.
	if cfg.AgentFramework == "" {
		cfg.AgentFramework = model.AgentFrameworkGemini
	}

	if firstTime {
		if cfg.PollInterval <= 0 {
			return Settings{}, validationErrorf("pollInterval is required the first time settings are saved")
		}
		if cfg.MaxConcurrent < 1 {
			return Settings{}, validationErrorf("maxConcurrent is required the first time settings are saved")
		}
		if strings.TrimSpace(cfg.GeminiModel) == "" {
			return Settings{}, validationErrorf("geminiModel is required the first time settings are saved")
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
	return c.settingsFrom(cfg), nil
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
