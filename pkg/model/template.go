package model

import "time"

// TaskTemplate is a reusable declaration of a task's content -- the same
// "what to do" fields Task and Schedule already carry, saved once under a
// name so more than one place can point at it instead of repeating the
// same title, body, and grants by hand (bwsalmon/agents#516). A schedule
// is the first caller (Schedule.TemplateID); nothing about this type
// itself knows what a schedule is, the same way Task itself knows nothing
// about Schedule -- more callers are expected to arrive later without
// this type changing shape for them.
//
// Deliberately, there is no Target and no Base here: unlike Title/Body/
// AutoMerge/Reads/Grants, which one repeats verbatim across every
// caller, which repo and branch a firing targets is a property of the
// use -- a schedule's own standing repo, or the repo and branch a suite
// run or qualification candidate is running against -- not of the
// reusable content itself. Every caller supplies its own Target and Base
// when it fires a task from this template.
//
// Unlike Task, there is no Intent and no Origin here: those describe one
// filing, not a reusable declaration meant to be filed more than once by
// more than one caller, each of which decides its own origin and intent
// (fireTaskSchedule, for a schedule, always files IntentImplement
// attributed to the scheduler principal -- a template does not override
// either).
type TaskTemplate struct {
	ID string
	// Name is how a human picks this template out of a list -- shown in
	// the template picker itself, never copied onto a filed Task the way
	// Title is.
	Name string

	Title string
	Body  string

	Reads  []RepoRef
	Grants []Grant

	AutoMerge bool

	CreatedAt time.Time
}
