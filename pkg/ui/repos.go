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
// against (a zero PollInterval panics time.NewTicker at the next
// restart -- see UpdateSettings' own doc comment).
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
