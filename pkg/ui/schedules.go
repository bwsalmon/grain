package ui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// scheduleNotFoundError marks a schedule ID with no schedule behind it --
// writeClientError maps it to a 404, the same as NotFoundError does for
// a task id. It is its own type rather than NotFoundError itself so the
// message a caller sees names a schedule, not a task.
type scheduleNotFoundError struct{ ID string }

func (e *scheduleNotFoundError) Error() string { return "no schedule " + e.ID }

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
	Reads []string `json:"reads,omitempty"`
	// TemplateID and TemplateName are set when this schedule fires from
	// a Template (bwsalmon/agents#516) rather than its own inline
	// content -- Title/Description/AutoMerge/Reads/Capabilities above
	// still reflect that template's content (kept in sync each time the
	// schedule fires, or is itself saved), so a UI that only wants to
	// render a summary needs no separate template lookup; TemplateName
	// is only for a UI that wants to say which template, e.g. to link
	// to it. Repo and Base are never among them: a template carries no
	// target of its own (model.Template's own doc comment on why), so
	// they are always this schedule's own fields, template-backed or
	// not.
	TemplateID   string `json:"templateId,omitempty"`
	TemplateName string `json:"templateName,omitempty"`
	// SuiteID and SuiteName are set when this schedule runs a suite on
	// its cadence rather than filing a single task -- Title is the
	// suite's own name in that case (model.Schedule.SuiteID's own doc
	// comment on the display cache), and
	// Description/AutoMerge/Reads/Capabilities mean nothing, since a
	// suite run takes all of those from the suite and its templates.
	// Repo and Base still decide what the run targets, exactly as they
	// do for a task.
	SuiteID      string     `json:"suiteId,omitempty"`
	SuiteName    string     `json:"suiteName,omitempty"`
	Capabilities []string   `json:"capabilities"`
	Recurrence   Recurrence `json:"recurrence"`
	Enabled      bool       `json:"enabled"`
	NextRunAt    time.Time  `json:"nextRunAt"`
	LastRunAt    *time.Time `json:"lastRunAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// scheduleFrom renders s -- templateName and suiteName are the
// schedule's own TemplateID/SuiteID resolved to a name by the caller
// (fetched once per list rather than once per row's own doc comment on
// why these are parameters rather than lookups this function does
// itself), empty when the id in question is nil or the caller has no
// name to offer.
func scheduleFrom(s model.Schedule, templateName, suiteName string) Schedule {
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
	if s.TemplateID != nil {
		out.TemplateID = *s.TemplateID
		out.TemplateName = templateName
	}
	if s.SuiteID != nil {
		out.SuiteID = *s.SuiteID
		out.SuiteName = suiteName
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
	list, err := c.Store.ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	// Templates and suites are looked up once per distinct id rather than
	// once per schedule -- more than one schedule pointing at the same
	// template or suite (the whole point of TemplateID and SuiteID
	// existing) would otherwise repeat the same store call.
	templateNames, suiteNames := map[string]string{}, map[string]string{}
	out := make([]Schedule, 0, len(list))
	for _, s := range list {
		var templateName, suiteName string
		if s.TemplateID != nil {
			id := *s.TemplateID
			if cached, ok := templateNames[id]; ok {
				templateName = cached
			} else if t, err := c.Store.GetTemplate(ctx, id); err == nil && t != nil {
				templateName = t.Name
				templateNames[id] = templateName
			}
		}
		if s.SuiteID != nil {
			id := *s.SuiteID
			if cached, ok := suiteNames[id]; ok {
				suiteName = cached
			} else if suite, err := c.Store.GetSuite(ctx, id); err == nil && suite != nil {
				suiteName = suite.Name
				suiteNames[id] = suiteName
			}
		}
		out = append(out, scheduleFrom(s, templateName, suiteName))
	}
	return out, nil
}

// CreateScheduleRequest is a new schedule's fields -- CreateTaskRequest's
// own shape, minus DependsOn and Approved: a schedule fires with no
// reviewer in the loop each time (see fireTaskSchedule's own doc comment
// on why it needs no approved flag), and a one-shot depends-on link makes
// no sense against a task this schedule refiles indefinitely, so those
// two task-only concerns have no home here. Recurrence is always
// required: a schedule with no cadence cannot ever fire.
//
// Repo and Base are always this request's own fields, template-backed
// or not: a template carries no target of its own (model.Template's own
// doc comment on why), so which repo and branch this schedule fires
// against is decided here, at the point of use, exactly as bwsalmon/
// agents#516 asks. Repo is always required.
//
// TemplateID, given, takes over
// Title/Description/AutoMerge/Capabilities/Reads entirely
// (bwsalmon/agents#516) -- CreateSchedule copies the named Template's
// own content in and ignores those five fields on this request
// outright, rather than letting a caller mix a template with its own
// per-field overrides, a combination this API deliberately does not
// support (see CreateSchedule's own doc comment). Left blank, Title is
// also required, exactly as it always was.
//
// SuiteID, given, makes this a schedule that runs a whole suite on its
// cadence rather than filing one task: the suite decides everything
// about what runs (its items, how many passes, approval, auto-merge),
// so Title/Description/AutoMerge/Capabilities/Reads are ignored the
// same way a TemplateID makes them ignored, and TemplateID itself may
// not be given alongside it. Base is required for a suite-backed
// schedule, matching CreateSuiteRunRequest's own rule: a suite run
// stacks its tasks against one named branch and has no default to fall
// back to.
type CreateScheduleRequest struct {
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Repo         string     `json:"repo"`
	Base         string     `json:"base"`
	AutoMerge    bool       `json:"autoMerge"`
	Capabilities []string   `json:"capabilities"`
	Reads        []string   `json:"reads"`
	TemplateID   string     `json:"templateId"`
	SuiteID      string     `json:"suiteId"`
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
//
// Repo and Base always come from req, template-backed or not
// (CreateScheduleRequest's own doc comment on why); a template-backed
// schedule (req.TemplateID set) needs no Title of its own:
// scheduleContentFromTemplate below reads it off the template instead,
// the same field fireTaskSchedule itself re-reads from the template on
// every firing rather than trusting whatever this call snapshots.
//
// A suite-backed schedule (req.SuiteID set) needs no Title either, for
// the same reason one field further out: the suite's own name stands in
// as this schedule's display title, re-read by fireSuiteSchedule on every
// firing rather than trusted from here.
func (c *Client) CreateSchedule(ctx context.Context, req CreateScheduleRequest) (Schedule, error) {
	recurrence, err := parseRecurrence(req.Recurrence)
	if err != nil {
		return Schedule{}, err
	}
	if strings.TrimSpace(req.Repo) == "" {
		return Schedule{}, validationErrorf("repo is required")
	}
	target, err := model.ParseRepo(req.Repo)
	if err != nil {
		return Schedule{}, &ValidationError{err: err}
	}

	var sched model.Schedule
	var templateName, suiteName string
	if strings.TrimSpace(req.SuiteID) != "" {
		if strings.TrimSpace(req.TemplateID) != "" {
			return Schedule{}, validationErrorf(
				"a schedule fires either a suite or a task, not both: give suiteId or templateId, not both")
		}
		if strings.TrimSpace(req.Base) == "" {
			return Schedule{}, validationErrorf("base is required: a suite run has no default branch to fall back to")
		}
		suite, err := c.Store.GetSuite(ctx, req.SuiteID)
		if err != nil {
			return Schedule{}, err
		}
		if suite == nil {
			return Schedule{}, validationErrorf("unknown suite %s", req.SuiteID)
		}
		sched = model.Schedule{Title: suite.Name, SuiteID: &suite.ID}
		suiteName = suite.Name
	} else if strings.TrimSpace(req.TemplateID) != "" {
		tmpl, err := c.Store.GetTemplate(ctx, req.TemplateID)
		if err != nil {
			return Schedule{}, err
		}
		if tmpl == nil {
			return Schedule{}, validationErrorf("unknown template %s", req.TemplateID)
		}
		sched = scheduleContentFromTemplate(*tmpl)
		templateName = tmpl.Name
	} else {
		if strings.TrimSpace(req.Title) == "" {
			return Schedule{}, validationErrorf("title is required")
		}
		grants, err := c.grantsFor(req.Capabilities, model.GrantByLabel)
		if err != nil {
			return Schedule{}, err
		}
		reads, err := parseReads(req.Reads)
		if err != nil {
			return Schedule{}, err
		}
		sched = model.Schedule{
			Title:     req.Title,
			Body:      req.Description,
			AutoMerge: req.AutoMerge,
			Reads:     reads,
			Grants:    grants,
		}
	}
	sched.Target = target
	sched.Base = req.Base

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	id, err := c.Store.NewScheduleID(ctx)
	if err != nil {
		return Schedule{}, err
	}
	now := c.now()
	sched.ID = id
	sched.Recurrence = recurrence
	sched.Enabled = enabled
	sched.NextRunAt = now
	sched.CreatedAt = now
	if err := c.Store.PutSchedule(ctx, sched); err != nil {
		return Schedule{}, err
	}
	return scheduleFrom(sched, templateName, suiteName), nil
}

// scheduleContentFromTemplate is the content half of a Schedule that a
// template actually carries (everything but identity, recurrence,
// timing, and Target/Base -- a template has no target of its own,
// model.Template's own doc comment on why) copied from t --
// CreateSchedule's and UpdateSchedule's shared way of attaching a
// schedule to a template, and also what fireTaskSchedule itself calls
// each time a template-backed schedule fires, so a schedule's own
// inline copy never drifts from the template it names for longer than
// one firing. Callers still need to set Target and Base themselves.
func scheduleContentFromTemplate(t model.Template) model.Schedule {
	return model.Schedule{
		Title:      t.Title,
		Body:       t.Body,
		AutoMerge:  t.AutoMerge,
		Reads:      t.Reads,
		Grants:     t.Grants,
		TemplateID: &t.ID,
	}
}

// UpdateScheduleRequest is a schedule's editable fields -- nil means
// "leave this one alone", UpdateTaskRequest's own convention. Enabled is
// the pause/resume toggle: setting it false stops this schedule from
// ever coming due without losing its NextRunAt, so resuming it later
// picks up rather than firing every occurrence that elapsed while paused.
//
// Repo and Base follow the plain nil-means-leave-alone rule regardless
// of TemplateID: a template carries no target of its own
// (model.Template's own doc comment on why), so attaching one never
// touches this schedule's own Target/Base, and either can still be
// edited on the same request that attaches or detaches a template.
//
// TemplateID follows the same nil-means-leave-alone rule, with two
// non-nil cases of its own (bwsalmon/agents#516): a non-empty id
// attaches this schedule to that template, overwriting Title/
// Description/AutoMerge/Reads/Capabilities from it and ignoring those
// same five fields on this request, exactly as CreateScheduleRequest's
// own doc comment describes; an empty string detaches it, leaving the
// schedule's current content (most recently synced from whatever
// template it pointed at) in place as a now-independent copy, editable
// through those same five fields from then on.
//
// SuiteID repoints a suite-backed schedule at a different suite, and is
// the one field here that cannot change what *kind* of schedule this is
// (model.Schedule.SuiteID's own doc comment on why): given on a
// schedule that fires a task, or given as "" on one that runs a suite,
// it is refused rather than quietly converting a schedule into
// something with no content to fire. Everything a suite-backed schedule
// still decides for itself -- Repo, Base, Recurrence, Enabled -- is
// editable here exactly as it is for any other schedule;
// Title/Description/AutoMerge/Reads/Capabilities are ignored, the same
// as they are while a TemplateID is set.
type UpdateScheduleRequest struct {
	Title        *string     `json:"title,omitempty"`
	Description  *string     `json:"description,omitempty"`
	Repo         *string     `json:"repo,omitempty"`
	Base         *string     `json:"base,omitempty"`
	AutoMerge    *bool       `json:"autoMerge,omitempty"`
	Capabilities *[]string   `json:"capabilities,omitempty"`
	Reads        *[]string   `json:"reads,omitempty"`
	TemplateID   *string     `json:"templateId,omitempty"`
	SuiteID      *string     `json:"suiteId,omitempty"`
	Recurrence   *Recurrence `json:"recurrence,omitempty"`
	Enabled      *bool       `json:"enabled,omitempty"`
}

// UpdateSchedule edits a schedule's fields in place -- its NextRunAt and
// LastRunAt are left exactly as they are; only fireTaskSchedule ever
// changes those.
func (c *Client) UpdateSchedule(ctx context.Context, id string, req UpdateScheduleRequest) (Schedule, error) {
	existing, err := c.Store.GetSchedule(ctx, id)
	if err != nil {
		return Schedule{}, err
	}
	if existing == nil {
		return Schedule{}, &scheduleNotFoundError{ID: id}
	}
	suiteBacked := existing.SuiteID != nil

	// repointing is non-nil only when this suite-backed schedule is
	// being moved to a different suite -- the one thing req.SuiteID can
	// do, since what a schedule fires is fixed when it is created
	// (UpdateScheduleRequest's own doc comment on why).
	var repointing *model.Suite
	if req.SuiteID != nil {
		wanted := strings.TrimSpace(*req.SuiteID)
		switch {
		case !suiteBacked && wanted != "":
			return Schedule{}, validationErrorf(
				"this schedule files a task, not a suite: create a suite-backed schedule instead")
		case suiteBacked && wanted == "":
			return Schedule{}, validationErrorf(
				"a suite-backed schedule cannot be detached from its suite: delete it, or point it at another suite")
		case suiteBacked:
			suite, err := c.Store.GetSuite(ctx, wanted)
			if err != nil {
				return Schedule{}, err
			}
			if suite == nil {
				return Schedule{}, validationErrorf("unknown suite %s", wanted)
			}
			repointing = suite
		}
	}
	if suiteBacked {
		if req.TemplateID != nil && strings.TrimSpace(*req.TemplateID) != "" {
			return Schedule{}, validationErrorf(
				"this schedule runs a suite, so it fires no template of its own")
		}
		if req.Base != nil && strings.TrimSpace(*req.Base) == "" {
			return Schedule{}, validationErrorf("base is required: a suite run has no default branch to fall back to")
		}
	}

	// attaching is non-nil only when req.TemplateID names a template to
	// attach to (the non-empty case) -- content below is populated from
	// it, and every other content field on req is ignored, the same
	// "template wins outright" rule CreateSchedule applies.
	var attaching *model.Template
	if req.TemplateID != nil && strings.TrimSpace(*req.TemplateID) != "" {
		tmpl, err := c.Store.GetTemplate(ctx, *req.TemplateID)
		if err != nil {
			return Schedule{}, err
		}
		if tmpl == nil {
			return Schedule{}, validationErrorf("unknown template %s", *req.TemplateID)
		}
		attaching = tmpl
	}

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
	var grants []model.Grant
	var reads []model.RepoRef
	if attaching == nil && !suiteBacked {
		if req.Title != nil && strings.TrimSpace(*req.Title) == "" {
			return Schedule{}, validationErrorf("title cannot be empty")
		}
		if req.Capabilities != nil {
			var err error
			grants, err = c.grantsFor(*req.Capabilities, model.GrantByLabel)
			if err != nil {
				return Schedule{}, err
			}
		}
		if req.Reads != nil {
			var err error
			reads, err = parseReads(*req.Reads)
			if err != nil {
				return Schedule{}, err
			}
		}
	}
	var recurrence *model.Recurrence
	if req.Recurrence != nil {
		r, err := parseRecurrence(*req.Recurrence)
		if err != nil {
			return Schedule{}, err
		}
		recurrence = &r
	}

	if err := c.Store.UpdateSchedule(ctx, id, func(s *model.Schedule) error {
		if s.SuiteID != nil {
			// A suite-backed schedule carries no content of its own to
			// edit -- the suite does. Only which suite it runs can change
			// here, and only to another suite.
			if repointing != nil {
				s.SuiteID, s.Title = &repointing.ID, repointing.Name
			}
		} else if attaching != nil {
			content := scheduleContentFromTemplate(*attaching)
			s.Title, s.Body, s.AutoMerge, s.Reads, s.Grants, s.TemplateID =
				content.Title, content.Body, content.AutoMerge, content.Reads, content.Grants, content.TemplateID
		} else {
			// req.TemplateID != nil here means the empty-string case:
			// detach, keeping whatever content is already on the row and
			// still applying any of the individual field edits below in
			// the same request.
			if req.TemplateID != nil {
				s.TemplateID = nil
			}
			if req.Title != nil {
				s.Title = *req.Title
			}
			if req.Description != nil {
				s.Body = *req.Description
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
		}
		// Target and Base are always applied on their own, template
		// attached or not (UpdateScheduleRequest's own doc comment on
		// why): a template carries no target of its own.
		if target != nil {
			s.Target = *target
		}
		if req.Base != nil {
			s.Base = *req.Base
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

	updated, err := c.Store.GetSchedule(ctx, id)
	if err != nil {
		return Schedule{}, err
	}
	var templateName, suiteName string
	if attaching != nil {
		templateName = attaching.Name
	} else if updated.TemplateID != nil {
		if t, err := c.Store.GetTemplate(ctx, *updated.TemplateID); err == nil && t != nil {
			templateName = t.Name
		}
	}
	if repointing != nil {
		suiteName = repointing.Name
	} else if updated.SuiteID != nil {
		if suite, err := c.Store.GetSuite(ctx, *updated.SuiteID); err == nil && suite != nil {
			suiteName = suite.Name
		}
	}
	return scheduleFrom(*updated, templateName, suiteName), nil
}

// DeleteSchedule removes a schedule outright -- Store.DeleteSchedule's
// own doc comment gives the reasoning: unlike a task, a schedule is only
// ever a standing declaration, so there is no history on the row itself
// worth keeping once a human no longer wants it.
func (c *Client) DeleteSchedule(ctx context.Context, id string) error {
	existing, err := c.Store.GetSchedule(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return &scheduleNotFoundError{ID: id}
	}
	return c.Store.DeleteSchedule(ctx, id)
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
