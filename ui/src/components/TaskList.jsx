import { useState } from "react";
import { Checkbox, Chip, FormControlLabel } from "@mui/material";
import DragIndicatorIcon from "@mui/icons-material/DragIndicator";
import { STATE_LABELS, capabilityName, completionPhase } from "../state.js";
import { ListEmpty, ListHeader, ListSearchField, ListSortSelect, ListToolbar } from "./ListPrimitives.jsx";
import StateDot, { isLiveRunning } from "./StateDot.jsx";

const FILTER_TITLES = { all: "All tasks", blocked: "Blocked" };

// SORTS is every order the toolbar's own Select offers, keyed by what it
// stores in sortBy. "manual" is the backlog order the store itself
// keeps (Store.Reorder, bwsalmon/agents#476) and the only one dragging
// a row makes sense against -- picking any other one is a display-only
// reordering of the same tasks, so drag-and-drop is disabled outside it
// rather than let a drop silently reorder the backlog underneath a view
// that no longer matches it.
const SORTS = {
  manual: { label: "Backlog order", cmp: null },
  newest: { label: "Newest first", cmp: (a, b) => new Date(b.createdAt || 0) - new Date(a.createdAt || 0) },
  oldest: { label: "Oldest first", cmp: (a, b) => new Date(a.createdAt || 0) - new Date(b.createdAt || 0) },
  title: { label: "Title (A–Z)", cmp: (a, b) => a.title.localeCompare(b.title) },
};

// A stacked task -- the merge queue's own automatic fix for another
// task's pull request (bwsalmon/agents#378) -- is not new work of its
// own, so it is nested under the task named by generatedFrom instead of
// listed as a separate row, as long as that task also passes the
// current filter. One that doesn't (its parent fell out of the filtered
// view, or the parent it names is gone) falls back to a plain row of
// its own, so it is never silently dropped from the list.
function groupByStack(tasks, matches) {
  const topLevel = [];
  const orphans = [];
  for (const t of tasks) {
    if (!matches(t)) continue;
    if (t.stacked && t.generatedFrom) orphans.push(t);
    else topLevel.push(t);
  }
  const topLevelIds = new Set(topLevel.map((t) => t.id));
  const children = new Map();
  for (const c of orphans) {
    if (!topLevelIds.has(c.generatedFrom)) {
      topLevel.push(c);
      continue;
    }
    const list = children.get(c.generatedFrom) || [];
    list.push(c);
    children.set(c.generatedFrom, list);
  }
  return { topLevel, children };
}

export default function TaskList({ tasks, stateFilter, config, onOpenTask, selected, onToggleSelect, onSelectAll, onReorder }) {
  // search and sortBy are local, not lifted to App.jsx alongside
  // stateFilter/repoFilter: unlike those two, they are a refinement of
  // the list currently on screen rather than a standing question about
  // "which tasks", so it is fine for them to reset the next time this
  // component mounts (switching to the repos or schedules view and back)
  // instead of surviving the trip the way repoFilter deliberately does.
  const [search, setSearch] = useState("");
  const [sortBy, setSortBy] = useState("manual");

  // showClosedOverride is null until this list's own "Show closed tasks"
  // checkbox is touched -- until then, showClosed instead follows
  // config.showClosedByDefault (bwsalmon/agents#537's deployment-wide
  // default), even though config itself only arrives after this
  // component's first render (App.jsx fetches it once, asynchronously).
  // Once a viewer picks a value here it wins for the rest of this list's
  // life, same "local refinement" treatment as search/sortBy above.
  const [showClosedOverride, setShowClosedOverride] = useState(null);
  const showClosed = showClosedOverride !== null ? showClosedOverride : !!config?.showClosedByDefault;

  const q = search.trim().toLowerCase();
  const matches = (t) => {
    if (stateFilter === "blocked" ? !t.blocked : stateFilter !== "all" && t.state !== stateFilter) return false;
    // Closed tasks are hidden everywhere except the "Closed" filter
    // itself, unless showClosed turns that back on -- viewing "Closed"
    // is always a request to see them, so the toggle has no say there.
    if (!showClosed && stateFilter !== "closed" && t.state === "closed") return false;
    return q === "" || t.title.toLowerCase().includes(q) || String(t.id).toLowerCase().includes(q);
  };
  const closedCount = tasks.filter((t) => t.state === "closed").length;

  const reorderEnabled = !!onReorder && sortBy === "manual";
  const cmp = SORTS[sortBy].cmp;
  const sortedTasks = cmp ? [...tasks].sort(cmp) : tasks;

  const { topLevel, children } = groupByStack(sortedTasks, matches);
  const visibleIds = topLevel.flatMap((t) => [t.id, ...(children.get(t.id) || []).map((c) => c.id)]);
  const allSelected = visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));

  const title = FILTER_TITLES[stateFilter] || STATE_LABELS[stateFilter] || stateFilter;

  // dragIds is every id being dragged right now, in their own current
  // backlog order -- a single row not part of the current selection
  // drags alone; a row that is part of a multi-selection drags the whole
  // selection as a block (bwsalmon/agents#476). null means nothing is
  // being dragged. overId is purely the drop-target highlight and never
  // drives the actual move -- that only happens in onDrop.
  const [dragIds, setDragIds] = useState(null);
  const [overId, setOverId] = useState(null);

  // dropOn resolves a drop at targetId (or at the very end, when
  // targetId is null) to the two backlog neighbours -- among the tasks
  // currently visible under this filter -- the drop landed between, and
  // hands them to onReorder as afterId/beforeId. The store places the
  // dragged tasks between whatever those two names resolve to in the
  // *full*, unfiltered backlog, which is what makes a drag inside a
  // filtered view still land correctly relative to tasks the filter is
  // hiding: dropping at the very top of a filtered view has no
  // preceding job, so it goes just before the following one instead --
  // the same rule the issue itself asks for.
  const dropOn = (targetId) => {
    if (!dragIds) return;
    const dragging = new Set(dragIds);
    const visible = topLevel.map((t) => t.id).filter((id) => !dragging.has(id));
    const idx = targetId === null ? visible.length : visible.indexOf(targetId);
    onReorder(dragIds, idx > 0 ? visible[idx - 1] : null, idx < visible.length ? visible[idx] : null);
    setDragIds(null);
    setOverId(null);
  };

  const startDrag = (t) => {
    const ids = selected.has(t.id) && selected.size > 1 ? selected : new Set([t.id]);
    setDragIds(topLevel.filter((x) => ids.has(x.id)).map((x) => x.id));
  };

  return (
    <main>
      <ListHeader title={title} count={visibleIds.length} />
      {tasks.length > 0 && (
        <ListToolbar>
          <ListSearchField placeholder="Search tasks…" value={search} onChange={setSearch} />
          <ListSortSelect id="task-sort" value={sortBy} onChange={setSortBy} options={SORTS} />
          {closedCount > 0 && stateFilter !== "closed" && (
            <FormControlLabel
              control={(
                <Checkbox
                  size="small"
                  checked={showClosed}
                  onChange={(e) => setShowClosedOverride(e.target.checked)}
                />
              )}
              label="Show closed tasks"
            />
          )}
        </ListToolbar>
      )}
      {visibleIds.length > 0 && (
        <div className="select-all">
          <FormControlLabel
            control={(
              <Checkbox
                size="small"
                checked={allSelected}
                onChange={(e) => onSelectAll(visibleIds, e.target.checked)}
              />
            )}
            label="Select all"
          />
        </div>
      )}
      <ul className="task-list">
        {topLevel.map((t) => (
          <li
            key={t.id}
            className={overId === t.id && dragIds && !dragIds.includes(t.id) ? "task-drop-target" : undefined}
            draggable={reorderEnabled}
            onDragStart={() => reorderEnabled && startDrag(t)}
            onDragEnd={() => { setDragIds(null); setOverId(null); }}
            onDragOver={(e) => {
              if (!dragIds || dragIds.includes(t.id)) return;
              e.preventDefault();
              setOverId(t.id);
            }}
            onDrop={(e) => { e.preventDefault(); dropOn(t.id); }}
          >
            <TaskRow t={t} config={config} onOpenTask={onOpenTask} selected={selected} onToggleSelect={onToggleSelect}
              draggable={reorderEnabled} dragging={dragIds?.includes(t.id) ?? false} />
            {children.has(t.id) && (
              <ul className="task-sublist">
                {children.get(t.id).map((c) => (
                  <li key={c.id}>
                    <TaskRow t={c} config={config} onOpenTask={onOpenTask} selected={selected} onToggleSelect={onToggleSelect}
                      reserveDragSpace={reorderEnabled} />
                  </li>
                ))}
              </ul>
            )}
          </li>
        ))}
        {reorderEnabled && dragIds && (
          <li
            className={`task-drop-end${overId === "__end__" ? " task-drop-target" : ""}`}
            onDragOver={(e) => { e.preventDefault(); setOverId("__end__"); }}
            onDrop={(e) => { e.preventDefault(); dropOn(null); }}
          />
        )}
      </ul>
      {topLevel.length === 0 && (
        <ListEmpty>
          {q
            ? "No tasks match your search."
            : !showClosed && closedCount > 0 && stateFilter !== "closed"
              ? "No tasks in this state (closed tasks are hidden)."
              : "No tasks in this state."}
        </ListEmpty>
      )}
    </main>
  );
}

// TaskRow is exported so any other view that lists tasks -- the repo
// pane's own per-repo sublist (bwsalmon/agents#474) is the first -- can
// render a row identical to this one instead of re-deriving the badge
// and chip markup. Selection and dragging are both opt-in: a caller with
// nowhere to put a batch-actions bar or a reorderable list (the repo
// pane, again) just omits onToggleSelect/draggable and gets a plain row
// with no checkbox or drag handle rather than a dead one.
//
// reserveDragSpace is for the third case: a row that sits *inside* a
// reorderable list but has no backlog position to drag it to -- a
// stacked task, which always renders under the task it fixes. Dropping
// the handle entirely would slide such a row's contents a handle's width
// to the left of every draggable row around it, so the nested rows ended
// up hanging left of their own parent instead of indented under it. It
// keeps the handle's column, empty.
export function TaskRow({ t, config, onOpenTask, selected, onToggleSelect, draggable, dragging, reserveDragSpace }) {
  const phase = completionPhase(t);
  return (
    <div className={`task-row${dragging ? " task-row-dragging" : ""}`} onClick={() => onOpenTask(t.id)}>
      {draggable ? (
        <DragIndicatorIcon
          className="task-drag-handle"
          fontSize="small"
          titleAccess="Drag to reorder"
          onClick={(e) => e.stopPropagation()}
        />
      ) : reserveDragSpace ? (
        <span className="task-drag-spacer" aria-hidden="true" />
      ) : null}
      {onToggleSelect && (
        <Checkbox
          size="small"
          className="task-select"
          checked={selected.has(t.id)}
          onClick={(e) => e.stopPropagation()}
          onChange={() => onToggleSelect(t.id)}
          inputProps={{ "aria-label": `Select ${t.id}` }}
        />
      )}
      <span
        className={`badge badge-icon badge-${t.state}${isLiveRunning(t.state) ? " badge-mark" : ""}`}
        title={STATE_LABELS[t.state] || t.state}
      >
        <StateDot state={t.state} title={STATE_LABELS[t.state] || t.state} />
      </span>
      <span className="task-number">{t.id}</span>
      <span className="task-title">{t.title}</span>
      <span className="chips">
        {t.scheduled && <Chip size="small" className="chip-scheduled" title="filed automatically by a schedule" label="scheduled" />}
        {t.configuration ? (
          <Chip size="small" className="chip-interactive" title="grain's own configuration agent" label="configuration" />
        ) : t.interactive ? (
          <Chip size="small" className="chip-interactive" title="a live chat, not a background task" label="interactive" />
        ) : null}
        {t.repo && <Chip size="small" label={t.repo} />}
        {(t.reads || []).map((repo) => (
          <Chip key={repo} size="small" variant="outlined" title="read-only" label={`${repo} (read)`} />
        ))}
        {t.capabilities.map((id) => (
          <Chip key={id} size="small" label={capabilityName(config, id)} />
        ))}
      </span>
      {phase && <Chip size="small" color={phase.color} title={phase.title} label={phase.label} />}
      {t.blocked && (
        <Chip size="small" color="error" title={`Waiting on ${t.blockedBy.join(", ")}`} label="Blocked" />
      )}
    </div>
  );
}
