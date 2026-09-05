import { useState } from "react";
import { Button, Chip } from "@mui/material";
import { knownRepos } from "../state.js";
import { useTimeZone } from "../TimeZoneContext.jsx";
import { formatDateTime } from "../time.js";
import { useListOrder } from "../listOrder.js";
import ScheduleOverlay from "./ScheduleOverlay.jsx";
import {
  ListEmpty,
  ListHeader,
  ListSearchField,
  ListSortSelect,
  ListToolbar,
  ReorderableList,
} from "./ListPrimitives.jsx";
import ItemGlyph from "./ItemGlyph.jsx";

// SORTS mirrors TaskList's own toolbar Select (bwsalmon/agents#547): a
// schedule has no backlog order to sort by (it is never itself dispatched
// against other schedules), so the task list's "Backlog order" drops out
// and a custom order of this browser's own takes its place as the
// default (listOrder.js, grain/task-327), falling back to Title (A-Z) --
// what this list was sorted by before it could be dragged --  for every
// schedule nobody has moved. TemplatesList draws the same split.
//
// "Next run" is the one order here that is about a schedule rather than
// about its name: what fires next, which is the question a schedules
// page is usually open to answer.
const byTitle = (a, b) => a.title.localeCompare(b.title);
const SORTS = {
  custom: { label: "Custom order", cmp: byTitle },
  title: { label: "Title (A–Z)", cmp: byTitle },
  next: {
    label: "Next run",
    cmp: (a, b) =>
      new Date(a.nextRunAt || 0) - new Date(b.nextRunAt || 0) || byTitle(a, b),
  },
  newest: {
    label: "Newest first",
    cmp: (a, b) => new Date(b.createdAt || 0) - new Date(a.createdAt || 0),
  },
  oldest: {
    label: "Oldest first",
    cmp: (a, b) => new Date(a.createdAt || 0) - new Date(b.createdAt || 0),
  },
};

// SchedulesList is the schedules page's main pane (bwsalmon/agents#547):
// a flat list of every schedule's key details -- title, repo, cadence,
// the suite or template it fires, paused state -- with TaskList's own
// search-and-sort toolbar,
// TemplatesList's own precedent for the same split applied here. Nothing
// about any one schedule (description, base branch, reads, capabilities,
// editing, pausing, or deleting) lives here any more; all of that moved
// into ScheduleOverlay, opened either by the "+ New schedule" button or
// by clicking a row, the same NewTaskOverlay/DetailOverlay split
// TemplatesList/TemplateOverlay already draw.
//
// Which schedule is open is App.jsx's state (openScheduleId), not this
// component's, so the URL can name it -- /schedules/sched-1, the way
// /tasks/42 already names an open task (grain/task-139). Everything
// else here stays local: search, sort, and the blank "+ New schedule"
// pane, which has no schedule to name yet.
export default function SchedulesList({
  schedules,
  templates = [],
  suites = [],
  config,
  tasks,
  openScheduleId,
  onOpenSchedule,
  onRefresh,
  showError,
}) {
  const [search, setSearch] = useState("");
  const [sortBy, setSortBy] = useState("custom");
  const [showNew, setShowNew] = useState(false);
  const zone = useTimeZone();
  const repoOptions = knownRepos(config, tasks);
  const editing = schedules.find((s) => s.id === openScheduleId) || null;

  const q = search.trim().toLowerCase();
  const matches = (s) =>
    q === "" ||
    s.title.toLowerCase().includes(q) ||
    s.repo.toLowerCase().includes(q);

  // Every schedule, not just the ones the search leaves showing: what a
  // drag stores has to know where the hidden rows sit too (listOrder.js).
  const [ordered, move] = useListOrder(
    "schedules",
    schedules,
    (s) => s.id,
    byTitle,
  );
  const sorted =
    sortBy === "custom" ? ordered : [...schedules].sort(SORTS[sortBy].cmp);
  const visible = sorted.filter(matches);

  return (
    <main>
      <ListHeader
        title="Schedules"
        icon={<ItemGlyph kind="schedules" size={20} />}
        count={visible.length}
        action={
          <Button
            variant="contained"
            size="small"
            sx={{ ml: "auto" }}
            onClick={() => setShowNew(true)}
          >
            + New schedule
          </Button>
        }
      />
      {schedules.length > 0 && (
        <ListToolbar>
          <ListSearchField
            placeholder="Search schedules…"
            value={search}
            onChange={setSearch}
          />
          <ListSortSelect
            id="schedule-sort"
            value={sortBy}
            onChange={setSortBy}
            options={SORTS}
          />
        </ListToolbar>
      )}
      <ReorderableList
        className="schedules-list"
        items={visible}
        idOf={(s) => s.id}
        reorder={sortBy === "custom" ? move : null}
      >
        {(s, { handle, dragging }) => (
          <div
            className={`schedule-row${dragging ? " task-row-dragging" : ""}`}
            onClick={() => onOpenSchedule(s.id)}
          >
            {/* Two lines of text, so the handle is a column beside them
                rather than the first thing on the summary line -- the
                same place it sits on a one-line row, held there by
                .schedule-row's own flex (style.css). */}
            {handle}
            <div className="schedule-body">
              <div className="schedule-summary">
                <span className="schedule-title">{s.title}</span>
                <Chip size="small" label={s.repo} />
                <Chip size="small" label={describeRecurrence(s.recurrence)} />
                {s.suiteId && (
                  <Chip
                    size="small"
                    variant="outlined"
                    label={`Suite: ${s.suiteName || s.suiteId}`}
                  />
                )}
                {s.templateId && (
                  <Chip
                    size="small"
                    variant="outlined"
                    label={`Template: ${s.templateName || s.templateId}`}
                  />
                )}
                {!s.enabled && (
                  <Chip size="small" color="error" label="Paused" />
                )}
              </div>
              <div className="schedule-meta hint">
                Next run {formatWhen(s.nextRunAt, zone)}
                {s.lastRunAt && (
                  <> · last ran {formatWhen(s.lastRunAt, zone)}</>
                )}
              </div>
            </div>
          </div>
        )}
      </ReorderableList>
      {schedules.length === 0 && <ListEmpty>No schedules yet.</ListEmpty>}
      {schedules.length > 0 && visible.length === 0 && (
        <ListEmpty>No schedules match your search.</ListEmpty>
      )}

      {showNew && (
        <ScheduleOverlay
          repoOptions={repoOptions}
          templates={templates}
          suites={suites}
          config={config}
          onClose={() => setShowNew(false)}
          onSaved={onRefresh}
          showError={showError}
        />
      )}
      {editing && (
        <ScheduleOverlay
          schedule={editing}
          repoOptions={repoOptions}
          templates={templates}
          suites={suites}
          config={config}
          onClose={() => onOpenSchedule(null)}
          onSaved={onRefresh}
          showError={showError}
        />
      )}
    </main>
  );
}

function capitalize(s) {
  return s[0].toUpperCase() + s.slice(1);
}

// describeRecurrence renders a schedule's cadence the way a human reads
// it -- the row summary's own short form of what RecurrenceFields lets a
// human set.
function describeRecurrence(r) {
  if (!r) return "";
  switch (r.kind) {
    case "everyNHours":
      return `every ${r.everyNHours}h`;
    case "daily":
      return `daily at ${r.timeOfDay}`;
    case "weekly":
      return `${capitalize(r.weekday)}s at ${r.timeOfDay}`;
    case "monthly":
      return `monthly on day ${r.dayOfMonth} at ${r.timeOfDay}`;
    default:
      return r.kind;
  }
}

function formatWhen(iso, zone) {
  return formatDateTime(iso, zone);
}
