package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

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
	PollInterval           string   `json:"pollInterval"`
	Slots                  []string `json:"slots"`
	GeminiModel            string   `json:"geminiModel"`
	MaxAgentTurns          int      `json:"maxAgentTurns"`
	GitHubHost             string   `json:"githubHost"`
	GitHubInsecureHTTP     bool     `json:"githubInsecureHttp"`
	GCPProject             string   `json:"gcpProject"`
	GCPServiceAccountEmail string   `json:"gcpServiceAccountEmail"`
}

func settingsFrom(c model.Config) Settings {
	return Settings{
		Configured:             true,
		PollInterval:           c.PollInterval.String(),
		Slots:                  c.Slots,
		GeminiModel:            c.GeminiModel,
		MaxAgentTurns:          c.MaxAgentTurns,
		GitHubHost:             c.GitHubHost,
		GitHubInsecureHTTP:     c.GitHubInsecureHTTP,
		GCPProject:             c.GCPProject,
		GCPServiceAccountEmail: c.GCPServiceAccountEmail,
	}
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
		return Settings{}, nil
	}
	return settingsFrom(*cfg), nil
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
	Slots                  *[]string `json:"slots"`
	GeminiModel            *string   `json:"geminiModel"`
	MaxAgentTurns          *int      `json:"maxAgentTurns"`
	GitHubHost             *string   `json:"githubHost"`
	GitHubInsecureHTTP     *bool     `json:"githubInsecureHttp"`
	GCPProject             *string   `json:"gcpProject"`
	GCPServiceAccountEmail *string   `json:"gcpServiceAccountEmail"`
}

// UpdateSettings applies req on top of whatever is currently stored (the
// zero model.Config if nothing is yet) and writes the result back
// wholesale.
//
// The first time settings are ever saved, PollInterval, Slots,
// GeminiModel and GitHubHost are required: leaving one of them out would
// otherwise write a zero value that reads back later as a deliberate
// setting rather than as "never configured" -- Configured already tells
// a caller that much on the way in, so writing a config that could not
// be told apart from one somebody actually chose is worse than asking
// for the field up front. MaxAgentTurns, GitHubInsecureHTTP,
// GCPProject and GCPServiceAccountEmail have real, meaningful zero
// values (the framework's own default, HTTPS, and "no GCP capability
// configured" respectively -- daemon.go's own flag defaults), so nothing
// here demands them.
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
	if req.Slots != nil {
		if len(*req.Slots) == 0 {
			return Settings{}, validationErrorf("slots cannot be empty")
		}
		cfg.Slots = *req.Slots
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

	if firstTime {
		if cfg.PollInterval <= 0 {
			return Settings{}, validationErrorf("pollInterval is required the first time settings are saved")
		}
		if len(cfg.Slots) == 0 {
			return Settings{}, validationErrorf("slots is required the first time settings are saved")
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
	return settingsFrom(cfg), nil
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
