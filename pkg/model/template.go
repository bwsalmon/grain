package model

import "time"

// Template is a reusable declaration of a task's content -- the same
// "what to do" fields Task and Schedule already carry, saved once under
// a name so more than one place can point at it instead of repeating
// the same title, body, and grants by hand (bwsalmon/agents#516). A
// schedule is the first caller (Schedule.TemplateID); nothing about
// this type itself knows what a schedule is, the same way Task itself
// knows nothing about Schedule -- more callers are expected to arrive
// later without this type changing shape for them.
//
// Target and Base are optional (grain/task-285): left unset, which repo
// and branch a firing targets is a property of the use -- a schedule's
// own standing repo, or the repo and branch a suite run or qualification
// candidate is running against -- and every caller supplies its own,
// exactly as it did while a template could carry none at all. Set, they
// bind this template to one repo (and, if Base is set too, one branch):
// content that only makes sense against that repo travels with it, and
// no caller has to name the repo a second time or can name a different
// one by mistake.
//
// Unlike Task, there is no Intent and no Origin here: those describe one
// filing, not a reusable declaration meant to be filed more than once by
// more than one caller, each of which decides its own origin and intent
// (fireTaskSchedule, for a schedule, always files IntentImplement
// attributed to the scheduler principal -- a template does not override
// either).
type Template struct {
	ID string
	// Name is how a human picks this template out of a list -- shown in
	// the template picker itself, never copied onto a filed Task the way
	// Title is.
	Name string

	Title string
	Body  string

	// Target, when non-nil, is the repo every task filed from this
	// template targets, whoever fires it and whatever repo that caller
	// otherwise works against -- a schedule's Target, a suite run's
	// Target. nil means unbound: the caller's own target decides, which
	// is how every template behaved before binding existed and still the
	// default for a template created without one.
	//
	// A qualification plan is the one caller that does not let a binding
	// move a firing: a qualification run exists to test one candidate's
	// own branch, so its tasks always target the candidate's repo and
	// branch, and a bound template is instead only allowed into a plan
	// for the repo it is bound to (CreateQualificationRun,
	// ui.Client.PutQualificationPlan).
	Target *RepoRef
	// Base is the branch a bound template's firings target. Only
	// meaningful alongside Target -- an unbound template's firings take
	// the caller's own base -- and optional even then: empty means the
	// target repo's own default branch, the same as an empty Base
	// anywhere else (Task.Base). So a template can bind a repo without
	// pinning a branch, which is what most bound templates want.
	Base string

	Reads  []RepoRef
	Grants []Grant

	AutoMerge bool

	CreatedAt time.Time
}

// FiringTarget is the repo and branch a task filed from this template
// targets, given the target and base the caller would otherwise have
// used: the binding when there is one, the caller's own when there is
// not. Every caller that turns a template into a Task goes through here
// (orchestrator.fireTaskSchedule, fireSuitePass, ui's own preview of
// what a template-backed schedule will fire) so the rule is written once
// rather than re-derived, subtly differently, at each of them.
//
// A bound template's own empty Base is passed straight through rather
// than falling back to the caller's: a caller's base names a branch of
// the repo *it* was going to target, which a binding has just replaced,
// so carrying it over would be a branch name from one repo applied to
// another. Empty means the target repo's default branch, which is
// exactly what a binding with no branch asks for.
func (t Template) FiringTarget(target RepoRef, base string) (RepoRef, string) {
	if t.Target == nil {
		return target, base
	}
	return *t.Target, t.Base
}
