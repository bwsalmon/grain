import { useState } from "react";
import { Button, Chip, FormControl, InputLabel, MenuItem, Select, TextField, Typography } from "@mui/material";
import { knownRepos } from "../state.js";
import ScheduleOverlay from "./ScheduleOverlay.jsx";

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
// template, paused state -- with TaskList's own search-and-sort toolbar,
// TemplatesList's own precedent for the same split applied here. Nothing
// about any one schedule (description, base branch, reads, capabilities,
// editing, pausing, or deleting) lives here any more; all of that moved
// into ScheduleOverlay, opened either by the "+ New schedule" button or
// by clicking a row, the same NewTaskOverlay/DetailOverlay split
// TemplatesList/TemplateOverlay already draw.
export default function SchedulesList({ schedules, templates = [], config, tasks, onRefresh, showError }) {
  const [search, setSearch] = useState("");
  const [sortBy, setSortBy] = useState("title");
  const [showNew, setShowNew] = useState(false);
  const [editing, setEditing] = useState(null);
  const repoOptions = knownRepos(config, tasks);

  const q = search.trim().toLowerCase();
  const matches = (s) => q === "" || s.title.toLowerCase().includes(q) || s.repo.toLowerCase().includes(q);

  const visible = schedules.filter(matches).sort(SORTS[sortBy].cmp);

  return (
    <main>
      <div className="content-header">
        <Typography variant="h6" component="h2" sx={{ m: 0, fontSize: "1rem", fontWeight: 600 }}>Scheduled tasks</Typography>
        <span className="count">{visible.length}</span>
        <Button variant="contained" size="small" sx={{ ml: "auto" }} onClick={() => setShowNew(true)}>+ New schedule</Button>
      </div>
      {schedules.length > 0 && (
        <div className="task-list-toolbar">
          <TextField
            size="small"
            placeholder="Search schedules…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            sx={{ flex: 1, maxWidth: 320 }}
          />
          <FormControl size="small" sx={{ minWidth: 170 }}>
            <InputLabel id="schedule-sort-label">Sort</InputLabel>
            <Select
              labelId="schedule-sort-label"
              label="Sort"
              value={sortBy}
              onChange={(e) => setSortBy(e.target.value)}
            >
              {Object.entries(SORTS).map(([id, { label }]) => <MenuItem key={id} value={id}>{label}</MenuItem>)}
            </Select>
          </FormControl>
        </div>
      )}
      <ul className="schedules-list">
        {visible.map((s) => (
          <li className="schedule-row" key={s.id} onClick={() => setEditing(s)}>
            <div className="schedule-summary">
              <span className="schedule-title">{s.title}</span>
              <Chip size="small" label={s.repo} />
              <Chip size="small" label={describeRecurrence(s.recurrence)} />
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
      {schedules.length === 0 && <p className="empty">No scheduled tasks.</p>}
      {schedules.length > 0 && visible.length === 0 && <p className="empty">No schedules match your search.</p>}

      {showNew && (
        <ScheduleOverlay repoOptions={repoOptions} templates={templates} config={config} onClose={() => setShowNew(false)} onSaved={onRefresh} showError={showError} />
      )}
      {editing && (
        <ScheduleOverlay schedule={editing} repoOptions={repoOptions} templates={templates} config={config} onClose={() => setEditing(null)} onSaved={onRefresh} showError={showError} />
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
