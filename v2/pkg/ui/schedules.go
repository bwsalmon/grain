package ui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
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

// Recurrence is a schedule's cadence, over the wire -- model.Recurrence's
// fields, with Kind matching model.RecurrenceKind's own string values
// exactly (no second vocabulary to translate) and TimeOfDay/Weekday
// rendered the way a human reads them ("14:30", "monday") rather than as
// minutes-since-midnight or time.Weekday's own int. Which of
// EveryNHours/TimeOfDay/Weekday/DayOfMonth is meaningful depends on Kind,
// the same sum-type-as-struct shape model.Recurrence itself uses.
type Recurrence struct {
	Kind        string `json:"kind"`
	EveryNHours int    `json:"everyNHours,omitempty"`
	TimeOfDay   string `json:"timeOfDay,omitempty"`
	Weekday     string `json:"weekday,omitempty"`
	DayOfMonth  int    `json:"dayOfMonth,omitempty"`
}

var weekdayNames = [...]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

func formatTimeOfDay(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

func parseTimeOfDay(text string) (int, error) {
	hh, mm, ok := strings.Cut(text, ":")
	if !ok {
		return 0, validationErrorf("timeOfDay: want HH:MM, got %q", text)
	}
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, validationErrorf("timeOfDay: want HH:MM, got %q", text)
	}
	return h*60 + m, nil
}

func parseWeekday(name string) (time.Weekday, error) {
	for i, n := range weekdayNames {
		if n == strings.ToLower(strings.TrimSpace(name)) {
			return time.Weekday(i), nil
		}
	}
	return 0, validationErrorf("weekday: unknown day %q", name)
}

func recurrenceFrom(r model.Recurrence) Recurrence {
	out := Recurrence{Kind: string(r.Kind)}
	switch r.Kind {
	case model.RecurrenceEveryNHours:
		out.EveryNHours = r.EveryNHours
	case model.RecurrenceDaily:
		out.TimeOfDay = formatTimeOfDay(r.TimeOfDay)
	case model.RecurrenceWeekly:
		out.TimeOfDay = formatTimeOfDay(r.TimeOfDay)
		out.Weekday = weekdayNames[r.Weekday]
	case model.RecurrenceMonthly:
		out.TimeOfDay = formatTimeOfDay(r.TimeOfDay)
		out.DayOfMonth = r.DayOfMonth
	}
	return out
}

// parseRecurrence turns the wire shape back into model.Recurrence,
// rejecting anything Validate would -- caught here, before the store is
// touched, the same as parseInterval used to for the plain-duration
// version this replaces.
func parseRecurrence(req Recurrence) (model.Recurrence, error) {
	r := model.Recurrence{Kind: model.RecurrenceKind(req.Kind), EveryNHours: req.EveryNHours, DayOfMonth: req.DayOfMonth}
	if req.TimeOfDay != "" {
		minutes, err := parseTimeOfDay(req.TimeOfDay)
		if err != nil {
			return model.Recurrence{}, err
		}
		r.TimeOfDay = minutes
	}
	if req.Weekday != "" {
		weekday, err := parseWeekday(req.Weekday)
		if err != nil {
			return model.Recurrence{}, err
		}
		r.Weekday = weekday
	}
	if err := r.Validate(); err != nil {
		return model.Recurrence{}, validationErrorf("recurrence: %v", err)
	}
	return r, nil
}

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
	// Reads and Capabilities mirror Task's own fields (bwsalmon/agents#464):
	// every task this schedule files carries them, the same as one filed
	// by hand through CreateTaskRequest.
	Reads        []string   `json:"reads,omitempty"`
	Capabilities []string   `json:"capabilities"`
	Recurrence   Recurrence `json:"recurrence"`
	Enabled      bool       `json:"enabled"`
	NextRunAt    time.Time  `json:"nextRunAt"`
	LastRunAt    *time.Time `json:"lastRunAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}

func scheduleFrom(s model.ScheduledTask) Schedule {
	out := Schedule{
		ID:           s.ID,
		Title:        s.Title,
		Description:  s.Body,
		Repo:         s.Target.String(),
		Base:         s.Base,
		AutoMerge:    s.AutoMerge,
		Capabilities: []string{},
		Recurrence:   recurrenceFrom(s.Recurrence),
		Enabled:      s.Enabled,
		NextRunAt:    s.NextRunAt,
		LastRunAt:    s.LastRunAt,
		CreatedAt:    s.CreatedAt,
	}
	for _, r := range s.Reads {
		out.Reads = append(out.Reads, r.String())
	}
	for _, g := range s.Grants {
		out.Capabilities = append(out.Capabilities, g.Capability)
	}
	return out
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
// own shape, minus DependsOn and Approved: a schedule fires with no
// reviewer in the loop each time (see fireScheduledTask's own doc comment
// on why it needs no approved flag), and a one-shot depends-on link makes
// no sense against a task this schedule refiles indefinitely, so those
// two task-only concerns have no home here. Repo and Recurrence are
// required: a schedule with no write target or no cadence cannot ever
// fire.
type CreateScheduleRequest struct {
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Repo         string     `json:"repo"`
	Base         string     `json:"base"`
	AutoMerge    bool       `json:"autoMerge"`
	Capabilities []string   `json:"capabilities"`
	Reads        []string   `json:"reads"`
	Recurrence   Recurrence `json:"recurrence"`
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
	recurrence, err := parseRecurrence(req.Recurrence)
	if err != nil {
		return Schedule{}, err
	}
	grants, err := c.grantsFor(req.Capabilities)
	if err != nil {
		return Schedule{}, err
	}
	reads, err := parseReads(req.Reads)
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
		ID:         id,
		Title:      req.Title,
		Body:       req.Description,
		Target:     target,
		Base:       req.Base,
		AutoMerge:  req.AutoMerge,
		Reads:      reads,
		Grants:     grants,
		Recurrence: recurrence,
		Enabled:    enabled,
		NextRunAt:  now,
		CreatedAt:  now,
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
// picks up rather than firing every occurrence that elapsed while paused.
type UpdateScheduleRequest struct {
	Title        *string     `json:"title,omitempty"`
	Description  *string     `json:"description,omitempty"`
	Repo         *string     `json:"repo,omitempty"`
	Base         *string     `json:"base,omitempty"`
	AutoMerge    *bool       `json:"autoMerge,omitempty"`
	Capabilities *[]string   `json:"capabilities,omitempty"`
	Reads        *[]string   `json:"reads,omitempty"`
	Recurrence   *Recurrence `json:"recurrence,omitempty"`
	Enabled      *bool       `json:"enabled,omitempty"`
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
	var recurrence *model.Recurrence
	if req.Recurrence != nil {
		r, err := parseRecurrence(*req.Recurrence)
		if err != nil {
			return Schedule{}, err
		}
		recurrence = &r
	}
	var grants []model.Grant
	if req.Capabilities != nil {
		var err error
		grants, err = c.grantsFor(*req.Capabilities)
		if err != nil {
			return Schedule{}, err
		}
	}
	var reads []model.RepoRef
	if req.Reads != nil {
		var err error
		reads, err = parseReads(*req.Reads)
		if err != nil {
			return Schedule{}, err
		}
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
		if req.Capabilities != nil {
			s.Grants = grants
		}
		if req.Reads != nil {
			s.Reads = reads
		}
		if recurrence != nil {
			s.Recurrence = *recurrence
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
