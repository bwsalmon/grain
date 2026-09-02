package model_test

// Task suites' own model tests (bwsalmon/agents#642) -- TaskSuite.
// Validate and the pass reductions SyncTaskSuites decides from, the
// same discipline qualification_test.go already holds
// QualificationPlan.Validate and QualificationStatus to. Nothing here
// touches a store: every function under test is a pure reduction over
// values, which is exactly why it is worth pinning down here rather
// than only through the reconciler's own end-to-end tests.

import (
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
)

func countSuite() model.TaskSuite {
	return model.TaskSuite{
		Name:  "smoke",
		Items: []model.TaskSuiteItem{{TemplateID: "template-smoke"}},
		Mode:  model.TaskSuiteCount, Count: 3,
	}
}

func untilCleanSuite() model.TaskSuite {
	return model.TaskSuite{
		Name:  "sweep",
		Items: []model.TaskSuiteItem{{TemplateID: "template-sweep"}},
		Mode:  model.TaskSuiteUntilClean, MaxPasses: 5,
	}
}

func TestTaskSuiteModeValid(t *testing.T) {
	for _, m := range []model.TaskSuiteMode{model.TaskSuiteCount, model.TaskSuiteUntilClean} {
		if !m.Valid() {
			t.Errorf("%q.Valid() = false, want true", m)
		}
	}
	for _, m := range []model.TaskSuiteMode{"", "COUNT", "untilclean", "forever"} {
		if m.Valid() {
			t.Errorf("%q.Valid() = true, want false", m)
		}
	}
}

func TestTaskSuiteValidateAcceptsBothModes(t *testing.T) {
	if err := countSuite().Validate(); err != nil {
		t.Errorf("count suite: %v", err)
	}
	if err := untilCleanSuite().Validate(); err != nil {
		t.Errorf("until_clean suite: %v", err)
	}
}

func TestTaskSuiteValidateRejectsNoItems(t *testing.T) {
	s := countSuite()
	s.Items = nil
	if err := s.Validate(); err == nil {
		t.Fatal("a suite with no templates validated, want an error")
	}
}

func TestTaskSuiteValidateRejectsABlankTemplateID(t *testing.T) {
	s := countSuite()
	s.Items = []model.TaskSuiteItem{{TemplateID: "template-smoke"}, {TemplateID: ""}}
	if err := s.Validate(); err == nil {
		t.Fatal("a suite item with no template validated, want an error")
	}
}

func TestTaskSuiteValidateRejectsCountBelowOne(t *testing.T) {
	s := countSuite()
	for _, n := range []int{0, -1} {
		s.Count = n
		if err := s.Validate(); err == nil {
			t.Errorf("count %d validated, want an error", n)
		}
	}
}

func TestTaskSuiteValidateRejectsMaxPassesBelowOne(t *testing.T) {
	s := untilCleanSuite()
	for _, n := range []int{0, -1} {
		s.MaxPasses = n
		if err := s.Validate(); err == nil {
			t.Errorf("max passes %d validated, want an error", n)
		}
	}
}

func TestTaskSuiteValidateIgnoresTheOtherModesBound(t *testing.T) {
	// Count is meaningless to an until_clean suite and MaxPasses to a
	// count one -- ui.CreateSuite leaves whichever the form did not ask
	// for at zero, so Validate must not read it.
	s := untilCleanSuite()
	s.Count = 0
	if err := s.Validate(); err != nil {
		t.Errorf("until_clean suite with Count 0: %v", err)
	}
	c := countSuite()
	c.MaxPasses = 0
	if err := c.Validate(); err != nil {
		t.Errorf("count suite with MaxPasses 0: %v", err)
	}
}

func TestTaskSuiteValidateRejectsAnUnknownMode(t *testing.T) {
	s := countSuite()
	s.Mode = "forever"
	if err := s.Validate(); err == nil {
		t.Fatal("an unknown mode validated, want an error")
	}
}

// passTask is one instance in a pass, in whatever state a case needs.
func passTask(pass int, state model.State) model.TaskSuiteTaskStatus {
	return model.TaskSuiteTaskStatus{
		TaskID: "task-" + string(state), PassNumber: pass, State: state,
	}
}

func TestCurrentPassIsTheHighestPassNumber(t *testing.T) {
	run := model.TaskSuiteRun{Tasks: []model.TaskSuiteTaskStatus{
		passTask(1, model.StateCompleted), passTask(2, model.StateCompleted), passTask(3, model.StateRunning),
	}}
	if got := run.CurrentPass(); got != 3 {
		t.Fatalf("CurrentPass() = %d, want 3", got)
	}
}

func TestCurrentPassIsZeroBeforeAnyPassHasFired(t *testing.T) {
	if got := (model.TaskSuiteRun{}).CurrentPass(); got != 0 {
		t.Fatalf("CurrentPass() = %d on a run with no tasks, want 0", got)
	}
}

func TestPassTasksReturnsOnlyThatPass(t *testing.T) {
	a := model.TaskSuiteTaskStatus{TaskID: "task-1", PassNumber: 1, State: model.StateCompleted}
	b := model.TaskSuiteTaskStatus{TaskID: "task-2", PassNumber: 2, State: model.StateCompleted}
	c := model.TaskSuiteTaskStatus{TaskID: "task-3", PassNumber: 2, State: model.StateRunning}
	run := model.TaskSuiteRun{Tasks: []model.TaskSuiteTaskStatus{a, b, c}}

	got := run.PassTasks(2)
	if len(got) != 2 || got[0].TaskID != "task-2" || got[1].TaskID != "task-3" {
		t.Fatalf("PassTasks(2) = %+v, want task-2 and task-3 in order", got)
	}
	if got := run.PassTasks(3); got != nil {
		t.Fatalf("PassTasks(3) = %+v on a run with no pass 3, want nil", got)
	}
}

func TestOutcomeOfPassCleanOnceEveryTaskCompletesWithNothingToShow(t *testing.T) {
	tasks := []model.TaskSuiteTaskStatus{passTask(1, model.StateCompleted), passTask(1, model.StateCompleted)}
	if got := model.OutcomeOfPass(tasks); got != model.PassClean {
		t.Fatalf("OutcomeOfPass = %v, want PassClean", got)
	}
}

func TestOutcomeOfPassChangedOnAPullRequestOrAProposal(t *testing.T) {
	opened := passTask(1, model.StateCompleted)
	opened.OpenedPullRequest = true
	if got := model.OutcomeOfPass([]model.TaskSuiteTaskStatus{passTask(1, model.StateCompleted), opened}); got != model.PassChanged {
		t.Fatalf("OutcomeOfPass with a pull request = %v, want PassChanged", got)
	}
	proposed := passTask(1, model.StateCompleted)
	proposed.Proposed = true
	if got := model.OutcomeOfPass([]model.TaskSuiteTaskStatus{passTask(1, model.StateCompleted), proposed}); got != model.PassChanged {
		t.Fatalf("OutcomeOfPass with a proposal = %v, want PassChanged", got)
	}
}

func TestOutcomeOfPassPendingWhileAnyTaskIsStillInFlight(t *testing.T) {
	for _, state := range []model.State{model.StateProposed, model.StateQueued, model.StateRunning, model.StateAwaitingReply} {
		tasks := []model.TaskSuiteTaskStatus{passTask(1, model.StateCompleted), passTask(1, state)}
		if got := model.OutcomeOfPass(tasks); got != model.PassPending {
			t.Errorf("OutcomeOfPass alongside a %s task = %v, want PassPending", state, got)
		}
	}
}

func TestOutcomeOfPassFailedOnAFailureOrACloseWithoutCompleting(t *testing.T) {
	for _, state := range []model.State{model.StateFailed, model.StateClosed} {
		tasks := []model.TaskSuiteTaskStatus{passTask(1, model.StateCompleted), passTask(1, state)}
		if got := model.OutcomeOfPass(tasks); got != model.PassFailed {
			t.Errorf("OutcomeOfPass alongside a %s task = %v, want PassFailed", state, got)
		}
	}
}

func TestOutcomeOfPassFailureOutranksAStragglerInEitherOrder(t *testing.T) {
	// A pass fires every item at once, so one item failing while another
	// is still running is the ordinary shape of a pass going wrong --
	// and it must read the same whichever order the two come back in,
	// rather than letting the straggler hide a failure already known.
	failed, running := passTask(1, model.StateFailed), passTask(1, model.StateRunning)
	if got := model.OutcomeOfPass([]model.TaskSuiteTaskStatus{failed, running}); got != model.PassFailed {
		t.Errorf("failure first: got %v, want PassFailed", got)
	}
	if got := model.OutcomeOfPass([]model.TaskSuiteTaskStatus{running, failed}); got != model.PassFailed {
		t.Errorf("straggler first: got %v, want PassFailed", got)
	}
}

func TestOutcomeOfPassFailureOutranksAChange(t *testing.T) {
	changed := passTask(1, model.StateCompleted)
	changed.OpenedPullRequest = true
	tasks := []model.TaskSuiteTaskStatus{changed, passTask(1, model.StateFailed)}
	if got := model.OutcomeOfPass(tasks); got != model.PassFailed {
		t.Fatalf("OutcomeOfPass = %v, want PassFailed", got)
	}
}

func TestOutcomeOfPassOfNoTasksIsClean(t *testing.T) {
	// Unreachable through the store -- CreateTaskSuiteRun always files a
	// pass, and a suite always has at least one item -- but a zero value
	// must not read as pending, which would strand a run forever.
	if got := model.OutcomeOfPass(nil); got != model.PassClean {
		t.Fatalf("OutcomeOfPass(nil) = %v, want PassClean", got)
	}
}
