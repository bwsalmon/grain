package model

import "time"

// ScheduledTask is a task template a human sets up once and grain refiles
// as a real Task on a fixed interval -- bwsalmon/agents#376's v2
// equivalent of v1's scheduled_jobs.py, with one deliberate difference:
// there is no template directory here, because the whole point of this
// package's own shape (README, "Input is a model update, not a GitHub
// issue") is that a schedule is a store row a UI can create, edit and
// delete, not a file an operator drops on a server's disk.
//
// docs/data-model.md's "the SCHEDULED special case dissolves" is why
// firing carries no approval flag of its own: creating a schedule is
// itself the standing human approval, so every task it later files lands
// already approved (orchestrator.fireScheduledTask sets Approval
// unconditionally) -- the actor rule (LandsQueued) still holds
// unqualified, because the automation that actually creates the firing
// is never what decided to trust it.
type ScheduledTask struct {
	ID    string
	Title string
	Body  string

	Target    RepoRef
	Base      string
	AutoMerge bool

	// Interval is how often this schedule fires, e.g. every 24h.
	Interval time.Duration
	// Enabled is the pause/resume switch a UI toggles -- a disabled
	// schedule stays around (with its own history in NextRunAt/LastRunAt)
	// rather than being deleted, the same as closing a task keeps its row
	// rather than erasing it.
	Enabled bool
	// NextRunAt is when this schedule next comes due. Set to the
	// schedule's own creation time when it is first created, so a fresh
	// schedule fires once right away rather than waiting a full interval
	// for its first task -- the same "queued the moment it is written"
	// immediacy CreateTask already gives a task filed by hand.
	NextRunAt time.Time
	// LastRunAt is when this schedule last actually filed a task, nil if
	// it never has.
	LastRunAt *time.Time

	CreatedAt time.Time
}
