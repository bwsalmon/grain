package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// templateNotFoundError marks a template ID with no template behind it --
// scheduleNotFoundError's own reasoning, so the message a caller sees
// names a template, not a task or a schedule.
type templateNotFoundError struct{ ID string }

func (e *templateNotFoundError) Error() string { return "no task template " + e.ID }

// Template is a task template's JSON shape (bwsalmon/agents#516) --
// Schedule's own content fields (Title/Description/AutoMerge/Reads/
// Capabilities), the subset a schedule's ScheduleOverlay.jsx already
// collects, minus everything about firing on a cadence and minus Repo/
// Base: a template is never itself something that fires, only something
// a schedule (or a future caller) fires from, and which repo and branch
// a firing targets is a property of that caller, not of the template
// (model.TaskTemplate's own doc comment on why).
type Template struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	AutoMerge    bool      `json:"autoMerge"`
	Reads        []string  `json:"reads,omitempty"`
	Capabilities []string  `json:"capabilities"`
	CreatedAt    time.Time `json:"createdAt"`
}

func templateFrom(t model.TaskTemplate) Template {
	out := Template{
		ID:           t.ID,
		Name:         t.Name,
		Title:        t.Title,
		Description:  t.Body,
		AutoMerge:    t.AutoMerge,
		Capabilities: []string{},
		CreatedAt:    t.CreatedAt,
	}
	for _, r := range t.Reads {
		out.Reads = append(out.Reads, r.String())
	}
	for _, g := range t.Grants {
		out.Capabilities = append(out.Capabilities, g.Capability)
	}
	return out
}

// ListTemplates returns every template, newest first.
func (c *Client) ListTemplates(ctx context.Context) ([]Template, error) {
	list, err := c.Store.ListTaskTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(list))
	for _, t := range list {
		out = append(out, templateFrom(t))
	}
	return out, nil
}

// CreateTemplateRequest is a new template's fields -- CreateScheduleRequest's
// own content subset, minus Repo/Base (a template carries no target of
// its own, model.TaskTemplate's own doc comment on why) and minus
// Recurrence/Enabled for the same reason Template itself leaves them out.
type CreateTemplateRequest struct {
	Name         string   `json:"name"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	AutoMerge    bool     `json:"autoMerge"`
	Capabilities []string `json:"capabilities"`
	Reads        []string `json:"reads"`
}

// CreateTemplate files a new template straight into the store.
func (c *Client) CreateTemplate(ctx context.Context, req CreateTemplateRequest) (Template, error) {
	if strings.TrimSpace(req.Name) == "" {
		return Template{}, validationErrorf("name is required")
	}
	if strings.TrimSpace(req.Title) == "" {
		return Template{}, validationErrorf("title is required")
	}
	grants, err := c.grantsFor(req.Capabilities, model.GrantByLabel)
	if err != nil {
		return Template{}, err
	}
	reads, err := parseReads(req.Reads)
	if err != nil {
		return Template{}, err
	}

	id, err := c.Store.NewTaskTemplateID(ctx)
	if err != nil {
		return Template{}, err
	}
	tmpl := model.TaskTemplate{
		ID:        id,
		Name:      req.Name,
		Title:     req.Title,
		Body:      req.Description,
		AutoMerge: req.AutoMerge,
		Reads:     reads,
		Grants:    grants,
		CreatedAt: c.now(),
	}
	if err := c.Store.PutTaskTemplate(ctx, tmpl); err != nil {
		return Template{}, err
	}
	return templateFrom(tmpl), nil
}

// UpdateTemplateRequest is a template's editable fields -- nil means
// "leave this one alone", UpdateScheduleRequest's own convention.
// Changing a template that a schedule already points at takes effect the
// next time that schedule fires (orchestrator.fireScheduledTask resolves
// TemplateID fresh each time), with no separate step to "push" the change
// out to every schedule using it.
type UpdateTemplateRequest struct {
	Name         *string   `json:"name,omitempty"`
	Title        *string   `json:"title,omitempty"`
	Description  *string   `json:"description,omitempty"`
	AutoMerge    *bool     `json:"autoMerge,omitempty"`
	Capabilities *[]string `json:"capabilities,omitempty"`
	Reads        *[]string `json:"reads,omitempty"`
}

// UpdateTemplate edits a template's fields in place.
func (c *Client) UpdateTemplate(ctx context.Context, id string, req UpdateTemplateRequest) (Template, error) {
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		return Template{}, validationErrorf("name cannot be empty")
	}
	if req.Title != nil && strings.TrimSpace(*req.Title) == "" {
		return Template{}, validationErrorf("title cannot be empty")
	}
	var grants []model.Grant
	if req.Capabilities != nil {
		var err error
		grants, err = c.grantsFor(*req.Capabilities, model.GrantByLabel)
		if err != nil {
			return Template{}, err
		}
	}
	var reads []model.RepoRef
	if req.Reads != nil {
		var err error
		reads, err = parseReads(*req.Reads)
		if err != nil {
			return Template{}, err
		}
	}

	existing, err := c.Store.GetTaskTemplate(ctx, id)
	if err != nil {
		return Template{}, err
	}
	if existing == nil {
		return Template{}, &templateNotFoundError{ID: id}
	}

	if err := c.Store.UpdateTaskTemplate(ctx, id, func(t *model.TaskTemplate) error {
		if req.Name != nil {
			t.Name = *req.Name
		}
		if req.Title != nil {
			t.Title = *req.Title
		}
		if req.Description != nil {
			t.Body = *req.Description
		}
		if req.AutoMerge != nil {
			t.AutoMerge = *req.AutoMerge
		}
		if req.Capabilities != nil {
			t.Grants = grants
		}
		if req.Reads != nil {
			t.Reads = reads
		}
		return nil
	}); err != nil {
		return Template{}, err
	}

	updated, err := c.Store.GetTaskTemplate(ctx, id)
	if err != nil {
		return Template{}, err
	}
	return templateFrom(*updated), nil
}

// DeleteTemplate removes a template outright -- DeleteScheduledTask's own
// "no history worth keeping" reasoning applies again here, except a
// template additionally refuses to delete out from under a schedule that
// still fires from it, a qualification plan (bwsalmon/agents#518) that
// still schedules from it, or a task suite (bwsalmon/agents#642) that
// still runs it: unlike editing a template (which every schedule, plan
// or suite pointing at it is meant to pick up), deleting one out from
// under any of them would silently strand its next firing -- a
// schedule's with no content to file, a plan's with no template for
// CreateQualificationRun to resolve, a suite's with no template for
// CreateTaskSuiteRun/FireNextPass to resolve -- worse than the plain,
// retried error each would otherwise have to surface days or weeks
// later. A human wanting to delete it anyway repoints or deletes those
// schedules, plans and suites first.
func (c *Client) DeleteTemplate(ctx context.Context, id string) error {
	existing, err := c.Store.GetTaskTemplate(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return &templateNotFoundError{ID: id}
	}
	inUse, err := c.Store.SchedulesUsingTemplate(ctx, id)
	if err != nil {
		return err
	}
	if len(inUse) > 0 {
		return validationErrorf(
			"template is used by %d schedule(s); repoint or delete those first", len(inUse))
	}
	usedBy, err := c.Store.QualificationPlansUsingTemplate(ctx, id)
	if err != nil {
		return err
	}
	if len(usedBy) > 0 {
		return validationErrorf(
			"template is used by %d repo's qualification plan; remove it from those first", len(usedBy))
	}
	suites, err := c.Store.TaskSuitesUsingTemplate(ctx, id)
	if err != nil {
		return err
	}
	if len(suites) > 0 {
		return validationErrorf(
			"template is used by %d task suite(s); remove it from those first", len(suites))
	}
	return c.Store.DeleteTaskTemplate(ctx, id)
}

// --- handlers ------------------------------------------------------------

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	list, err := s.tasks.ListTemplates(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req CreateTemplateRequest
	if !readJSON(w, r, &req) {
		return
	}
	tmpl, err := s.tasks.CreateTemplate(r.Context(), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tmpl)
}

func (s *Server) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	var req UpdateTemplateRequest
	if !readJSON(w, r, &req) {
		return
	}
	tmpl, err := s.tasks.UpdateTemplate(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tmpl)
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	if err := s.tasks.DeleteTemplate(r.Context(), r.PathValue("id")); err != nil {
		writeClientError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
