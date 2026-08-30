package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// templateNotFoundError marks a template ID with no template behind it --
// scheduleNotFoundError's own reasoning, so the message a caller sees
// names a template, not a task or a schedule.
type templateNotFoundError struct{ ID string }

func (e *templateNotFoundError) Error() string { return "no task template " + e.ID }

// Template is a task template's JSON shape (bwsalmon/agents#516) --
// Schedule's own content fields (Title/Description/Repo/Base/AutoMerge/
// Reads/Capabilities), the subset a schedule's ScheduleForm already
// collects, minus everything about firing on a cadence: a template is
// never itself something that fires, only something a schedule (or a
// future caller) fires from.
type Template struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Repo         string    `json:"repo"`
	Base         string    `json:"base,omitempty"`
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
		Repo:         t.Target.String(),
		Base:         t.Base,
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
// own content subset, Recurrence/Enabled left out for the same reason
// Template itself leaves them out.
type CreateTemplateRequest struct {
	Name         string   `json:"name"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Repo         string   `json:"repo"`
	Base         string   `json:"base"`
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
	if strings.TrimSpace(req.Repo) == "" {
		return Template{}, validationErrorf("repo is required")
	}
	target, err := model.ParseRepo(req.Repo)
	if err != nil {
		return Template{}, &ValidationError{err: err}
	}
	grants, err := c.grantsFor(req.Capabilities)
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
		Target:    target,
		Base:      req.Base,
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
	Repo         *string   `json:"repo,omitempty"`
	Base         *string   `json:"base,omitempty"`
	AutoMerge    *bool     `json:"autoMerge,omitempty"`
	Capabilities *[]string `json:"capabilities,omitempty"`
	Reads        *[]string `json:"reads,omitempty"`
}

// UpdateTemplate edits a template's fields in place.
func (c *Client) UpdateTemplate(ctx context.Context, id string, req UpdateTemplateRequest) (Template, error) {
	var target *model.RepoRef
	if req.Repo != nil {
		if strings.TrimSpace(*req.Repo) == "" {
			return Template{}, validationErrorf("repo cannot be empty: a template with no target repo cannot be used")
		}
		parsed, err := model.ParseRepo(*req.Repo)
		if err != nil {
			return Template{}, &ValidationError{err: err}
		}
		target = &parsed
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		return Template{}, validationErrorf("name cannot be empty")
	}
	if req.Title != nil && strings.TrimSpace(*req.Title) == "" {
		return Template{}, validationErrorf("title cannot be empty")
	}
	var grants []model.Grant
	if req.Capabilities != nil {
		var err error
		grants, err = c.grantsFor(*req.Capabilities)
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
		if target != nil {
			t.Target = *target
		}
		if req.Base != nil {
			t.Base = *req.Base
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
// still fires from it: unlike editing a template (which every schedule
// pointing at it is meant to pick up), deleting one out from under a live
// schedule would silently strand that schedule's next firing with no
// content to file, worse than the plain 404 fireScheduledTask would
// otherwise have to surface days or weeks later. A human wanting to
// delete it anyway repoints or deletes those schedules first.
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
