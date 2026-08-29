package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// ReleaseConfig is a repo's release settings' JSON shape -- the prod/rc
// branch names, release branch prefix and hand-edited major version
// bwsalmon/agents#398 asked for. Configured is false when nothing has
// saved one yet, the same "tell unconfigured apart from every field
// happening to be zero" convention Settings already uses.
type ReleaseConfig struct {
	Configured          bool   `json:"configured"`
	Repo                string `json:"repo"`
	ProdBranch          string `json:"prodBranch"`
	RCBranch            string `json:"rcBranch"`
	ReleaseBranchPrefix string `json:"releaseBranchPrefix"`
	MajorVersion        int    `json:"majorVersion"`
}

func releaseConfigFrom(cfg model.ReleaseConfig) ReleaseConfig {
	return ReleaseConfig{
		Configured: true, Repo: cfg.Repo.String(),
		ProdBranch: cfg.ProdBranch, RCBranch: cfg.RCBranch,
		ReleaseBranchPrefix: cfg.ReleaseBranchPrefix, MajorVersion: cfg.MajorVersion,
	}
}

// Candidate is one release candidate's JSON shape -- what the "cut" and
// "promote" buttons in a repo's own release panel act on and render.
type Candidate struct {
	ID           int64  `json:"id"`
	Repo         string `json:"repo"`
	MajorVersion int    `json:"majorVersion"`
	Number       int    `json:"number"`
	Version      int    `json:"version"`
	// Label is the issue's own naming scheme rendered -- "3.7-rc1" --
	// what a human reads rather than reconstructing from the three
	// numbers above.
	Label         string `json:"label"`
	Branch        string `json:"branch"`
	ReleaseBranch string `json:"releaseBranch,omitempty"`
	// Status is one of "cutting", "active", "promoting", "promoted" --
	// model.CandidateStatus's own vocabulary, unchanged, since it is
	// already the plain English a UI wants to show.
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	PromotedAt *time.Time `json:"promotedAt,omitempty"`
	// Error is the releases reconciler's own account of why Status hasn't
	// advanced yet, if anything has gone wrong -- model.Candidate's own
	// LastError.
	Error string `json:"error,omitempty"`
}

func candidateFrom(c model.Candidate) Candidate {
	return Candidate{
		ID: c.ID, Repo: c.Repo.String(), MajorVersion: c.MajorVersion,
		Number: c.Number, Version: c.Version,
		Label:         model.CandidateLabel(c.MajorVersion, c.Number, c.Version),
		Branch:        c.Branch,
		ReleaseBranch: c.ReleaseBranch,
		Status:        string(c.Status),
		CreatedAt:     c.CreatedAt,
		PromotedAt:    c.PromotedAt,
		Error:         c.LastError,
	}
}

func candidatesFrom(cs []model.Candidate) []Candidate {
	out := make([]Candidate, len(cs))
	for i, c := range cs {
		out[i] = candidateFrom(c)
	}
	return out
}

func parseRepoPath(r *http.Request) (model.RepoRef, error) {
	owner, name := r.PathValue("owner"), r.PathValue("name")
	if owner == "" || name == "" {
		return model.RepoRef{}, validationErrorf("repo owner and name are required")
	}
	return model.RepoRef{Owner: owner, Name: name}, nil
}

// GetReleaseConfig reads repo's release settings. A zero ReleaseConfig
// with Configured false, and no error, means nothing has saved one yet.
func (c *Client) GetReleaseConfig(ctx context.Context, repo model.RepoRef) (ReleaseConfig, error) {
	cfg, err := c.Store.GetReleaseConfig(ctx, repo)
	if err != nil {
		return ReleaseConfig{}, err
	}
	if cfg == nil {
		return ReleaseConfig{Repo: repo.String()}, nil
	}
	return releaseConfigFrom(*cfg), nil
}

// ListReleaseConfigs returns every repo with release settings configured
// -- what a release panel with no repo already in hand lists to let a
// human pick one.
func (c *Client) ListReleaseConfigs(ctx context.Context) ([]ReleaseConfig, error) {
	cfgs, err := c.Store.ListReleaseConfigs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ReleaseConfig, len(cfgs))
	for i, cfg := range cfgs {
		out[i] = releaseConfigFrom(cfg)
	}
	return out, nil
}

// UpdateReleaseConfigRequest is a repo's release settings, all required:
// unlike UpdateSettings' partial-update shape, there is no existing row
// to leave a field alone against the first time a repo is configured, and
// every field here is meaningful at its zero value (an empty branch name,
// or major version 0) in a way that would silently misconfigure a repo
// rather than plainly reject the request.
type UpdateReleaseConfigRequest struct {
	ProdBranch          string `json:"prodBranch"`
	RCBranch            string `json:"rcBranch"`
	ReleaseBranchPrefix string `json:"releaseBranchPrefix"`
	MajorVersion        int    `json:"majorVersion"`
}

// PutReleaseConfig saves repo's release settings wholesale.
func (c *Client) PutReleaseConfig(ctx context.Context, repo model.RepoRef, req UpdateReleaseConfigRequest) (ReleaseConfig, error) {
	if strings.TrimSpace(req.ProdBranch) == "" {
		return ReleaseConfig{}, validationErrorf("prodBranch cannot be empty")
	}
	if strings.TrimSpace(req.RCBranch) == "" {
		return ReleaseConfig{}, validationErrorf("rcBranch cannot be empty")
	}
	if req.MajorVersion < 0 {
		return ReleaseConfig{}, validationErrorf("majorVersion cannot be negative")
	}
	cfg := model.ReleaseConfig{
		Repo: repo, ProdBranch: req.ProdBranch, RCBranch: req.RCBranch,
		ReleaseBranchPrefix: req.ReleaseBranchPrefix, MajorVersion: req.MajorVersion,
	}
	if err := c.Store.PutReleaseConfig(ctx, cfg); err != nil {
		return ReleaseConfig{}, err
	}
	return releaseConfigFrom(cfg), nil
}

// ListCandidates returns repo's release history, newest first.
func (c *Client) ListCandidates(ctx context.Context, repo model.RepoRef) ([]Candidate, error) {
	cs, err := c.Store.ListCandidates(ctx, repo)
	if err != nil {
		return nil, err
	}
	return candidatesFrom(cs), nil
}

// releaseErrorf maps one of model's own release sentinel errors to the
// ValidationError Server maps to a 400 -- every one of them is a caller
// mistake (repo not configured, wrong sequencing), never a store fault.
func releaseErrorf(err error) error {
	switch {
	case errors.Is(err, model.ErrNoReleaseConfig):
		return validationErrorf("this repo has no release configuration yet")
	case errors.Is(err, model.ErrCandidateActive):
		return validationErrorf("this repo already has an unpromoted release candidate")
	case errors.Is(err, model.ErrNoCandidate):
		return validationErrorf("this repo has no release candidate yet")
	case errors.Is(err, model.ErrCandidateNotReady):
		return validationErrorf("the current release candidate has not finished cutting yet")
	case errors.Is(err, model.ErrAlreadyPromoted):
		return validationErrorf("the current release candidate was already promoted")
	default:
		return err
	}
}

// CutCandidate is the issue's own "create a new rc": it records a fresh
// model.Candidate declaring the cut, and returns immediately -- the
// releases reconciler (pkg/orchestrator.SyncReleases) is what actually
// creates the branch on GitHub, per this package's own doc comment ("
// nothing here talks to GitHub at all").
func (c *Client) CutCandidate(ctx context.Context, repo model.RepoRef) (Candidate, error) {
	candidate, err := c.Store.CutCandidate(ctx, repo, c.now())
	if err != nil {
		return Candidate{}, releaseErrorf(err)
	}
	return candidateFrom(candidate), nil
}

// PromoteCandidate is the issue's own "promote the current rc": it
// records the promotion against repo's current candidate and returns
// immediately, the same declarative handoff CutCandidate makes.
func (c *Client) PromoteCandidate(ctx context.Context, repo model.RepoRef) (Candidate, error) {
	candidate, err := c.Store.PromoteCandidate(ctx, repo)
	if err != nil {
		return Candidate{}, releaseErrorf(err)
	}
	return candidateFrom(candidate), nil
}

// --- handlers ------------------------------------------------------------

func (s *Server) handleGetReleaseConfig(w http.ResponseWriter, r *http.Request) {
	repo, err := parseRepoPath(r)
	if err != nil {
		writeClientError(w, err)
		return
	}
	cfg, err := s.tasks.GetReleaseConfig(r.Context(), repo)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleListReleaseConfigs(w http.ResponseWriter, r *http.Request) {
	cfgs, err := s.tasks.ListReleaseConfigs(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfgs)
}

func (s *Server) handlePutReleaseConfig(w http.ResponseWriter, r *http.Request) {
	repo, err := parseRepoPath(r)
	if err != nil {
		writeClientError(w, err)
		return
	}
	var req UpdateReleaseConfigRequest
	if !readJSON(w, r, &req) {
		return
	}
	cfg, err := s.tasks.PutReleaseConfig(r.Context(), repo, req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleListCandidates(w http.ResponseWriter, r *http.Request) {
	repo, err := parseRepoPath(r)
	if err != nil {
		writeClientError(w, err)
		return
	}
	candidates, err := s.tasks.ListCandidates(r.Context(), repo)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, candidates)
}

func (s *Server) handleCutCandidate(w http.ResponseWriter, r *http.Request) {
	repo, err := parseRepoPath(r)
	if err != nil {
		writeClientError(w, err)
		return
	}
	candidate, err := s.tasks.CutCandidate(r.Context(), repo)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, candidate)
}

func (s *Server) handlePromoteCandidate(w http.ResponseWriter, r *http.Request) {
	repo, err := parseRepoPath(r)
	if err != nil {
		writeClientError(w, err)
		return
	}
	candidate, err := s.tasks.PromoteCandidate(r.Context(), repo)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, candidate)
}
