package ui

import (
	"context"
	"net/http"

	"github.com/bwsalmon/grain/pkg/model"
)

// AddRepoRequest is POST /api/repos' body.
type AddRepoRequest struct {
	Repo string `json:"repo"`
}

// AddTargetRepo appends repo to the deployment's TargetRepos allowlist --
// the repos pane's own "Add" (bwsalmon/agents#473), replacing the text
// field Settings used to bury this behind. A no-op, not an error, when
// repo is already present.
//
// This is a read-modify-write against whatever UpdateSettings already
// has stored, delegated to it rather than duplicated here, so a
// deployment that has never saved settings at all still gets
// UpdateSettings' own "poll interval etc. are required the first time"
// refusal instead of silently seeding a Config no daemon could start
// against (a zero PollInterval panics time.NewTicker the next time a
// daemon starts against it -- see UpdateSettings' own doc comment).
func (c *Client) AddTargetRepo(ctx context.Context, repo string) (Settings, error) {
	parsed, err := model.ParseRepo(repo)
	if err != nil {
		return Settings{}, validationErrorf("repo: %v", err)
	}
	repo = parsed.String()

	current, err := c.GetSettings(ctx)
	if err != nil {
		return Settings{}, err
	}
	for _, r := range current.TargetRepos {
		if r == repo {
			return current, nil
		}
	}
	updated := append(append([]string{}, current.TargetRepos...), repo)
	return c.UpdateSettings(ctx, UpdateSettingsRequest{TargetRepos: &updated})
}

// RemoveTargetRepo removes repo from the deployment's TargetRepos
// allowlist -- the repos pane's own "Remove" (bwsalmon/agents#473). A
// no-op, not an error, when repo isn't present -- including every repo
// on an unrestricted deployment (empty TargetRepos), which are only ever
// known through the tasks that already target them rather than through
// this allowlist.
//
// Removing repo here only narrows what a *new* task may target from now
// on; any task that already targets it keeps doing so, and keeps
// showing repo on the repos pane regardless of TargetRepos, the same way
// it would if repo had never been added at all.
func (c *Client) RemoveTargetRepo(ctx context.Context, repo string) (Settings, error) {
	current, err := c.GetSettings(ctx)
	if err != nil {
		return Settings{}, err
	}
	updated := make([]string, 0, len(current.TargetRepos))
	for _, r := range current.TargetRepos {
		if r != repo {
			updated = append(updated, r)
		}
	}
	if len(updated) == len(current.TargetRepos) {
		return current, nil
	}
	return c.UpdateSettings(ctx, UpdateSettingsRequest{TargetRepos: &updated})
}

// RepoDefaults is one repo's own task defaults -- GET and PUT
// /api/repos/{owner}/{name}/capabilities' shared JSON shape, and the
// per-repo half of what the Settings pane's Capabilities tab chooses
// deployment-wide (grain/task-24).
//
// It reports all three sets rather than only the one that is editable
// here, because the editable one is meaningless on its own: a repo row
// that lists gcp-key looks identical whether that is the only thing
// putting gcp-key on tasks here or a restatement of something every task
// in the deployment already gets, and the pane doing the editing has to
// be able to say which.
type RepoDefaults struct {
	Repo string `json:"repo"`
	// DefaultCapabilities is this repo's own set --
	// model.RepoConfig.DefaultCapabilities, exactly as stored, and the
	// only one of the three a PUT here writes.
	DefaultCapabilities []string `json:"defaultCapabilities"`
	// DeploymentDefaultCapabilities is model.Config.DefaultCapabilities,
	// the deployment-wide layer, reported read-only: it is chosen on
	// Settings' Capabilities tab, and a repo can add to it but never
	// subtract from it (model.RepoConfig.DefaultCapabilities has why).
	// The repo pane annotates each of these in its own picker, so
	// "unticking this here would not turn it off" is visible rather than
	// discovered by trying.
	DeploymentDefaultCapabilities []string `json:"deploymentDefaultCapabilities"`
	// EffectiveDefaultCapabilities is what a task filed against this repo
	// would actually start out holding: the union of the two above,
	// deployment-wide first, filtered to what this build still offers --
	// (*Client).defaultCapabilities' own answer, for this repo, rather
	// than a second computation of it that could drift.
	EffectiveDefaultCapabilities []string `json:"effectiveDefaultCapabilities"`
}

// SetRepoCapabilitiesRequest is PUT /api/repos/{owner}/{name}/
// capabilities' body: the whole of this repo's own default set,
// replaced rather than added to, the same way UpdateSettingsRequest.
// DefaultCapabilities replaces the deployment-wide one.
//
// A plain []string rather than the *[]string that field uses: there is
// one field here and replacing it is the entire request, so an omitted
// list and an empty one mean the same thing -- this repo adds nothing --
// and there is no "leave this one alone" for a request whose only
// purpose is to say what the set now is.
type SetRepoCapabilitiesRequest struct {
	DefaultCapabilities []string `json:"defaultCapabilities"`
}

// RepoDefaults reads repo's own defaults, alongside the deployment-wide
// set they compose with and the effective set that composition produces.
// A repo with nothing stored is not an error: it reports an empty own
// set, which is exactly what it contributes.
func (c *Client) RepoDefaults(ctx context.Context, repo string) (RepoDefaults, error) {
	parsed, err := model.ParseRepo(repo)
	if err != nil {
		return RepoDefaults{}, validationErrorf("repo: %v", err)
	}
	return c.repoDefaults(ctx, parsed)
}

func (c *Client) repoDefaults(ctx context.Context, repo model.RepoRef) (RepoDefaults, error) {
	out := RepoDefaults{Repo: repo.String()}
	stored, err := c.Store.GetRepoConfig(ctx, repo)
	if err != nil {
		return RepoDefaults{}, err
	}
	if stored != nil {
		out.DefaultCapabilities = stored.DefaultCapabilities
	}
	cfg, err := c.Store.GetConfig(ctx)
	if err != nil {
		return RepoDefaults{}, err
	}
	if cfg != nil {
		out.DeploymentDefaultCapabilities = cfg.DefaultCapabilities
	}
	effective, err := c.defaultCapabilities(ctx, &repo)
	if err != nil {
		return RepoDefaults{}, err
	}
	out.EffectiveDefaultCapabilities = effective
	return out, nil
}

// SetRepoDefaultCapabilities replaces repo's own default capability set
// -- the repos pane's per-repo counterpart to the Settings pane's
// deployment-wide picker (bwsalmon/grain task-24).
//
// Every id must have a row in OfferedCapabilities, rejected here rather
// than skipped for the same reason UpdateSettings rejects one: this is
// somebody choosing a set, and a choice that could never take effect
// should be refused while whoever made it is still looking at it.
// Duplicates are dropped rather than refused -- a picker can produce one,
// and a set is what this is.
//
// An id the deployment already defaults is accepted and stored as given,
// not folded away: the two layers are chosen by different people at
// different times, and a repo that names one explicitly keeps it if the
// deployment-wide entry is later dropped (model.RepoConfig.
// DefaultCapabilities). What a task is filed with is the union either
// way, so storing it changes nothing today and preserves an intent that
// would otherwise be silently discarded.
//
// This does not check repo against Config.TargetRepos. A repo can be
// configured before it is allow-listed, and a repo removed from the
// allowlist keeps whatever it said here -- the same independence
// RemoveTargetRepo already leaves between "targeted" and "configured".
// Nothing is granted by this row alone: a task still has to target the
// repo, and targeting one off the allowlist parks the task before it
// ever dispatches (parkOffAllowlist).
func (c *Client) SetRepoDefaultCapabilities(ctx context.Context, repo string, ids []string) (RepoDefaults, error) {
	parsed, err := model.ParseRepo(repo)
	if err != nil {
		return RepoDefaults{}, validationErrorf("repo: %v", err)
	}
	seen := make(map[string]bool, len(ids))
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := c.capabilityByID(id); !ok {
			return RepoDefaults{}, validationErrorf("defaultCapabilities: unknown capability %s", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}
	if err := c.Store.PutRepoConfig(ctx, model.RepoConfig{Repo: parsed, DefaultCapabilities: clean}); err != nil {
		return RepoDefaults{}, err
	}
	return c.repoDefaults(ctx, parsed)
}

func (s *Server) handleGetRepoCapabilities(w http.ResponseWriter, r *http.Request) {
	defaults, err := s.tasks.RepoDefaults(r.Context(), r.PathValue("owner")+"/"+r.PathValue("name"))
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, defaults)
}

func (s *Server) handleSetRepoCapabilities(w http.ResponseWriter, r *http.Request) {
	var req SetRepoCapabilitiesRequest
	if !readJSON(w, r, &req) {
		return
	}
	defaults, err := s.tasks.SetRepoDefaultCapabilities(r.Context(),
		r.PathValue("owner")+"/"+r.PathValue("name"), req.DefaultCapabilities)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, defaults)
}

func (s *Server) handleAddTargetRepo(w http.ResponseWriter, r *http.Request) {
	var req AddRepoRequest
	if !readJSON(w, r, &req) {
		return
	}
	settings, err := s.tasks.AddTargetRepo(r.Context(), req.Repo)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleRemoveTargetRepo(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	settings, err := s.tasks.RemoveTargetRepo(r.Context(), repo)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
