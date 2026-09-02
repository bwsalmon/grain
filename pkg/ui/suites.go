package ui

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// suiteNotFoundError marks a suite ID with no task suite behind it --
// scheduleNotFoundError's own reasoning, so the message a caller sees
// names a suite, not a task or a schedule.
type suiteNotFoundError struct{ ID string }

func (e *suiteNotFoundError) Error() string { return "no task suite " + e.ID }

// suiteRunNotFoundError is the same, for a run id.
type suiteRunNotFoundError struct{ ID int64 }

func (e *suiteRunNotFoundError) Error() string {
	return "no task suite run " + strconv.FormatInt(e.ID, 10)
}

// SuiteItem is one template a suite runs, over the wire -- TemplateName
// is resolved by the caller (ListSuites/CreateSuite/UpdateSuite fetch it
// once per distinct id), Schedule.TemplateName's own reasoning.
type SuiteItem struct {
	TemplateID   string `json:"templateId"`
	TemplateName string `json:"templateName,omitempty"`
}

// Suite is a task suite's JSON shape (bwsalmon/agents#642) -- a saved
// combination of task templates plus how to run them, created once and
// run against any repo and branch any number of times.
type Suite struct {
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	Items []SuiteItem `json:"items"`
	// Mode is "count" or "until_clean" -- model.TaskSuiteMode's own
	// vocabulary, unchanged, since it is already the plain English a UI
	// wants to show and to send back.
	Mode string `json:"mode"`
	// Count is meaningful for Mode "count" only; MaxPasses for
	// "until_clean" only -- which is which is model.TaskSuite.Validate's
	// own job, not this type's.
	Count           int       `json:"count,omitempty"`
	MaxPasses       int       `json:"maxPasses,omitempty"`
	RequireApproval bool      `json:"requireApproval"`
	AutoMerge       bool      `json:"autoMerge"`
	CreatedAt       time.Time `json:"createdAt"`
}

func suiteItemsFrom(items []model.TaskSuiteItem, names map[string]string) []SuiteItem {
	out := make([]SuiteItem, 0, len(items))
	for _, it := range items {
		out = append(out, SuiteItem{TemplateID: it.TemplateID, TemplateName: names[it.TemplateID]})
	}
	return out
}

func suiteFrom(s model.TaskSuite, names map[string]string) Suite {
	return Suite{
		ID:              s.ID,
		Name:            s.Name,
		Items:           suiteItemsFrom(s.Items, names),
		Mode:            string(s.Mode),
		Count:           s.Count,
		MaxPasses:       s.MaxPasses,
		RequireApproval: s.RequireApproval,
		AutoMerge:       s.AutoMerge,
		CreatedAt:       s.CreatedAt,
	}
}

// templateNamesFor resolves every id in ids to its template's own Name,
// once per distinct id -- ListSchedules' own caching, applied here since
// a suite's own items (and a list of many suites) can repeat a template.
func (c *Client) templateNamesFor(ctx context.Context, ids []string) (map[string]string, error) {
	names := map[string]string{}
	for _, id := range ids {
		if _, ok := names[id]; ok || id == "" {
			continue
		}
		if t, err := c.Store.GetTaskTemplate(ctx, id); err == nil && t != nil {
			names[id] = t.Name
		}
	}
	return names, nil
}

func templateIDsOf(items []model.TaskSuiteItem) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.TemplateID
	}
	return ids
}

// ListSuites returns every task suite, newest first.
func (c *Client) ListSuites(ctx context.Context) ([]Suite, error) {
	list, err := c.Store.ListTaskSuites(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range list {
		ids = append(ids, templateIDsOf(s.Items)...)
	}
	names, err := c.templateNamesFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Suite, 0, len(list))
	for _, s := range list {
		out = append(out, suiteFrom(s, names))
	}
	return out, nil
}

// parseSuiteMode validates mode and the count/maxPasses pair that goes
// with it -- CreateSuite's and UpdateSuite's shared check, so a bad
// suite is refused here rather than left for model.TaskSuite.Validate to
// discover with a less specific message.
func parseSuiteMode(mode string, count, maxPasses int) (model.TaskSuiteMode, error) {
	switch model.TaskSuiteMode(mode) {
	case model.TaskSuiteCount:
		if count < 1 {
			return "", validationErrorf("count must be at least 1")
		}
		return model.TaskSuiteCount, nil
	case model.TaskSuiteUntilClean:
		if maxPasses < 1 {
			return "", validationErrorf("maxPasses must be at least 1")
		}
		return model.TaskSuiteUntilClean, nil
	default:
		return "", validationErrorf(`mode must be "count" or "until_clean", got %q`, mode)
	}
}

func parseSuiteItems(templateIDs []string) ([]model.TaskSuiteItem, error) {
	if len(templateIDs) == 0 {
		return nil, validationErrorf("a task suite needs at least one task template")
	}
	items := make([]model.TaskSuiteItem, 0, len(templateIDs))
	for _, id := range templateIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, validationErrorf("a task suite item needs a template")
		}
		items = append(items, model.TaskSuiteItem{TemplateID: id})
	}
	return items, nil
}

// checkSuiteTemplatesExist reports an error naming the first templateId
// in items that GetTaskTemplate cannot find -- CreateSuite's and
// UpdateSuite's own early check, so a suite pointed at a typo'd or
// already-deleted template is refused at save time rather than left for
// CreateTaskSuiteRun/FireNextPass to fail on days or a pass later.
func (c *Client) checkSuiteTemplatesExist(ctx context.Context, items []model.TaskSuiteItem) error {
	for _, it := range items {
		t, err := c.Store.GetTaskTemplate(ctx, it.TemplateID)
		if err != nil {
			return err
		}
		if t == nil {
			return validationErrorf("unknown template %s", it.TemplateID)
		}
	}
	return nil
}

// CreateSuiteRequest is a new suite's fields. AutoMerge, omitted,
// defaults to true and RequireApproval, omitted, defaults to false --
// bwsalmon/agents#642's own "by default they should auto queue and auto
// merge."
type CreateSuiteRequest struct {
	Name            string   `json:"name"`
	TemplateIDs     []string `json:"templateIds"`
	Mode            string   `json:"mode"`
	Count           int      `json:"count"`
	MaxPasses       int      `json:"maxPasses"`
	RequireApproval bool     `json:"requireApproval"`
	AutoMerge       *bool    `json:"autoMerge"`
}

// CreateSuite files a new task suite template straight into the store.
func (c *Client) CreateSuite(ctx context.Context, req CreateSuiteRequest) (Suite, error) {
	if strings.TrimSpace(req.Name) == "" {
		return Suite{}, validationErrorf("name is required")
	}
	items, err := parseSuiteItems(req.TemplateIDs)
	if err != nil {
		return Suite{}, err
	}
	mode, err := parseSuiteMode(req.Mode, req.Count, req.MaxPasses)
	if err != nil {
		return Suite{}, err
	}
	if err := c.checkSuiteTemplatesExist(ctx, items); err != nil {
		return Suite{}, err
	}
	autoMerge := true
	if req.AutoMerge != nil {
		autoMerge = *req.AutoMerge
	}

	id, err := c.Store.NewTaskSuiteID(ctx)
	if err != nil {
		return Suite{}, err
	}
	suite := model.TaskSuite{
		ID:              id,
		Name:            req.Name,
		Items:           items,
		Mode:            mode,
		Count:           req.Count,
		MaxPasses:       req.MaxPasses,
		RequireApproval: req.RequireApproval,
		AutoMerge:       autoMerge,
		CreatedAt:       c.now(),
	}
	if err := suite.Validate(); err != nil {
		return Suite{}, &ValidationError{err: err}
	}
	if err := c.Store.PutTaskSuite(ctx, suite); err != nil {
		return Suite{}, err
	}
	names, err := c.templateNamesFor(ctx, templateIDsOf(suite.Items))
	if err != nil {
		return Suite{}, err
	}
	return suiteFrom(suite, names), nil
}

// UpdateSuiteRequest is a suite's editable fields -- nil means "leave
// this one alone", UpdateScheduleRequest's own convention. Editing a
// suite never changes a run already in flight (model.TaskSuiteRun's own
// doc comment: everything a run needs is snapshotted at creation).
type UpdateSuiteRequest struct {
	Name            *string   `json:"name,omitempty"`
	TemplateIDs     *[]string `json:"templateIds,omitempty"`
	Mode            *string   `json:"mode,omitempty"`
	Count           *int      `json:"count,omitempty"`
	MaxPasses       *int      `json:"maxPasses,omitempty"`
	RequireApproval *bool     `json:"requireApproval,omitempty"`
	AutoMerge       *bool     `json:"autoMerge,omitempty"`
}

// UpdateSuite edits a suite's fields in place.
func (c *Client) UpdateSuite(ctx context.Context, id string, req UpdateSuiteRequest) (Suite, error) {
	existing, err := c.Store.GetTaskSuite(ctx, id)
	if err != nil {
		return Suite{}, err
	}
	if existing == nil {
		return Suite{}, &suiteNotFoundError{ID: id}
	}

	var items []model.TaskSuiteItem
	if req.TemplateIDs != nil {
		items, err = parseSuiteItems(*req.TemplateIDs)
		if err != nil {
			return Suite{}, err
		}
		if err := c.checkSuiteTemplatesExist(ctx, items); err != nil {
			return Suite{}, err
		}
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		return Suite{}, validationErrorf("name cannot be empty")
	}

	// Validated against the merged copy before anything is written, not
	// inside the store's own read-modify-write closure: mutate may run
	// more than once against a freshly re-read row (Store.UpdateTaskSuite's
	// own retry shape), and model.TaskSuite.Validate's error is a plain
	// error, not this package's ValidationError -- easier to check once,
	// here, where the type writeClientError needs to see it as is still
	// in scope, than to have the store's own s.write wrap it in transit.
	merged := *existing
	if req.Name != nil {
		merged.Name = *req.Name
	}
	if items != nil {
		merged.Items = items
	}
	if req.Mode != nil {
		merged.Mode = model.TaskSuiteMode(*req.Mode)
	}
	if req.Count != nil {
		merged.Count = *req.Count
	}
	if req.MaxPasses != nil {
		merged.MaxPasses = *req.MaxPasses
	}
	if req.RequireApproval != nil {
		merged.RequireApproval = *req.RequireApproval
	}
	if req.AutoMerge != nil {
		merged.AutoMerge = *req.AutoMerge
	}
	if err := merged.Validate(); err != nil {
		return Suite{}, &ValidationError{err: err}
	}

	if err := c.Store.UpdateTaskSuite(ctx, id, func(s *model.TaskSuite) error {
		*s = merged
		return nil
	}); err != nil {
		return Suite{}, err
	}

	updated, err := c.Store.GetTaskSuite(ctx, id)
	if err != nil {
		return Suite{}, err
	}
	names, err := c.templateNamesFor(ctx, templateIDsOf(updated.Items))
	if err != nil {
		return Suite{}, err
	}
	return suiteFrom(*updated, names), nil
}

// DeleteSuite removes a suite outright -- DeleteTaskSuite's own doc
// comment gives the reasoning: a run already started from it keeps its
// own snapshot and is untouched.
func (c *Client) DeleteSuite(ctx context.Context, id string) error {
	existing, err := c.Store.GetTaskSuite(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return &suiteNotFoundError{ID: id}
	}
	return c.Store.DeleteTaskSuite(ctx, id)
}

// --- runs --------------------------------------------------------------

// SuiteRunTask is one task instance a run's pass instantiated, over the
// wire.
type SuiteRunTask struct {
	TaskID            string `json:"taskId"`
	TemplateID        string `json:"templateId"`
	TemplateName      string `json:"templateName"`
	Pass              int    `json:"pass"`
	State             string `json:"state"`
	Approved          bool   `json:"approved"`
	OpenedPullRequest bool   `json:"openedPullRequest"`
	Proposed          bool   `json:"proposed"`
}

// SuiteRun is one run of a suite against a repo and branch, over the
// wire -- what a "see the status of outstanding task suite runs" view
// needs (bwsalmon/agents#642).
type SuiteRun struct {
	ID              int64  `json:"id"`
	SuiteID         string `json:"suiteId"`
	SuiteName       string `json:"suiteName"`
	Repo            string `json:"repo"`
	Base            string `json:"base"`
	Mode            string `json:"mode"`
	Count           int    `json:"count,omitempty"`
	MaxPasses       int    `json:"maxPasses,omitempty"`
	RequireApproval bool   `json:"requireApproval"`
	AutoMerge       bool   `json:"autoMerge"`
	// Status is one of "active", "succeeded", "failed" --
	// model.TaskSuiteRunStatus's own vocabulary.
	Status      string         `json:"status"`
	Error       string         `json:"error,omitempty"`
	Pass        int            `json:"pass"`
	CreatedAt   time.Time      `json:"createdAt"`
	CompletedAt *time.Time     `json:"completedAt,omitempty"`
	Tasks       []SuiteRunTask `json:"tasks"`
}

func suiteRunFrom(r model.TaskSuiteRun) SuiteRun {
	out := SuiteRun{
		ID:              r.ID,
		SuiteID:         r.SuiteID,
		SuiteName:       r.SuiteName,
		Repo:            r.Target.String(),
		Base:            r.Base,
		Mode:            string(r.Mode),
		Count:           r.Count,
		MaxPasses:       r.MaxPasses,
		RequireApproval: r.RequireApproval,
		AutoMerge:       r.AutoMerge,
		Status:          string(r.Status),
		Error:           r.LastError,
		Pass:            r.CurrentPass(),
		CreatedAt:       r.CreatedAt,
		CompletedAt:     r.CompletedAt,
	}
	for _, t := range r.Tasks {
		out.Tasks = append(out.Tasks, SuiteRunTask{
			TaskID: t.TaskID, TemplateID: t.TemplateID, TemplateName: t.TemplateName,
			Pass: t.PassNumber, State: string(t.State), Approved: t.Approved,
			OpenedPullRequest: t.OpenedPullRequest, Proposed: t.Proposed,
		})
	}
	return out
}

// ListSuiteRuns returns every task suite run, newest first.
func (c *Client) ListSuiteRuns(ctx context.Context) ([]SuiteRun, error) {
	list, err := c.Store.ListTaskSuiteRuns(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SuiteRun, 0, len(list))
	for _, r := range list {
		out = append(out, suiteRunFrom(r))
	}
	return out, nil
}

// GetSuiteRun returns one run, or an error if there is none with that id.
func (c *Client) GetSuiteRun(ctx context.Context, id int64) (SuiteRun, error) {
	run, err := c.Store.GetTaskSuiteRun(ctx, id)
	if err != nil {
		return SuiteRun{}, err
	}
	if run == nil {
		return SuiteRun{}, &suiteRunNotFoundError{ID: id}
	}
	return suiteRunFrom(*run), nil
}

// CreateSuiteRunRequest starts suiteId running against repo and base --
// bwsalmon/agents#642's own "run the template against a repo and
// branch."
type CreateSuiteRunRequest struct {
	SuiteID string `json:"suiteId"`
	Repo    string `json:"repo"`
	Base    string `json:"base"`
}

// CreateSuiteRun starts a new run of req.SuiteID against req.Repo and
// req.Base, filing its first pass immediately.
func (c *Client) CreateSuiteRun(ctx context.Context, req CreateSuiteRunRequest) (SuiteRun, error) {
	if strings.TrimSpace(req.SuiteID) == "" {
		return SuiteRun{}, validationErrorf("suiteId is required")
	}
	if strings.TrimSpace(req.Repo) == "" {
		return SuiteRun{}, validationErrorf("repo is required")
	}
	if strings.TrimSpace(req.Base) == "" {
		return SuiteRun{}, validationErrorf("base is required: a task suite run has no default branch to fall back to")
	}
	target, err := model.ParseRepo(req.Repo)
	if err != nil {
		return SuiteRun{}, &ValidationError{err: err}
	}
	suite, err := c.Store.GetTaskSuite(ctx, req.SuiteID)
	if err != nil {
		return SuiteRun{}, err
	}
	if suite == nil {
		return SuiteRun{}, &suiteNotFoundError{ID: req.SuiteID}
	}
	run, err := c.Store.CreateTaskSuiteRun(ctx, *suite, target, req.Base, c.now())
	if err != nil {
		return SuiteRun{}, err
	}
	return suiteRunFrom(run), nil
}

// --- handlers ------------------------------------------------------------

func (s *Server) handleListSuites(w http.ResponseWriter, r *http.Request) {
	list, err := s.tasks.ListSuites(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateSuite(w http.ResponseWriter, r *http.Request) {
	var req CreateSuiteRequest
	if !readJSON(w, r, &req) {
		return
	}
	suite, err := s.tasks.CreateSuite(r.Context(), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, suite)
}

func (s *Server) handleUpdateSuite(w http.ResponseWriter, r *http.Request) {
	var req UpdateSuiteRequest
	if !readJSON(w, r, &req) {
		return
	}
	suite, err := s.tasks.UpdateSuite(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, suite)
}

func (s *Server) handleDeleteSuite(w http.ResponseWriter, r *http.Request) {
	if err := s.tasks.DeleteSuite(r.Context(), r.PathValue("id")); err != nil {
		writeClientError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListSuiteRuns(w http.ResponseWriter, r *http.Request) {
	list, err := s.tasks.ListSuiteRuns(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateSuiteRun(w http.ResponseWriter, r *http.Request) {
	var req CreateSuiteRunRequest
	if !readJSON(w, r, &req) {
		return
	}
	run, err := s.tasks.CreateSuiteRun(r.Context(), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) handleGetSuiteRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeClientError(w, validationErrorf("invalid run id"))
		return
	}
	run, err := s.tasks.GetSuiteRun(r.Context(), id)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}
