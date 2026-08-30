# Scheduled tasks (v2)

bwsalmon/agents#376: give v2 the recurring-chore mechanism v1 already
proved out (bwsalmon/agents#163, `docs/roadmap.md` §28 — a dependency
audit, a weekly report, something that lands on the queue on its own
cadence instead of only ever starting from a human filing it), reshaped
the way v2 already reshaped ordinary task creation: a schedule is a store
row a UI can create, edit, pause and delete, not a template file an
operator drops on the host.

This document describes what is actually implemented on this branch
(`v2/pkg/model/schedule.go`, `pkg/orchestrator/schedule.go`,
`pkg/ui/schedules.go`, `pkg/ui/frontend/src/components/
ScheduledTasksOverlay.jsx`, and the accompanying tests), not a proposal —
it exists so a reviewer has one place to read the shape of the feature
and why it looks the way it does, alongside the code's own doc comments.
I have read the diff in full; I have not been able to run `go build`/`go
test` against it myself (this sandbox's Go toolchain is 1.19.8, and
`v2/go.mod` requires 1.26.2 — a pre-existing environment limit, not
something this change introduced), so the "does it actually pass"
question is left to CI/review rather than asserted here.

## What v1 already settled, kept unchanged

`grain/automation/scheduled_jobs.py` and `core.py`'s `_scheduled_jobs`
worked out the mechanism, and this keeps the two load-bearing decisions:

- **No new trigger, cron, or scheduler.** The whole system is already one
  poll loop (`cmd/grain/daemon.go`'s `reconcile`, ticking
  `orchestrator.RunCycle`); a schedule is one more
  `orchestrator.Reconciler` in that same loop
  (`pkg/orchestrator/cycle.go`'s `Reconcilers()`), not new
  infrastructure.
- **A previous, unfinished firing suppresses the next one, independently
  of whether the interval has elapsed.** v1 checked GitHub for an open
  issue carrying the job's marker label; this checks the store for an
  open task carrying the schedule's tag (`Store.HasOpenTaskWithTag`).
  Same reasoning both times: a chore that runs long must not get a
  duplicate, and a chore that finishes early is still held to its own
  cadence rather than refiring immediately.

## What changed, and why

**A schedule is a store row, not a file.** v1's `ScheduledJobsConfig`
loaded `/data/config/scheduled-jobs/*.md` because v1's task creation
itself went through GitHub, and "absence of the directory is the off
switch" fit that world. v2 already moved task creation onto the store
(`Client.CreateTask` writes straight to `model.Store`, no issue
involved) specifically so a UI or the CLI could create one without a
redeploy; a `ScheduledTask` (`pkg/model/schedule.go`) is the same idea —
`ScheduledTasksOverlay.jsx` creates, pauses/resumes, and deletes one the
same way `SecretsOverlay.jsx` already manages secrets.

**No `needs_approval` field.** v1's design draft called `SCHEDULED` "the
one origin that chooses per instance" before `docs/data-model.md`
dissolved the special case: creating a schedule *is* the human's standing
approval, so every firing lands already approved
(`fireScheduledTask` sets `Approval` unconditionally). This
implementation takes that dissolution all the way — there is no
per-schedule opt-in to land `proposed` instead, unlike v1's own
`needs_approval` header. A schedule whose work genuinely needs review
each time is not what this covers today; see "Left open" below.

**Idempotency is a tag on the fired task, not a label matched by
schedule name.** `firingTag(scheduleID)` produces `"schedule:<id>"`,
attached to `Task.Tags` — `docs/data-model.md`'s own resolution ("neither
a state nor a capability: it is an idempotency tag") kept literally,
queried back by `Store.HasOpenTaskWithTag` (a join of `task_tag` against
the `task_state` view for anything short of `closed`).

**Interval is a Go duration string, not v1's integer hours.**
`Schedule.Interval` round-trips as `"24h0m0s"` etc., matching
`Settings.PollInterval`'s already-established convention
(`pkg/ui/settings.go`) rather than inventing a second syntax — this also
buys sub-hour cadences (`"30m"`) v1's `Interval-Hours` header could not
express.

**`NextRunAt`, not "last fired + interval, computed on read."** Each
schedule carries its own `NextRunAt`/`LastRunAt` columns
(`scheduled_task` table, `SchemaVersion` 7 → 8), advanced by
`fireScheduledTask` itself in a loop that walks `NextRunAt` forward by
`Interval` until it is back in the future — so a schedule paused (or a
daemon down) through several missed intervals fires once on resume and
resyncs to its normal cadence, rather than firing once per missed
interval or drifting against wall-clock time. A freshly created schedule
gets `NextRunAt = now`, so it fires on the very next cycle rather than
waiting a full interval for its first task — `CreateTask`'s own "queued
the moment it is written" immediacy, applied to a schedule's timing.

**Pausing keeps the row.** `DeleteScheduledTask` removes a schedule
outright with no soft-delete, since (per its own doc comment) a schedule
carries no history worth keeping once nobody wants it — every task it
already filed stays exactly where it was. Pausing instead
(`Enabled = false`) is the separate, reversible operation, matching the
UI's "Pause"/"Resume" button rather than "Delete" for that case.

## Shape of the pieces

- **`model.ScheduledTask`** (`pkg/model/schedule.go`): `ID`, `Title`,
  `Body`, `Target`, `Base`, `AutoMerge`, `Interval`, `Enabled`,
  `NextRunAt`, `LastRunAt`, `CreatedAt`. No `Reads`, `Grants`,
  `DependsOn`, or `Capabilities` — a firing carries none of those (see
  "Left open").
- **Schema** (`pkg/model/schema.go`): `scheduled_task` (one row per
  schedule) and `scheduled_task_sequence` (its own autoincrement
  allocator, so a schedule id like `sched-3` is never confused with a
  task id — `NewScheduledTaskID`, distinct from `NewTaskID`).
  `SchemaVersion` 7 → 8. No changes to `task`, `task_state`,
  `task_blocked`, or `task_ready`: the tag-based open-firing check reads
  `task_tag`/`task_state`, both of which already existed.
- **Store** (`pkg/model/store.go`): `PutScheduledTask`,
  `GetScheduledTask`, `ListScheduledTasks`, `UpdateScheduledTask`
  (read-modify-write-and-retry, `UpdateTask`'s own shape),
  `DeleteScheduledTask`, `DueScheduledTasks(ctx, now)` (enabled schedules
  whose `NextRunAt` has passed), `HasOpenTaskWithTag`.
- **Orchestrator** (`pkg/orchestrator/schedule.go`): `reconcileSchedule`,
  registered in `Reconcilers()` *before* `dispatch` — so a task a
  schedule files this tick is dispatchable the same tick, the same
  latency argument that already puts `sync` last. `fireScheduledTask`
  does the open-tag check, allocates a task id, builds and writes the
  `model.Task` (attributed to a new `scheduler` principal — automation,
  ID `"schedule"`, distinct from `"grain"`'s relayed-agent-output
  identity and from the merge queue's own automatic-fix identity), then
  advances `NextRunAt`/`LastRunAt`.
- **HTTP API** (`pkg/ui/schedules.go`, wired in `server.go`): `GET
  /api/schedules`, `POST /api/schedules`, `PATCH /api/schedules/{id}`,
  `DELETE /api/schedules/{id}` — no `GET /api/schedules/{id}`, since the
  overlay only ever needs the list. `Client.CreateSchedule`/
  `UpdateSchedule`/`DeleteSchedule`/`ListSchedules` are store-backed the
  same way `Client.CreateTask` is; `HTTPClient` (the CLI/remote path)
  gains no matching methods, so a schedule can be managed from the web
  UI but not yet from `grain` on the command line — see "Left open."
- **Frontend** (`ScheduledTasksOverlay.jsx`): a list-plus-form overlay
  following `SecretsOverlay.jsx`'s pattern exactly, opened from a new
  sidebar entry alongside Secrets/Settings. A task the overlay's own
  schedule fired shows a "Scheduled" badge in the ordinary task list
  (`Task.Scheduled`, off `Origin.Reason == ReasonSchedule` — the same
  treatment `Task.Stacked` already gives `ReasonFix`).

## Tests present

`pkg/model/sqlite/schedule_store_test.go` (store round-trips, due-listing
against a fixed `now`, the open-tag gate), `pkg/orchestrator/
schedule_test.go` (a due schedule fires exactly one task and advances
its timing; an open previous firing suppresses the next one; a
not-yet-due schedule fires nothing), `pkg/ui/schedules_test.go` and
`server_schedules_test.go` (the `Client` methods and the five HTTP
routes), and `ScheduledTasksOverlay.test.jsx` plus the `Sidebar.test.jsx`
addition for the new entry point.

## Left open

- **No CLI.** `grain schedule ...` would need `HTTPClient` methods this
  branch does not add; straightforward on top of the existing `Client`
  methods, not bundled here.
- **No per-schedule approval gate.** Every firing lands already
  approved; a schedule whose output should be reviewed before it
  dispatches has no way to ask for that today, unlike v1's
  `needs_approval` header. Worth a follow-up if a real chore needs it.
- **No `Reads`, `Grants`, or `DependsOn` on a schedule.** A firing gets
  none of `Task`'s wider capability/read/dependency surface — fine for a
  simple recurring chore against one write target, not yet expressive
  enough for one that needs a capability or a read-only companion repo.
- **No per-schedule timezone.** `NextRunAt`/`LastRunAt` are UTC
  `time.Time` throughout, same as every other timestamp in this store;
  "every N hours/minutes since it last fired" is the whole of what a
  schedule can express, not "at 09:00 local time."

## Update: schedules got their own pane (bwsalmon/agents#455)

The "Frontend" bullet above and `ScheduledTasksOverlay.jsx` describe how
this first shipped; bwsalmon/agents#455 moved the UI from a modal opened
by a footer button to a full pane (`SchedulesList.jsx`), selected the
same way the repo page already is: a `ListItemButton` alongside "Repos"
in `Sidebar.jsx`, switching `App.jsx`'s `view` state to `"schedules"`
rather than toggling an overlay's visibility. This also closed the gap
the original overlay left open -- editing an existing schedule's title,
description, repo, base branch, auto-merge, or interval, which
`UpdateSchedule`/`PATCH /api/schedules/{id}` already accepted but no UI
exposed beyond the enabled toggle. No backend change was needed; the API
surface was already complete.

## Update: cadences beyond "every N hours", and the rest of Task's own field set (bwsalmon/agents#464)

Two of this document's own "Left open" items from when the feature first
shipped are closed as of here.

**A schedule is no longer only "every N hours/minutes since it last
fired."** `model.ScheduledTask.Interval` (a bare `time.Duration`) is
replaced by `Recurrence` (`pkg/model/schedule.go`), a small sum type: `Kind`
is one of `RecurrenceEveryNHours`, `RecurrenceDaily`, `RecurrenceWeekly`, or
`RecurrenceMonthly`, and the fields that apply depend on which -- the same
"column per case, most left unset" shape `Task.Origin`/`Task.Approval`
already use for a different sum type. `Recurrence.Next(after)` is the
whole of the new logic: the first occurrence strictly after `after`,
wall-clock aligned for the three new kinds (a calendar month is not a
fixed number of hours, so `RecurrenceMonthly` walks actual months rather
than adding an approximate duration, clamping a day-of-month past a
shorter month's own last day rather than overflowing into the next one).
`fireScheduledTask`'s loop is otherwise unchanged in shape -- it still
walks forward from the schedule's own `NextRunAt` (never from "now"
directly) until back in the future, so a schedule paused, or a daemon
down, through several missed occurrences still fires exactly once on
resume. `EveryNHours` keeps hour granularity rather than the old
`Interval`'s arbitrary duration string, matching v1's own
`Interval-Hours` header and what this issue itself asks for; the
still-open "no per-schedule timezone" bullet below is otherwise
unchanged, since `TimeOfDay`/`Weekday`/`DayOfMonth` are read against UTC,
same as `NextRunAt`/`LastRunAt` always have been.

A database created before this migrates in place
(`ensureScheduledTaskRecurrenceColumns`, `pkg/model/store.go`): its old
`interval_ms` column is read once, backfilled into `every_n_hours`
(rounded down to whole hours, matching `RecurrenceEveryNHours`'s own
granularity) and dropped, the same probe-then-`ALTER TABLE` approach
`ensureConfigMaxConcurrentColumn` already established for `grain_config`.
`SchemaVersion` 9 → 10.

**Reads and Grants join Target/Base/AutoMerge on a schedule.** The
former "Left open" bullet on this narrowed: a schedule's own `Reads`
(read-only repos, `scheduled_task_read`) and `Grants` (capabilities,
`scheduled_task_grant`) are new child tables, `task_read`/`task_grant`'s
own shape ported onto `scheduled_task_id`, and `fireScheduledTask` copies
both onto the filed `Task` the same way it already copies `Target`/
`Base`/`AutoMerge`. `DependsOn` and `Approved` remain deliberately absent
-- `ui.CreateScheduleRequest`'s own doc comment explains why: a one-shot
dependency link makes no sense against a task a schedule refiles
indefinitely, and a firing lands already approved by design regardless of
what a per-task `Approved` flag would otherwise ask for.

**`SchedulesList.jsx`'s create and edit forms are now one component**
(`ScheduleForm`), rather than two independently maintained copies of the
same field list -- worth doing now that the field list grew to match
`NewTaskOverlay.jsx`'s own (title, description, repo, base, read-only
repos, capabilities, auto-merge), plus a new `RecurrenceFields` picker:
a "Repeat" select for the four `Kind`s, showing only the inputs that
apply to whichever is chosen (an hours field for every-N-hours; a time
field for the other three; a weekday select added for weekly; a
day-of-month field added for monthly).

Still open, unchanged from before: no CLI, and no per-schedule approval
gate.
