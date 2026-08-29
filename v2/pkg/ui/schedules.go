package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// scheduleNotFoundError marks a schedule ID with no schedule behind it --
// writeClientError maps it to a 404, the same as NotFoundError does for
// a task id. It is its own type rather than NotFoundError itself so the
// message a caller sees names a schedule, not a task.
type scheduleNotFoundError struct{ ID string }

func (e *scheduleNotFoundError) Error() string { return "no scheduled task " + e.ID }

// Schedule is a schedule's JSON shape -- everything a UI needs to list,
// create, edit and pause one. There is no separate "run now" or history
// list here: NextRunAt/LastRunAt are the whole of what a UI can show
// about a schedule's own timing, and every task it has ever filed already
// shows up in the ordinary task list (Origin.Reason == schedule renders
// there as Task.Scheduled).
type Schedule struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Repo        string `json:"repo"`
	Base        string `json:"base,omitempty"`
	AutoMerge   bool   `json:"autoMerge"`
	// Interval is a Go duration string ("24h0m0s"), the same syntax
	// Settings.PollInterval already uses for the same reason: it is what
	// -poll-interval and time.ParseDuration both already speak, so there
	// is no second interval syntax for a caller to learn.
	Interval  string     `json:"interval"`
	Enabled   bool       `json:"enabled"`
	NextRunAt time.Time  `json:"nextRunAt"`
	LastRunAt *time.Time `json:"lastRunAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

func scheduleFrom(s model.ScheduledTask) Schedule {
	return Schedule{
		ID:          s.ID,
		Title:       s.Title,
		Description: s.Body,
		Repo:        s.Target.String(),
		Base:        s.Base,
		AutoMerge:   s.AutoMerge,
		Interval:    s.Interval.String(),
		Enabled:     s.Enabled,
		NextRunAt:   s.NextRunAt,
		LastRunAt:   s.LastRunAt,
		CreatedAt:   s.CreatedAt,
	}
}

// ListSchedules returns every schedule, newest first.
func (c *Client) ListSchedules(ctx context.Context) ([]Schedule, error) {
	list, err := c.Store.ListScheduledTasks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Schedule, 0, len(list))
	for _, s := range list {
		out = append(out, scheduleFrom(s))
	}
	return out, nil
}

// CreateScheduleRequest is a new schedule's fields -- CreateTaskRequest's
// own shape, minus DependsOn/Reads/Capabilities/Approved: a schedule
// fires with no reviewer in the loop each time (see fireScheduledTask's
// own doc comment on why it needs no approved flag), so those task-only
// concerns have no home here. Repo and Interval are required: a schedule
// with no write target or no cadence cannot ever fire.
type CreateScheduleRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Repo        string `json:"repo"`
	Base        string `json:"base"`
	AutoMerge   bool   `json:"autoMerge"`
	Interval    string `json:"interval"`
	// Enabled, omitted, defaults to true -- a schedule somebody just
	// created is normally meant to run, not to sit paused until a second
	// click.
	Enabled *bool `json:"enabled"`
}

// CreateSchedule files a new schedule straight into the store, due to
// fire for the first time the moment the next cycle runs -- CreateTask's
// own "queued the moment it is written" immediacy, applied to a
// schedule's own NextRunAt instead of a task's state.
func (c *Client) CreateSchedule(ctx context.Context, req CreateScheduleRequest) (Schedule, error) {
	if strings.TrimSpace(req.Title) == "" {
		return Schedule{}, validationErrorf("title is required")
	}
	if strings.TrimSpace(req.Repo) == "" {
		return Schedule{}, validationErrorf("repo is required")
	}
	target, err := model.ParseRepo(req.Repo)
	if err != nil {
		return Schedule{}, &ValidationError{err: err}
	}
	interval, err := parseInterval(req.Interval)
	if err != nil {
		return Schedule{}, err
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	id, err := c.Store.NewScheduledTaskID(ctx)
	if err != nil {
		return Schedule{}, err
	}
	now := c.now()
	sched := model.ScheduledTask{
		ID:        id,
		Title:     req.Title,
		Body:      req.Description,
		Target:    target,
		Base:      req.Base,
		AutoMerge: req.AutoMerge,
		Interval:  interval,
		Enabled:   enabled,
		NextRunAt: now,
		CreatedAt: now,
	}
	if err := c.Store.PutScheduledTask(ctx, sched); err != nil {
		return Schedule{}, err
	}
	return scheduleFrom(sched), nil
}

// UpdateScheduleRequest is a schedule's editable fields -- nil means
// "leave this one alone", UpdateTaskRequest's own convention. Enabled is
// the pause/resume toggle: setting it false stops this schedule from
// ever coming due without losing its NextRunAt, so resuming it later
// picks up rather than firing every interval that elapsed while paused.
type UpdateScheduleRequest struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Repo        *string `json:"repo,omitempty"`
	Base        *string `json:"base,omitempty"`
	AutoMerge   *bool   `json:"autoMerge,omitempty"`
	Interval    *string `json:"interval,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

// UpdateSchedule edits a schedule's fields in place -- its NextRunAt and
// LastRunAt are left exactly as they are; only fireScheduledTask ever
// changes those.
func (c *Client) UpdateSchedule(ctx context.Context, id string, req UpdateScheduleRequest) (Schedule, error) {
	var target *model.RepoRef
	if req.Repo != nil {
		if strings.TrimSpace(*req.Repo) == "" {
			return Schedule{}, validationErrorf("repo cannot be empty: a schedule with no target cannot fire")
		}
		parsed, err := model.ParseRepo(*req.Repo)
		if err != nil {
			return Schedule{}, &ValidationError{err: err}
		}
		target = &parsed
	}
	if req.Title != nil && strings.TrimSpace(*req.Title) == "" {
		return Schedule{}, validationErrorf("title cannot be empty")
	}
	var interval *time.Duration
	if req.Interval != nil {
		d, err := parseInterval(*req.Interval)
		if err != nil {
			return Schedule{}, err
		}
		interval = &d
	}

	existing, err := c.Store.GetScheduledTask(ctx, id)
	if err != nil {
		return Schedule{}, err
	}
	if existing == nil {
		return Schedule{}, &scheduleNotFoundError{ID: id}
	}

	if err := c.Store.UpdateScheduledTask(ctx, id, func(s *model.ScheduledTask) error {
		if req.Title != nil {
			s.Title = *req.Title
		}
		if req.Description != nil {
			s.Body = *req.Description
		}
		if target != nil {
			s.Target = *target
		}
		if req.Base != nil {
			s.Base = *req.Base
		}
		if req.AutoMerge != nil {
			s.AutoMerge = *req.AutoMerge
		}
		if interval != nil {
			s.Interval = *interval
		}
		if req.Enabled != nil {
			s.Enabled = *req.Enabled
		}
		return nil
	}); err != nil {
		return Schedule{}, err
	}

	updated, err := c.Store.GetScheduledTask(ctx, id)
	if err != nil {
		return Schedule{}, err
	}
	return scheduleFrom(*updated), nil
}

// DeleteSchedule removes a schedule outright -- Store.DeleteScheduledTask's
// own doc comment gives the reasoning: unlike a task, a schedule is only
// ever a standing declaration, so there is no history on the row itself
// worth keeping once a human no longer wants it.
func (c *Client) DeleteSchedule(ctx context.Context, id string) error {
	existing, err := c.Store.GetScheduledTask(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return &scheduleNotFoundError{ID: id}
	}
	return c.Store.DeleteScheduledTask(ctx, id)
}

func parseInterval(text string) (time.Duration, error) {
	d, err := time.ParseDuration(text)
	if err != nil {
		return 0, validationErrorf("interval: %v", err)
	}
	if d <= 0 {
		return 0, validationErrorf("interval must be positive")
	}
	return d, nil
}

// --- handlers ------------------------------------------------------------

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := s.tasks.ListSchedules(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var req CreateScheduleRequest
	if !readJSON(w, r, &req) {
		return
	}
	sched, err := s.tasks.CreateSchedule(r.Context(), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sched)
}

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	var req UpdateScheduleRequest
	if !readJSON(w, r, &req) {
		return
	}
	sched, err := s.tasks.UpdateSchedule(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sched)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.tasks.DeleteSchedule(r.Context(), r.PathValue("id")); err != nil {
		writeClientError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
