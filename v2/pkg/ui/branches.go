package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Branch is one requested branch's own JSON shape -- bwsalmon/
// agents#638's own "create a new branch on a repo from the repo page."
type Branch struct {
	Repo      string    `json:"repo"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	// Error is the branches reconciler's own account of why this branch
	// hasn't been created on GitHub yet, if anything has gone wrong.
	Error string `json:"error,omitempty"`
}

func branchFrom(b model.Branch) Branch {
	return Branch{
		Repo: b.Repo.String(), Name: b.Name, Status: string(b.Status),
		CreatedAt: b.CreatedAt, Error: b.LastError,
	}
}

func branchesFrom(bs []model.Branch) []Branch {
	out := make([]Branch, len(bs))
	for i, b := range bs {
		out[i] = branchFrom(b)
	}
	return out
}

// CreateBranchRequest is a new branch's user-given name -- the issue's
// own "create the given branch."
type CreateBranchRequest struct {
	Name string `json:"name"`
}

// CreateBranch is the issue's own "Add the ability to create a new branch
// on a repo from the repo page. This should just create the given branch
// in github": it records a fresh model.Branch declaring the intent, and
// returns immediately -- the branches reconciler (pkg/orchestrator.
// SyncBranches) is what actually creates it on GitHub, at repo's current
// default branch tip.
func (c *Client) CreateBranch(ctx context.Context, repo model.RepoRef, req CreateBranchRequest) (Branch, error) {
	name := strings.TrimSpace(req.Name)
	b, err := c.Store.CreateBranch(ctx, repo, name, c.now())
	if err != nil {
		if errors.Is(err, model.ErrInvalidBranchName) {
			return Branch{}, validationErrorf("invalid branch name")
		}
		return Branch{}, err
	}
	return branchFrom(b), nil
}

// ListBranches returns every branch ever requested for repo, newest
// first.
func (c *Client) ListBranches(ctx context.Context, repo model.RepoRef) ([]Branch, error) {
	bs, err := c.Store.ListBranches(ctx, repo)
	if err != nil {
		return nil, err
	}
	return branchesFrom(bs), nil
}

// --- handlers ------------------------------------------------------------

func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) {
	repo, err := parseRepoPath(r)
	if err != nil {
		writeClientError(w, err)
		return
	}
	branches, err := s.tasks.ListBranches(r.Context(), repo)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, branches)
}

func (s *Server) handleCreateBranch(w http.ResponseWriter, r *http.Request) {
	repo, err := parseRepoPath(r)
	if err != nil {
		writeClientError(w, err)
		return
	}
	var req CreateBranchRequest
	if !readJSON(w, r, &req) {
		return
	}
	branch, err := s.tasks.CreateBranch(r.Context(), repo, req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, branch)
}
