import { useState } from "react";
import { Button, Chip } from "@mui/material";
import { knownRepos } from "../state.js";
import ScheduleOverlay from "./ScheduleOverlay.jsx";
import { ListEmpty, ListHeader, ListSearchField, ListSortSelect, ListToolbar } from "./ListPrimitives.jsx";

// SORTS mirrors TaskList's own toolbar Select (bwsalmon/agents#547): a
// schedule has no backlog order to sort by (it is never itself dispatched
// against other schedules), so "manual" drops out and Title (A-Z) takes
// its place as the default, TemplatesList's own precedent for a list with
// no backlog of its own.
const SORTS = {
  title: { label: "Title (A–Z)", cmp: (a, b) => a.title.localeCompare(b.title) },
  newest: { label: "Newest first", cmp: (a, b) => new Date(b.createdAt || 0) - new Date(a.createdAt || 0) },
  oldest: { label: "Oldest first", cmp: (a, b) => new Date(a.createdAt || 0) - new Date(b.createdAt || 0) },
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
export default function SchedulesList({ schedules, templates = [], suites = [], config, tasks, openScheduleId, onOpenSchedule, onRefresh, showError }) {
  const [search, setSearch] = useState("");
  const [sortBy, setSortBy] = useState("title");
  const [showNew, setShowNew] = useState(false);
  const repoOptions = knownRepos(config, tasks);
  const editing = schedules.find((s) => s.id === openScheduleId) || null;

  const q = search.trim().toLowerCase();
  const matches = (s) => q === "" || s.title.toLowerCase().includes(q) || s.repo.toLowerCase().includes(q);

  const visible = schedules.filter(matches).sort(SORTS[sortBy].cmp);

  return (
    <main>
      <ListHeader
        title="Schedules"
        count={visible.length}
        action={<Button variant="contained" size="small" sx={{ ml: "auto" }} onClick={() => setShowNew(true)}>+ New schedule</Button>}
      />
      {schedules.length > 0 && (
        <ListToolbar>
          <ListSearchField placeholder="Search schedules…" value={search} onChange={setSearch} />
          <ListSortSelect id="schedule-sort" value={sortBy} onChange={setSortBy} options={SORTS} />
        </ListToolbar>
      )}
      <ul className="schedules-list">
        {visible.map((s) => (
          <li className="schedule-row" key={s.id} onClick={() => onOpenSchedule(s.id)}>
            <div className="schedule-summary">
              <span className="schedule-title">{s.title}</span>
              <Chip size="small" label={s.repo} />
              <Chip size="small" label={describeRecurrence(s.recurrence)} />
              {s.suiteId && <Chip size="small" variant="outlined" label={`Suite: ${s.suiteName || s.suiteId}`} />}
              {s.templateId && <Chip size="small" variant="outlined" label={`Template: ${s.templateName || s.templateId}`} />}
              {!s.enabled && <Chip size="small" color="error" label="Paused" />}
            </div>
            <div className="schedule-meta hint">
              Next run {formatWhen(s.nextRunAt)}
              {s.lastRunAt && <> · last ran {formatWhen(s.lastRunAt)}</>}
            </div>
          </li>
        ))}
      </ul>
      {schedules.length === 0 && <ListEmpty>No schedules yet.</ListEmpty>}
      {schedules.length > 0 && visible.length === 0 && <ListEmpty>No schedules match your search.</ListEmpty>}

      {showNew && (
        <ScheduleOverlay repoOptions={repoOptions} templates={templates} suites={suites} config={config} onClose={() => setShowNew(false)} onSaved={onRefresh} showError={showError} />
      )}
      {editing && (
        <ScheduleOverlay schedule={editing} repoOptions={repoOptions} templates={templates} suites={suites} config={config} onClose={() => onOpenSchedule(null)} onSaved={onRefresh} showError={showError} />
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
    case "everyNHours": return `every ${r.everyNHours}h`;
    case "daily": return `daily at ${r.timeOfDay}`;
    case "weekly": return `${capitalize(r.weekday)}s at ${r.timeOfDay}`;
    case "monthly": return `monthly on day ${r.dayOfMonth} at ${r.timeOfDay}`;
    default: return r.kind;
  }
}

function formatWhen(iso) {
  return new Date(iso).toLocaleString();
}
