package model

import (
	"fmt"
	"time"
)

// SuitePrincipal is the automation actor every task a task suite run
// files is attributed to -- its own actor, distinct from scheduler
// (schedule.go) and QualificationPrincipal (qualification.go), so a
// task's own conversation and origin make plain which mechanism filed
// it.
var SuitePrincipal = Principal{Kind: PrincipalAutomation, ID: "task-suite"}

// TaskSuiteItem is one TaskTemplate (bwsalmon/agents#516) a suite runs,
// referenced by id rather than copied -- content lives on the template,
// and a run resolves it fresh every time a pass fires, the same "not a
// stale copy" discipline fireTaskSchedule and CreateQualificationRun
// already hold TemplateID to.
type TaskSuiteItem struct {
	TemplateID string
}

// TaskSuiteMode decides when a run's passes stop.
type TaskSuiteMode string

const (
	// TaskSuiteCount runs the suite's items exactly Count times, whatever
	// each pass produces.
	TaskSuiteCount TaskSuiteMode = "count"
	// TaskSuiteUntilClean re-runs the suite's items pass after pass until
	// one pass opens no pull request and proposes no follow-up task --
	// bwsalmon/agents#642's own "run the tasks until they generate no
	// issues or repo changes" -- or until MaxPasses passes have run
	// without ever reaching one, whichever comes first.
	TaskSuiteUntilClean TaskSuiteMode = "until_clean"
)

func (m TaskSuiteMode) Valid() bool {
	switch m {
	case TaskSuiteCount, TaskSuiteUntilClean:
		return true
	}
	return false
}

// TaskSuite is a saved combination of task templates plus how to run them
// against a repo and branch (bwsalmon/agents#642) -- a template a human
// builds once, then runs any number of times, the same
// declared/instantiated split every other "run this against a repo"
// mechanism here already has (TaskTemplate -> Schedule/QualificationRun,
// here TaskSuite -> TaskSuiteRun).
//
// RequireApproval and AutoMerge are QualificationPlan's own two switches
// (bwsalmon/agents#518), read the same way here: RequireApproval leaves
// every pass's tasks unapproved for a human to approve through the
// ordinary task-approval flow -- there is no suite-specific approval
// endpoint, since a suite's tasks are just tasks -- and AutoMerge is
// stamped onto every task a run files so its pull request lands the
// moment it reads clean, with no separate review of the change itself.
// The issue's own "by default they should auto queue and auto merge" is
// RequireApproval false and AutoMerge true, CreateSuite's own default.
type TaskSuite struct {
	ID              string
	Name            string
	Items           []TaskSuiteItem
	Mode            TaskSuiteMode
	Count           int // TaskSuiteCount only; always >= 1
	MaxPasses       int // TaskSuiteUntilClean only; always >= 1
	RequireApproval bool
	AutoMerge       bool
	CreatedAt       time.Time
}

// Validate reports whether a suite's own fields make sense to run --
// checked once here, the same as QualificationPlan.Validate, rather than
// left for CreateTaskSuiteRun to discover mid-way through filing a pass.
// It does not check that any item's TemplateID names a template that
// still exists -- that is a store lookup, left to ui.Client.CreateSuite/
// UpdateSuite and to CreateTaskSuiteRun, which each resolve every item
// fresh at the moment they need its content.
func (s TaskSuite) Validate() error {
	if len(s.Items) == 0 {
		return fmt.Errorf("a task suite needs at least one task template")
	}
	for _, it := range s.Items {
		if it.TemplateID == "" {
			return fmt.Errorf("a task suite item needs a template")
		}
	}
	switch s.Mode {
	case TaskSuiteCount:
		if s.Count < 1 {
			return fmt.Errorf("count must be at least 1")
		}
	case TaskSuiteUntilClean:
		if s.MaxPasses < 1 {
			return fmt.Errorf("max passes must be at least 1")
		}
	default:
		return fmt.Errorf("unknown task suite mode %q", s.Mode)
	}
	return nil
}

// TaskSuiteRunStatus is a run's own progress -- unlike
// QualificationRunStatus, not purely derived from its tasks on every
// read, since deciding whether to stop or fire another pass is a
// judgement SyncTaskSuites itself makes once (was the last pass clean?
// has Count or MaxPasses been reached?) and records here.
type TaskSuiteRunStatus string

const (
	// TaskSuiteRunActive means a pass is still in flight, or the next one
	// is waiting on this cycle's reconciler to fire it.
	TaskSuiteRunActive TaskSuiteRunStatus = "active"
	// TaskSuiteRunSucceeded means the run stopped on its own terms:
	// TaskSuiteCount finished every pass, or TaskSuiteUntilClean reached
	// a pass that opened no pull request and proposed no follow-up task.
	TaskSuiteRunSucceeded TaskSuiteRunStatus = "succeeded"
	// TaskSuiteRunFailed means a pass's task failed or closed without
	// completing, or (TaskSuiteUntilClean only) MaxPasses ran out with no
	// pass ever reading clean.
	TaskSuiteRunFailed TaskSuiteRunStatus = "failed"
)

// TaskSuiteTaskStatus is one task instance a run's pass instantiated,
// with enough of its current progress to tell whether that pass has
// finished and, once it has, whether it was clean --
// QualificationTaskStatus's own shape, with PassNumber in place of
// InstanceIndex/Repeat since a suite run repeats whole passes rather
// than individual items.
type TaskSuiteTaskStatus struct {
	TaskID       string
	TemplateID   string
	TemplateName string
	PassNumber   int
	Approved     bool
	State        State
	// OpenedPullRequest is this task's own LinkFixes -- true once its run
	// pushed a branch and grain opened (or found) a pull request for it.
	OpenedPullRequest bool
	// Proposed is true if this task's run proposed at least one follow-up
	// task (a LinkProposedBy pointing back at it) -- the other half of
	// what a TaskSuiteUntilClean pass's own "clean" reading is built
	// from, alongside OpenedPullRequest.
	Proposed bool
}

// TaskSuiteRun is one run of a suite against a repo and branch --
// bwsalmon/agents#642's own "run the template against a repo and
// branch." Every field a run's own behaviour depends on (Items, Mode,
// Count, MaxPasses, RequireApproval, AutoMerge) is snapshotted from the
// suite at creation, not read live from it, so editing a suite never
// changes how a run already in flight behaves.
type TaskSuiteRun struct {
	ID        int64
	SuiteID   string
	SuiteName string // snapshot, QualificationTaskStatus.TemplateName's own reasoning
	// ScheduleID is the Schedule (schedule.go) whose firing started this
	// run, or empty for a run a human started by hand. It is both how a
	// run says where it came from and, through
	// Store.HasActiveRunForSchedule, the idempotency check that keeps a
	// schedule from starting a second run on top of one still in flight
	// -- the firing tag every task-filing schedule already carries, in
	// the form a run can wear.
	ScheduleID string
	Target     RepoRef
	// Base is the branch every task this run files targets, and (through
	// AutoMerge) the branch every one of them lands back on --
	// bwsalmon/agents#642's own "tasks created from the task suite should
	// stack against the source branch": every task's pull request bases
	// off Base, and once it merges the next task to run (the next item in
	// this pass, or the first item of the next pass) sees that change
	// simply by cloning Base's own current tip -- the same stacking trick
	// fileFixTask already uses with one task's own branch as Base,
	// applied here with a fixed Base every task in the run shares
	// instead.
	Base            string
	Items           []TaskSuiteItem
	Mode            TaskSuiteMode
	Count           int
	MaxPasses       int
	RequireApproval bool
	AutoMerge       bool
	Status          TaskSuiteRunStatus
	LastError       string
	CreatedAt       time.Time
	CompletedAt     *time.Time
	// Tasks is every pass's own instances, oldest first -- PassNumber
	// groups them, the same way QualificationRun.Tasks groups by
	// InstanceIndex.
	Tasks []TaskSuiteTaskStatus
}

// CurrentPass returns run's own highest PassNumber, or 0 if it has no
// tasks yet -- unreachable once created, since CreateTaskSuiteRun always
// files a first pass, but a safe zero value all the same.
func (r TaskSuiteRun) CurrentPass() int {
	max := 0
	for _, t := range r.Tasks {
		if t.PassNumber > max {
			max = t.PassNumber
		}
	}
	return max
}

// PassTasks returns the tasks belonging to pass n only.
func (r TaskSuiteRun) PassTasks(n int) []TaskSuiteTaskStatus {
	var out []TaskSuiteTaskStatus
	for _, t := range r.Tasks {
		if t.PassNumber == n {
			out = append(out, t)
		}
	}
	return out
}

// PassOutcome is what SyncTaskSuites reads off a pass's own tasks to
// decide what to do next.
type PassOutcome int

const (
	// PassPending means at least one task in the pass has not yet reached
	// a terminal state -- includes a task RequireApproval left
	// unapproved, since an unapproved task never leaves StateProposed on
	// its own.
	PassPending PassOutcome = iota
	// PassFailed means every task reached a terminal state, and at least
	// one of them failed or closed without completing.
	PassFailed
	// PassClean means every task completed, and none opened a pull
	// request or proposed a follow-up task.
	PassClean
	// PassChanged means every task completed, but at least one opened a
	// pull request or proposed a follow-up task.
	PassChanged
)

// OutcomeOfPass reduces one pass's own task instances to what
// SyncTaskSuites needs to decide whether to stop or fire another pass.
// A failure found anywhere outranks everything else, the same
// precedence QualificationStatus gives anyFailed over allCompleted --
// a straggler elsewhere does not hide a failure already known, and
// nothing here waits for every instance to settle before reporting one.
func OutcomeOfPass(tasks []TaskSuiteTaskStatus) PassOutcome {
	changed, anyFailed, anyPending := false, false, false
	for _, t := range tasks {
		switch t.State {
		case StateCompleted:
			if t.OpenedPullRequest || t.Proposed {
				changed = true
			}
		case StateFailed, StateClosed:
			anyFailed = true
		default:
			anyPending = true
		}
	}
	switch {
	case anyFailed:
		// Ahead of anyPending deliberately, and the reason this loop
		// records a straggler rather than returning on the first one it
		// sees: a pass fires every item at once, so a failure in one
		// item and a second item still running is the ordinary shape of
		// a pass going wrong. Reporting PassPending there would leave
		// the run active -- firing further passes, in TaskSuiteCount
		// mode -- on a pass already known to have failed.
		return PassFailed
	case anyPending:
		return PassPending
	case changed:
		return PassChanged
	}
	return PassClean
}
