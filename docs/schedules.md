# Schedules (v2)

bwsalmon/agents#376: give v2 the recurring-chore mechanism v1 already
proved out (bwsalmon/agents#163, `docs/roadmap.md` §28 — a dependency
audit, a weekly report, something that lands on the queue on its own
cadence instead of only ever starting from a human filing it), reshaped
the way v2 already reshaped ordinary task creation: a schedule is a store
row a UI can create, edit, pause and delete, not a template file an
operator drops on the host.

This document describes what is actually implemented on this branch
(`pkg/model/schedule.go`, `pkg/orchestrator/schedule.go`,
`pkg/ui/schedules.go`, `ui/src/components/ScheduleOverlay.jsx`,
and the accompanying tests), not a proposal — it exists so a reviewer
has one place to read the shape of the feature and why it looks the way
it does, alongside the code's own doc comments.
I have read the diff in full; I have not been able to run `go build`/`go
test` against it myself (this sandbox's Go toolchain is 1.19.8, and
`go.mod` requires 1.26.2 — a pre-existing environment limit, not
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
redeploy; a `Schedule` (`pkg/model/schedule.go`) is the same idea —
`ScheduleOverlay.jsx` creates, pauses/resumes, and deletes one the
same way `SecretsPanel.jsx` already manages secrets.

**No `needs_approval` field.** v1's design draft called `SCHEDULED` "the
one origin that chooses per instance" before `docs/data-model.md`
dissolved the special case: creating a schedule *is* the human's standing
approval, so every firing lands already approved
(`fireTaskSchedule` sets `Approval` unconditionally). This
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
(`schedule` table, `SchemaVersion` 7 → 8), advanced by
`fireTaskSchedule` itself in a loop that walks `NextRunAt` forward by
`Interval` until it is back in the future — so a schedule paused (or a
daemon down) through several missed intervals fires once on resume and
resyncs to its normal cadence, rather than firing once per missed
interval or drifting against wall-clock time. A freshly created schedule
gets `NextRunAt = now`, so it fires on the very next cycle rather than
waiting a full interval for its first task — `CreateTask`'s own "queued
the moment it is written" immediacy, applied to a schedule's timing.

**Pausing keeps the row.** `DeleteSchedule` removes a schedule
outright with no soft-delete, since (per its own doc comment) a schedule
carries no history worth keeping once nobody wants it — every task it
already filed stays exactly where it was. Pausing instead
(`Enabled = false`) is the separate, reversible operation, matching the
UI's "Pause"/"Resume" button rather than "Delete" for that case.

## Shape of the pieces

- **`model.Schedule`** (`pkg/model/schedule.go`): `ID`, `Title`,
  `Body`, `Target`, `Base`, `AutoMerge`, `Interval`, `Enabled`,
  `NextRunAt`, `LastRunAt`, `CreatedAt`. No `Reads`, `Grants`,
  `DependsOn`, or `Capabilities` — a firing carries none of those (see
  "Left open").
- **Schema** (`pkg/model/schema.go`): `schedule` (one row per
  schedule) and `schedule_sequence` (its own autoincrement
  allocator, so a schedule id like `sched-3` is never confused with a
  task id — `NewScheduleID`, distinct from `NewTaskID`).
  `SchemaVersion` 7 → 8. No changes to `task`, `task_state`,
  `task_blocked`, or `task_ready`: the tag-based open-firing check reads
  `task_tag`/`task_state`, both of which already existed.
- **Store** (`pkg/model/store.go`): `PutSchedule`,
  `GetSchedule`, `ListSchedules`, `UpdateSchedule`
  (read-modify-write-and-retry, `UpdateTask`'s own shape),
  `DeleteSchedule`, `DueSchedules(ctx, now)` (enabled schedules
  whose `NextRunAt` has passed), `HasOpenTaskWithTag`.
- **Orchestrator** (`pkg/orchestrator/schedule.go`): `reconcileSchedule`,
  registered in `Reconcilers()` *before* `dispatch` — so a task a
  schedule files this tick is dispatchable the same tick, the same
  latency argument that already puts `sync` last. `fireTaskSchedule`
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
- **Frontend** (`ScheduleOverlay.jsx`): a list-plus-form overlay
  following `SecretsPanel.jsx`'s pattern exactly, opened from a new
  sidebar entry alongside Settings. A task the overlay's own
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
routes), and `SchedulesList.test.jsx` plus the `Sidebar.test.jsx`
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

The "Frontend" bullet above and `ScheduleOverlay.jsx` describe how
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
fired."** `model.Schedule.Interval` (a bare `time.Duration`) is
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
`fireTaskSchedule`'s loop is otherwise unchanged in shape -- it still
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
(`ensureScheduleRecurrenceColumns`, `pkg/model/store.go`): its old
`interval_ms` column is read once, backfilled into `every_n_hours`
(rounded down to whole hours, matching `RecurrenceEveryNHours`'s own
granularity) and dropped, the same probe-then-`ALTER TABLE` approach
`ensureConfigMaxConcurrentColumn` already established for `grain_config`.
`SchemaVersion` 9 → 10.

**Reads and Grants join Target/Base/AutoMerge on a schedule.** The
former "Left open" bullet on this narrowed: a schedule's own `Reads`
(read-only repos, `schedule_read`) and `Grants` (capabilities,
`schedule_grant`) are new child tables, `task_read`/`task_grant`'s
own shape ported onto `schedule_id`, and `fireTaskSchedule` copies
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

## Update: templates (bwsalmon/agents#516)

A schedule's own content -- title, description, target repo, base
branch, auto-merge, read-only repos, and capabilities -- used to exist
only inline on the schedule itself, one copy per schedule with no way to
share it. `Template` (`pkg/model/template.go`) is that content pulled
out into its own store row, `template` plus its own
`template_read`/`template_grant` child tables (`SchemaVersion` 11 → 12),
so more than one schedule can point at the same declaration -- and, per
this issue's own framing ("we will add more places to use them in the
future"), so a later caller beyond schedules has somewhere to point too,
without this type or its store surface changing shape for that caller.

**A schedule optionally references a template rather than always
carrying its own copy.** `Schedule.TemplateID` (nullable
`schedule.template_id`, a database created before this migrates in
place via `ensureScheduleTemplateColumn` the same probe-then-`ALTER
TABLE` approach every other migration in `store.go` already uses) is
`nil` for a schedule that still declares its content inline, exactly as
every schedule always has; set, it names the template that content comes
from instead. `ui.CreateScheduleRequest`/`UpdateScheduleRequest` treat
the two as mutually exclusive -- a `templateId` given at creation takes
over Title/Description/Repo/Base/AutoMerge/Reads/Capabilities entirely
rather than letting a caller mix a template with its own per-field
overrides, a combination deliberately unsupported so "what does this
schedule actually file" never depends on resolving a merge between two
sources. `UpdateScheduleRequest.TemplateID` adds one more case an
ordinary nilable pointer already covers reasonably: given as `""` it
detaches a schedule from its template, leaving whatever content was last
synced onto the row in place as an independent, directly-editable copy
rather than blanking it out.

**Resolved fresh at firing time, not copied in once.**
`orchestrator.fireTaskSchedule` re-reads a template-backed schedule's
`Template` from the store on every firing rather than trusting
`Schedule`'s own inline columns, so editing a template changes what
every schedule pointing at it files *next*, with no separate "push the
edit out" step -- the entire reason a template is worth having over
copy-paste. Those inline columns are not dead weight even so: the same
firing writes the template's current content back onto them, so
`ui.scheduleFrom` can still render a schedule's effective title, repo,
and the rest with no join, and a template deleted or otherwise
unresolvable out from under a schedule fails that one firing with a
plain error (`reconcileSchedule`'s own "one schedule failing does not
stop the others" tolerance already covers this) rather than filing a
task with no content.

**Deleting a template in use is refused, not silently orphaning.**
`Store.SchedulesUsingTemplate` is what `ui.Client.DeleteTemplate` checks
before deleting: a template still named by a live schedule cannot be
removed until that schedule is repointed or deleted first, since deleting
it anyway would strand that schedule's next firing far more silently than
the plain 404 a stale reference would otherwise surface only days or
weeks later.

**HTTP API** (`pkg/ui/templates.go`, wired in `server.go`): `GET
/api/templates`, `POST /api/templates`, `PATCH /api/templates/{id}`,
`DELETE /api/templates/{id}` -- `schedules.go`'s own four-route shape,
with no per-template `GET` for the same reason schedules have none.

**Frontend** (`TemplatesList.jsx`): its own pane, alongside Scheduled
tasks in the sidebar, following `SchedulesList.jsx`'s own list-plus-form
shape minus `RecurrenceFields` -- a template carries no cadence of its
own. `SchedulesList.jsx`'s `ScheduleForm` gained a "Template" picker
ahead of its other fields: choosing one hides Title/Description/Repo/
Base/Reads/Capabilities/Auto-merge outright (rather than showing fields
a template-backed request would silently ignore) and shows a short note
that they come from the template instead.

Tests: `pkg/model/sqlite/template_store_test.go` (round trips, the
schema-12 migration), `pkg/orchestrator/schedule_test.go` (fresh
resolution across an edit between two firings, the missing-template
failure path), `pkg/ui/templates_test.go` and `server_templates_test.go`
(the `Client` methods and the four HTTP routes, plus the delete-while-in-
use guard), `pkg/ui/schedules_test.go` (attach/detach, the unknown-
template rejection), and `TemplatesList.test.jsx` plus the
`SchedulesList.test.jsx`/`App.test.jsx` additions for the picker.

Left open: no CLI for templates (`HTTPClient` gains no matching methods,
the same gap schedules already have); no UI affordance yet for the "more
places to use them" this issue asks for beyond schedules -- creating an
ordinary task from a template, most plausibly, is left for whenever a
concrete caller actually needs it.

## Update: templates page split into a list and a sub-page overlay (bwsalmon/agents#545)

`TemplatesList.jsx`'s list-plus-form shape (previous section) put every
template's full form inline in the row being edited, plus a permanently
open "New template" form at the foot of the list -- fine when templates
were new and few, but unlike `TaskList.jsx` it had no search, no sort,
and nothing to keep a longer list scannable.

`TemplatesList.jsx` is now a flat list of key details only -- name,
target repo, task title -- with `TaskList.jsx`'s own search-and-sort
toolbar (`SORTS` here has no `manual` entry: a template has no backlog
order, so `name` is the default instead). Everything about one
template -- description, base branch, read-only repos, capabilities,
auto-merge, and now delete too -- moved into a new `TemplateOverlay.jsx`,
opened either by the list's own "+ New template" button (blank) or by
clicking a row (pre-filled), `NewTaskOverlay.jsx`/`DetailOverlay.jsx`'s
own split between a list of key details and everything about one item
living behind a click. `TemplateOverlay` stays owned by `TemplatesList`
itself rather than lifting to `App.jsx` the way task detail/creation
does -- `SchedulesList.jsx`'s own form state is local for the same
reason: nothing outside this pane needs to know a template is mid-edit.

No API or model changes: `Template`'s wire shape already carried
everything the new overlay needed, including `createdAt` for the
Newest/Oldest sorts.

Tests: `TemplatesList.test.jsx` covers the list (search, sort, empty and
no-match states) and, since `TemplateOverlay` has no store state of its
own outside this pane, exercises create/edit/delete/cancel through it
the same way `SchedulesList.test.jsx` already exercises `ScheduleForm`
inline rather than as a separate suite.

## Update: schedules page split into a list and a sub-page overlay (bwsalmon/agents#547)

`SchedulesList.jsx`'s list-plus-form shape -- the very shape the two
updates above already carried over to `TemplatesList.jsx`, then moved
`TemplatesList.jsx` off of -- gets the same treatment here, so the
schedules page ends up looking like the tasks page the way both of those
pages already look like each other. Previously `SchedulesList.jsx` put a
full edit form inline in whichever row was being edited (`ScheduleForm`,
reused for both), a Pause/Resume and Delete button on every row, and a
permanently open "New schedule" form pinned to the foot of the list --
fine when there were only a couple of schedules, but with no search or
sort it did not scale the way `TaskList.jsx`'s own toolbar lets the task
list scale, and editing lived somewhere `TaskList.jsx`'s own rows never
put it.

`SchedulesList.jsx` is now a flat list of key details only -- title,
target repo, cadence, template (if any), and a "Paused" chip when
disabled -- with `TaskList.jsx`'s own search-and-sort toolbar (`SORTS`
here has no `manual` entry, `TemplatesList.jsx`'s own reasoning: a
schedule has no backlog order, so `title` is the default instead).
Everything about one schedule -- description, base branch, read-only
repos, capabilities, the recurrence picker, and now Pause/Resume and
Delete too -- moved into a new `ScheduleOverlay.jsx`, opened either by
the list's own "+ New schedule" button (blank) or by clicking a row
(pre-filled), `TemplateOverlay.jsx`'s own split applied here. Pause/
Resume and Delete sitting inside the overlay rather than on the row
matches where a task's own Close/Reopen already live -- inside
`DetailOverlay.jsx`, not on a `TaskList.jsx` row -- rather than being a
schedule-specific pattern.

No API or model changes: `Schedule`'s wire shape already carried
everything the new overlay and sort options need, including `createdAt`
for the Newest/Oldest sorts, and `PATCH /api/schedules/{id}` already
accepted a bare `{"enabled": ...}` body for Pause/Resume, unchanged from
how the old inline button used it.

Tests: `SchedulesList.test.jsx` covers the list (search, sort, empty and
no-match states, recurrence descriptions) and, since `ScheduleOverlay`
has no store state of its own outside this pane, exercises create/edit/
pause/resume/delete/cancel through it the same way
`TemplatesList.test.jsx` already exercises `TemplateOverlay`.
`App.test.jsx`'s own schedules-pane tests are updated to open the
overlay (a row click, or "+ New schedule") rather than an inline "Edit"
button.

## Update: templates carry no target repo or branch (bwsalmon/agents#516 revisited)

`Template` (`pkg/model/template.go`) originally carried Target and Base
alongside Title/Body/AutoMerge/Reads/Grants -- the earlier "Update:
templates" section above documents `ui.CreateScheduleRequest`/
`UpdateScheduleRequest` treating a `templateId` as taking over Repo/Base
along with everything else. That was wrong for the same reason a
qualification plan and a suite were already built the other way:
`QualificationPlan.Repo` and `SuiteRun.Target`/`Base` decide what those
two mechanisms target, never a template's own fields -- a template is
reusable content, and which repo and branch a firing targets is a
property of *how* it is used, not of the reusable content itself.
Schedules were the one caller that had a template override it anyway,
and it never generalized: `CreateQualificationRun` always targeted
`candidate.Repo`/`candidate.Branch`, whatever `tmpl.Target` said, and
only failed a run outright if the two happened to disagree.

`template` drops `target_owner`/`target_name`/`base`
(`ensureTemplateNoTargetColumns`, the same probe-then-`ALTER TABLE`
approach every other migration in `store.go` uses, in the direction
`ensureConfigMaxConcurrentColumn`'s own `slots` removal already goes:
probe for the old columns' presence, then drop them, since they are
`NOT NULL` and `PutTemplate` stops supplying them). `ui.Template`/
`CreateTemplateRequest`/`UpdateTemplateRequest` drop `Repo`/`Base` to
match.

`ui.CreateScheduleRequest`/`UpdateScheduleRequest` now take Repo and
Base as their own fields unconditionally, `templateId` or not --
`templateId` still takes over Title/Description/AutoMerge/Reads/
Capabilities entirely, the same "no mixing a template with per-field
overrides" rule as before, just five fields instead of seven.
`orchestrator.fireTaskSchedule` no longer reads Target/Base off a
resolved template; a schedule's own Target/Base are what every firing
targets, template-backed or not. `ui.PutQualificationPlan` and
`model.CreateQualificationRun` drop their "template must target this
same repo" check along with it -- there is no longer a template-side
Target to check against.

No change to suites: `Suite`/`SuiteRun`/`fireSuitePass` never read a
template's Target or Base to begin with (`model.SuiteItem`'s own doc
comment already says a suite resolves each item's template content
only), so `SuiteOverlay.jsx`/`SuiteRunOverlay.jsx` needed no behavioural
change -- only the template picker's secondary text (`t.repo`) and
`RepoReleases.jsx`'s "templates already declared for this repo" filter,
both of which assumed a field that no longer exists, come out.


## Update: a schedule can run a suite, not only file a task

A suite (`docs/design.md`, `pkg/model/suite.go` -- bwsalmon/agents#642)
was runnable only by hand: a human picks a suite, a repo and a branch,
and `ui.Client.CreateSuiteRun` starts one run. The recurring-chore case
this whole document is about wanted the other half -- "sweep this branch
for bugs every night", not "sweep it when I remember to click Run" -- so
a schedule now fires either of the two things grain knows how to start.

**One schedule type, not a second parallel mechanism.**
`Schedule.SuiteID` (`pkg/model/schedule.go`) sits beside the
existing `TemplateID`, `schedule.suite_id` beside
`schedule.template_id`, and the two are mutually exclusive: a
schedule files a task (inline content or a template's), or it starts a
suite run. Everything else about a schedule keeps meaning exactly what it
already meant -- the recurrence, the enabled/paused switch, `NextRunAt`/
`LastRunAt` and the walk-forward loop that resyncs a schedule paused
through several missed occurrences, the target repo and base branch, and
the "a previous firing that has not finished suppresses the next one"
rule. That was the point of doing it this way rather than adding a
`ScheduledSuite` row, a second reconciler and a second page: none of that
machinery is about what a firing *is*, so none of it needed a second
copy. `orchestrator.fireSchedule` is the whole of the fork --
`fireTaskSchedule` or `fireSuiteSchedule`, sharing `advanceSchedule`
for the timing half both need.

**Idempotency is an active run, not an open task.** A schedule filing a
task checks `Store.HasOpenTaskWithTag` for its own firing tag; a
schedule running a suite checks `Store.HasActiveRunForSchedule`, since a
firing there is a whole run over as many passes as the suite's mode asks
for, not one task. `suite_run.schedule_id` is what that reads (NULL for
a run a human started, which is also how a database created before this
migrates -- `ensureSuiteRunScheduleColumn`, the same
probe-then-`ALTER TABLE` approach as every other migration in
`store.go`). Both checks
have the same consequence: a suppressed firing leaves `NextRunAt` where
it was, so it is delayed rather than skipped.

**Resolved fresh at firing time.** `fireSuiteSchedule` re-reads the
`Suite` on every firing, exactly as `fireTaskSchedule` re-reads a
`Template`, so editing a suite changes what every schedule pointing at
it runs *next*, with no push step; the schedule's own `Title` is kept in
sync with the suite's name as a display cache the same way. A suite
deleted out from under a schedule fails that one firing with a plain
error rather than starting a run of nothing -- `ui.Client.DeleteSuite`
refuses the delete in the first place (`Store.SchedulesUsingSuite`),
`DeleteTemplate`'s own guard applied to the other thing a schedule can
point at.

**What a schedule fires is fixed when it is created.**
`ui.UpdateScheduleRequest.SuiteID` repoints a suite-backed schedule at a
different suite, but converting a schedule from one kind to the other is
refused in both directions: a suite has no title or body of its own to
become a task's, and a task's content has nowhere to go in a suite, so
the honest answer is "delete it and create the other kind" rather than a
half-populated row. This is the one place a suite-backed schedule reads
differently from a template-backed one, where `templateId: ""` detaches
and leaves the last-synced content behind as an editable copy.

**Base is required for a suite-backed schedule**, matching
`CreateSuiteRunRequest`'s own rule (a suite run stacks its tasks against
one named branch and has no default to fall back to), where an ordinary
schedule may leave it blank and take the repo's default branch. Every
task a firing files is the suite's own -- `SuitePrincipal`,
`ReasonSuite`, the suite's `RequireApproval` and `AutoMerge` -- since a
schedule decides only when a run happens and what it runs against, never
what the run's tasks look like. That is why `RequireApproval` is
available here at all, incidentally closing (for suites only) this
document's long-standing "no per-schedule approval gate" gap: a schedule
running a suite that requires approval leaves each pass's tasks for a
human, where a schedule filing a task still cannot.

**UI.** `ScheduleOverlay.jsx` gains a "Fires" picker (a task, or a task
suite) offered on a new schedule only, and a suite picker in place of the
template picker and every content field when a suite is chosen -- the
same "hide fields the request would silently ignore" rule the template
picker already follows. `SchedulesList.jsx` shows a `Suite: <name>` chip
beside the existing `Template:` one, and `SuitesList.jsx`'s run list
marks a run a schedule started with a "Scheduled" chip.

No `SchemaVersion` bump: both new columns are nullable and added by an
`ensure*` migration, so an existing database upgrades in place rather
than being moved aside at deploy (`SchemaVersion`'s own doc comment on
which changes need the bump).

Tests: `pkg/model/sqlite/schedule_store_test.go` (the `SuiteID` round
trip and `SchedulesUsingSuite`), `pkg/model/sqlite/suite_store_test.go`
(a run recording its schedule, `HasActiveRunForSchedule`, the
schedule_id migration), `pkg/orchestrator/schedule_test.go` (a due
suite-backed schedule starts exactly one run and advances its timing; an
active run suppresses the next firing; a missing suite fails the firing
without advancing it), `pkg/ui/schedules_test.go` and `suites_test.go`
(creation, the suite/template exclusivity, the required base, repointing,
the refused kind change, and the delete-while-in-use guard), and
`SchedulesList.test.jsx`.

Still open, unchanged: no CLI for schedules or suites.

## Update: the task list badges every task nobody filed by hand

`Task.SuiteRun` (`ReasonSuite`) had the backend half of the "Scheduled"
badge above and none of the frontend, so a whole pass of suite-filed
tasks sat in the backlog reading as work somebody typed in. `TaskRow`
now renders a `suite` chip beside `scheduled`, same tinted fill and
same title tooltip.

A stacked task -- the merge queue's own automatic fix, `ReasonFix` --
gets a `merge fix` chip on the same rule, but only when it is *not*
nested under the task it repairs: nesting is already the explanation,
and repeating it on every child row would be noise. `TaskRow` takes a
`nested` prop for that, set by `TaskList.jsx` on the rows it puts in a
`.task-sublist` and left off everywhere a task is listed flat -- the
repo pane, and `groupByStack`'s own fallback for a fix whose parent is
filtered out of the current view or gone. Its tooltip names the parent
when there is one to name.

Tests: `TaskList.test.jsx` (the suite chip, and the merge-fix chip
present in each un-nested case but absent under a parent).

## Update: "scheduled tasks" are just schedules

The feature was named twice over, and the two names had drifted apart.
Most of it already said *schedule*: `/api/schedules`,
`pkg/ui/schedules.go` and its `Schedule`, `SchedulesList.jsx`, the
`sched-` id prefix, this document's own prose throughout. The model, the
store and the schema still said *scheduled task* —
`model.ScheduledTask`, `Store.PutScheduledTask`, the `scheduled_task`
table — and so did the two labels a human actually reads, the sidebar
entry and the list heading. The older name is also actively wrong now: a
schedule has fired a whole suite rather than a single task since the
update above, so "scheduled task" names one of the two things it does.

So: one name, the shorter one. `model.Schedule`, `Store.PutSchedule`/
`GetSchedule`/`ListSchedules`/`UpdateSchedule`/`DeleteSchedule`/
`DueSchedules`/`NewScheduleID`, and `Store.CreateScheduledSuiteRun`.
`orchestrator.fireScheduledTask`/`fireScheduledSuite` become
`fireTaskSchedule`/`fireSuiteSchedule` — the two kinds of schedule, named
after the schedule rather than after what it fires, since `fireSchedule`
above already dispatches between them. The UI's two remaining "Scheduled
tasks" labels (the sidebar entry and the list heading) become
"Schedules", and this file moves from `docs/scheduled-tasks.md` to
`docs/schedules.md`.

The tables move with it: `scheduled_task`, `scheduled_task_sequence`,
`scheduled_task_read` and `scheduled_task_grant` become `schedule`,
`schedule_sequence`, `schedule_read` and `schedule_grant`, and the child
tables' `scheduled_task_id` column becomes `schedule_id`. A database
written before this migrates in place — `Store.renameScheduleTables`,
plain `ALTER TABLE ... RENAME`, no row rewritten and no id reissued
(SQLite carries an AUTOINCREMENT table's `sqlite_sequence` entry across a
rename, so the next `sched-` id follows the last one issued rather than
starting over). It runs *before* `Init` applies the DDL, unlike every
`ensure*` migration beside it, and has to: `Statements()` would otherwise
create an empty `schedule` table beside the populated `scheduled_task`
one, leaving the rename with nowhere to go. Each step is guarded on the
old name being present and the new one absent, so it is idempotent and
safe to interrupt.

No `SchemaVersion` bump, for the reason
`ensureConfigWorkerMergerColumns`' own rename of `max_concurrent` did not
need one: an existing database migrates into the new shape rather than
being one this build cannot be re-created into.

Nothing about behaviour changes, and neither does the wire format: the
REST surface was already `/api/schedules` with `pkg/ui.Schedule`'s own
JSON field names, so no client sees this at all. What stays "scheduled"
is what genuinely describes a *task*: `ui.Task.Scheduled`, the task
list's `scheduled` chip, and `model.ReasonSchedule` all still name a task
filed automatically by a schedule.

Tests: `pkg/model/sqlite/schedule_store_test.go` gains a database built
under the old four table names, checking `Init` carries the row, its
reads and grants, and the sequence position onto the new ones.

## Update: "task templates" and "task suites" are just templates and suites

The same double naming the section above unpicked for schedules was
still there twice over, on the two mechanisms that section leans on.
Most of both already said *template* and *suite*: `/api/templates` and
`/api/suites`, `pkg/ui/templates.go` and `pkg/ui/suites.go` with their
own `Template` and `Suite`, `TemplatesList.jsx`/`SuitesList.jsx`, the
`template-` and `suite-` id prefixes, this document's own prose. The
model, the store and the schema still said *task template* and *task
suite* -- `model.TaskTemplate`, `model.TaskSuite`,
`Store.PutTaskTemplate`, `Store.CreateTaskSuiteRun`, the `task_template`
and `task_suite` tables -- and so did the two sidebar entries and the
list headings a human actually reads.

The longer names were also saying something untrue. A template is not a
template *of a task*: it is reusable content that a schedule fires as a
task, a qualification plan runs against a candidate, and a suite runs as
one of its items -- `model.Template`'s own doc comment has said "more
callers are expected to arrive later" since it was written. A suite is
not a suite *of tasks* either; it is a suite of templates, which is what
a run turns into tasks a pass at a time.

So: one name each, the shorter one. `model.Template` and `model.Suite`,
with `SuiteItem`/`SuiteMode`/`SuiteRun`/`SuiteRunStatus`/
`SuiteTaskStatus` and the `SuiteCount`/`SuiteUntilClean`/`SuiteRun*`
constants following; `Store.NewTemplateID`/`PutTemplate`/`GetTemplate`/
`ListTemplates`/`UpdateTemplate`/`DeleteTemplate`, and
`Store.NewSuiteID`/`PutSuite`/`GetSuite`/`ListSuites`/`UpdateSuite`/
`DeleteSuite`/`SuitesUsingTemplate`/`CreateSuiteRun`/`GetSuiteRun`/
`ListSuiteRuns`/`ActiveSuiteRuns`/`CompleteSuiteRun`;
`orchestrator.SyncTaskSuites` becomes `SyncSuites`. The sidebar's "Task
templates" and "Task suites" entries and the two list headings become
"Templates" and "Suites".

The tables move with it: `task_template`, `task_template_sequence`,
`task_template_read` and `task_template_grant` become `template`,
`template_sequence`, `template_read` and `template_grant` (with the
child tables' `task_template_id` column becoming `template_id`), and
`task_suite`, `task_suite_sequence`, `task_suite_item`, `task_suite_run`,
`task_suite_run_item` and `task_suite_run_task` become `suite`,
`suite_sequence`, `suite_item`, `suite_run`, `suite_run_item` and
`suite_run_task`. A database written before this migrates in place --
`Store.renameTemplateAndSuiteTables`, `renameScheduleTables`' own shape
right down to running before `Init` applies the DDL, being guarded step
by step on the old name being present and the new one absent, and
bumping no `SchemaVersion`.

One thing that rename did not have to deal with: SQLite carries an index
across `ALTER TABLE ... RENAME` under its *own* old name, which no
`CREATE INDEX IF NOT EXISTS` in `Statements()` would ever match. The
five suite indexes are therefore dropped rather than renamed, and the
DDL immediately after creates them again under the names `schema.go`
declares -- including the unique one on `suite_run_task.task_id`, which
is load-bearing rather than cosmetic.

`model.SuitePrincipal`, the automation actor a run attributes the tasks
it files to, was `task-suite` and is now `suite` --
`QualificationPrincipal`'s own `qualification` is the shape it matches.
That one is a value in rows rather than a name in the schema, so
`Store.renameSuitePrincipal` carries existing tasks across with a plain
`UPDATE` of the two columns a run ever writes that actor to (a task's
origin, and the approval a run stamps on the tasks it files itself),
narrowed to rows whose kind is automation and whose id is exactly the
old string.

Nothing about behaviour changes, and neither does the wire format: the
REST surface was already `/api/templates` and `/api/suites` with
`pkg/ui`'s own JSON field names, and both id prefixes were already
`template-` and `suite-`, so no client sees this at all. What stays
"task" is what genuinely describes a task: `model.ReasonSuite`, the task
list's `suite` chip, and `ui.Task.SuiteRun` all still name a task filed
automatically by a suite run.

Tests: `pkg/model/sqlite/template_store_test.go` and
`suite_store_test.go` each gain a database built under the old table
names, checking `Init` carries every row, child row and sequence
position onto the new ones -- and, for suites, that the run's own tasks
and the unique index over them come across too. A third test seeds a
task carrying the old `task-suite` actor and checks `Init` brings it
onto `suite`.

## Update: a template may be bound to a repo and branch, optionally (grain/task-285)

The "templates carry no target repo or branch" section above went one
step too far. It was right that a template *had* to carry a target and
that every caller had to honour it: a qualification plan and a suite
were already built the other way, and schedules were the one caller
where a template overrode a repo the schedule had been pointed at.
Making a target impossible, though, left the many templates that only
make sense against one repo -- "cut this repo's release", "run this
repo's nightly sweep" -- with no way to say so, and their repo had to be
re-named (and could be mis-named) at every point of use.

So the target is back, optional this time. `model.Template` carries
`Target *RepoRef` and `Base string`: nil is unbound, which is what every
existing template is and what a template created without a repo stays,
and a non-nil Target binds the template to that repo, with Base pinning
a branch within it or staying empty for the repo's default.
`Template.FiringTarget(target, base)` is the whole rule in one place --
the binding when there is one, the caller's own when there is not --
and a bound template's empty Base is passed through rather than falling
back to the caller's, since a caller's base names a branch of the repo
the binding has just replaced.

Every caller that turns a template into a task goes through it:

- `orchestrator.fireTaskSchedule` targets the binding over the
  schedule's own Target/Base, and resyncs the schedule's row to it,
  alongside the Title/Body/AutoMerge/Reads/Grants display cache it
  already kept there.
- `ui.CreateSchedule`/`UpdateSchedule` apply the same rule at the point
  a schedule is created or attached to a template, so a schedule reads
  as what it will actually fire rather than only after its first firing.
  `repo` stops being required outright on `CreateScheduleRequest`: a
  schedule fired from a bound template needs none, every other schedule
  still does.
- `fireSuitePass` resolves target and base per item, so a suite can mix
  repo-agnostic items with ones bound to a repo of their own and each
  lands where it belongs.

Qualification is the one caller a binding cannot move, and the check the
earlier section deleted comes back with it. A qualification run exists
to test one candidate's own branch, so its tasks still target
`candidate.Repo`/`candidate.Branch` -- a bound template's branch never
moves one. A template bound to a *different* repo is instead refused
entry to that repo's plan: by `ui.Client.PutQualificationPlan` when a
plan is saved, by `ui.Client.UpdateTemplate` when a template a plan
already schedules is bound away from it (`DeleteTemplate`'s own "do not
strand a caller that points at this" care), and by
`CreateQualificationRun` as the backstop for a template bound after the
fact.

`template` gets `target_owner`/`target_name`/`base` back. They are
stored empty rather than NULL, which is what makes
`ensureTemplateBindingColumns` a plain probe-then-`ALTER TABLE ADD
COLUMN` in the direction every other migration in `store.go` goes: a
database from before templates could carry a target has the columns
already, and its values are exactly the bindings they now mean, so it
keeps them instead of dropping them, while one from the era in between
gets them added empty and every template in it stays unbound.
`ensureTemplateNoTargetColumns`, which dropped them, is gone -- left in
place it would have dropped the new ones on the next start.

Frontend: `TemplateOverlay` gains the two fields, optional and labelled
so, with a cleared repo meaning "unbind"; `TemplatesList` chips the
binding on the rows that have one and searches it; `ScheduleOverlay`
replaces its repo and base fields with a line naming where the schedule
will fire once a bound template is chosen; `SuiteOverlay`'s picker
labels a bound item with its repo; and `RepoReleases`' qualification
picker leaves out templates bound elsewhere, matching what the API will
accept.

## Update: the non-task list pages get an order of their own (grain/task-327)

The two sections above hand the schedules and templates pages
`TaskList.jsx`'s toolbar but not its rows: no drag handle, because
neither list has a backlog to reorder, and so also no "Select all" row
under the toolbar to space the first row away from it. Beside the task
list the result read as unfinished -- rows crammed against the
filter/sort panel, and nothing at the left of a row where a task row
carries its state and its handle. The repos page had the same two gaps
plus no sort menu at all, and the suites page had no toolbar whatsoever.

So all four get a *display* order, `ui/src/listOrder.js`: a list of ids
per page in `localStorage` (`grain.list-order.<page>`), the same
browser-local treatment the board's columns and the theme mode already
get, and deliberately nothing the daemon ever sees -- dragging a repo to
the top does not change what runs next, it changes where the row is for
the person who dragged it. It is each page's default sort ("Custom
order", the slot `TaskList`'s "Backlog order" occupies), falling back to
the alphabetical comparator each list already sorted by for every item
nobody has dragged, so a list nobody has touched looks exactly as it did
-- with handles on it. Picking any other order withdraws the handles,
the rule `TaskList` already applies to the backlog: there is nowhere for
a drop to land in an order the page computes.

`ReorderableList` in `ListPrimitives.jsx` is the gesture itself (the
`<ul>`, the `<li>`s, the drop highlight and the trailing "drop at the
end" zone), with each page rendering its own row through a render prop
so the handle sits inside the row's own flex line. `--list-rows-gap` in
`style.css` is the missing "Select all" row's vertical space handed back
to the four lists as a margin. And the repos list gains a status dot
ahead of each name (`state.js`'s `repoActivity`): running when an agent
is working there this moment, an open ring when work is outstanding but
none of it moving, grey when every task there is closed -- so a repo
with something happening in it is visible without reading its counts.
