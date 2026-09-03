package ui

import (
	"context"
	"net/http"
	"sort"

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

// RepoSummary is one repo as `grain repo list` reports it (grain/
// task-36): whether the deployment's allowlist names it, how its tasks
// are spread across the states, and what it defaults on its own -- the
// repos page's own row, for a shell with no page to open.
//
// Composed on the client side, out of GET /api/config and GET /api/tasks,
// rather than served by a GET /api/repos of its own. A repo is not a
// stored row here: docs/data-model.md's folder tree is still unbuilt, so
// "a repo grain knows about" is *derived* -- whatever TargetRepos names,
// union whatever a task targets, union whatever carries defaults of its
// own -- and the frontend already derives exactly that from these same
// two responses (state.js's repoRows). A third derivation, on the
// server, would be a second definition of the same thing to keep in
// step with the first.
type RepoSummary struct {
	// Repo is "owner/name".
	Repo string `json:"repo"`
	// Configured is whether Config.TargetRepos names this repo, as
	// against it being known only because something else here mentions
	// it -- repoRows' own `configured`, and the same distinction that
	// decides whether a repo's own page offers Remove. Always
	// false on an unrestricted deployment, whose allowlist is empty by
	// definition.
	Configured bool `json:"configured"`
	// Tasks is how many tasks target this repo (Task.Repo, never Reads
	// -- a read-only repo grants nothing and is not what a task "belongs
	// to"), and States is that same count broken down by state.
	Tasks  int                 `json:"tasks"`
	States map[model.State]int `json:"states,omitempty"`
	// Blocked is how many of those tasks are waiting on a link that is
	// still open (model.IsBlocked) -- the "something is stuck here"
	// signal the repos page exists to surface at a glance.
	Blocked int `json:"blocked"`
	// DefaultCapabilities is this repo's *own* default set
	// (model.RepoConfig.DefaultCapabilities), not the union a task filed
	// here would actually hold: `grain repo capabilities` reports all
	// three sets, and a list of every repo is the wrong place to repeat
	// the deployment-wide layer once per row.
	DefaultCapabilities []string `json:"defaultCapabilities,omitempty"`
}

// RepoStateOrder is the order a repo's per-state counts are meant to be
// read in -- ui/src/state.js's own STATE_ORDER, mirrored here so `grain
// repo list` walks a repo's tasks the way the repos page does rather
// than landing on Go's map iteration order.
var RepoStateOrder = []model.State{
	model.StateProposed,
	model.StateQueued,
	model.StateRunning,
	model.StateAwaitingReply,
	model.StateFailed,
	model.StateCompleted,
	model.StateClosed,
}

// repoSummaries composes one RepoSummary per known repo, sorted by name,
// from the three things that can make grain know a repo at all: the
// allowlist, the tasks that target it, and the defaults it carries.
//
// That third source is where this deviates from state.js's repoRows,
// deliberately. SetRepoDefaultCapabilities does not require a repo to be
// allow-listed (its own doc comment: a repo can be configured before it
// is allowed, and stays configured after it is removed), so a repo can
// hold defaults while matching neither of the other two sources.
// Dropping it would mean `grain repo capabilities` could write a set
// that `grain repo list` then never admits exists -- and a list whose
// whole job includes reporting per-repo defaults must not be the one
// place they are invisible.
func repoSummaries(targetRepos []string, tasks []Task, repoDefaults map[string][]string) []RepoSummary {
	byRepo := make(map[string]*RepoSummary)
	row := func(repo string) *RepoSummary {
		if r, ok := byRepo[repo]; ok {
			return r
		}
		r := &RepoSummary{Repo: repo, DefaultCapabilities: repoDefaults[repo]}
		byRepo[repo] = r
		return r
	}
	for _, repo := range targetRepos {
		row(repo).Configured = true
	}
	for repo := range repoDefaults {
		row(repo)
	}
	for _, t := range tasks {
		// A task nobody has pointed at a repo yet is omitted rather than
		// grouped under a blank name, the same as repoRows.
		if t.Repo == "" {
			continue
		}
		r := row(t.Repo)
		r.Tasks++
		if r.States == nil {
			r.States = make(map[model.State]int)
		}
		r.States[t.State]++
		if t.Blocked {
			r.Blocked++
		}
	}
	out := make([]RepoSummary, 0, len(byRepo))
	for _, r := range byRepo {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
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
