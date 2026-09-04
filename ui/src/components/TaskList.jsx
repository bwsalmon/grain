import { useState } from "react";
import { Button, Checkbox, Chip, FormControlLabel } from "@mui/material";
import DragIndicatorIcon from "@mui/icons-material/DragIndicator";
import {
  STATE_LABELS,
  capabilityName,
  completionPhase,
  runActivity,
  stackedChip,
  stateLabel,
} from "../state.js";
import {
  SORTS,
  filterViews,
  matchesFilters,
  sortTasks,
} from "../taskFilters.js";
import {
  ListEmpty,
  ListFilterSelect,
  ListHeader,
  ListSearchField,
  ListSortSelect,
  ListToolbar,
} from "./ListPrimitives.jsx";
import StateDot, { isLiveRunning } from "./StateDot.jsx";

const FILTER_TITLES = { all: "All tasks", blocked: "Blocked" };

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

export default function TaskList({
  tasks,
  stateFilter,
  config,
  narrowing,
  onNarrow,
  onOpenTask,
  selected,
  onToggleSelect,
  onSelectAll,
  onReorder,
}) {
  // The search text, the sort order and the attribute filters are all
  // App's now, and the URL's (grain/task-317): they used to be local
  // state here, on the grounds that a refinement of the list on screen
  // could reset the next time this component mounted, but a list
  // narrowed by half a dozen menus at once is a view somebody wants to
  // come back to and to send to someone else -- so it lives in the
  // query string, where every other piece of navigable state already
  // lives (paths.js), and survives switching views and back the way
  // stateFilter does.
  //
  // narrowing.filters is keyed by FILTERS entry id; a missing key, or
  // "", is that attribute left out of the question. onNarrow takes a
  // patch of the narrowing, so each control below only says what it
  // changes.
  const { search, sortBy, filters } = narrowing;
  const setSearch = (value) => onNarrow({ search: value });
  const setSortBy = (value) => onNarrow({ sortBy: value });
  const setFilter = (id, value) =>
    onNarrow({ filters: { ...filters, [id]: value } });

  // showClosedOverride is null until this list's own "Show closed tasks"
  // checkbox is touched -- until then, showClosed instead follows
  // config.showClosedByDefault (bwsalmon/agents#537's deployment-wide
  // default), even though config itself only arrives after this
  // component's first render (App.jsx fetches it once, asynchronously).
  // Once a viewer picks a value here it wins for the rest of this list's
  // life. It stays local where search/sortBy/filters no longer are: it
  // is not a narrowing somebody would link to but a standing rule about
  // what this list shows at all, and its default is the deployment's
  // (config.showClosedByDefault) rather than the URL's.
  const [showClosedOverride, setShowClosedOverride] = useState(null);
  const showClosed =
    showClosedOverride !== null
      ? showClosedOverride
      : !!config?.showClosedByDefault;

  const q = search.trim().toLowerCase();

  // inState is what this list is *about* before any of the toolbar's own
  // refinements: the sidebar's question plus the closed-task rule. It is
  // separated out because the toolbar's filter menus are built from the
  // tasks that pass it -- a menu should not offer a repo whose every
  // task the current state filter is already hiding.
  const inState = (t) => {
    if (
      stateFilter === "blocked"
        ? !t.blocked
        : stateFilter !== "all" && t.state !== stateFilter
    )
      return false;
    // Closed tasks are hidden everywhere except the "Closed" filter
    // itself, unless showClosed turns that back on -- viewing "Closed"
    // is always a request to see them, so the toggle has no say there.
    return showClosed || stateFilter === "closed" || t.state !== "closed";
  };
  const inStateTasks = tasks.filter(inState);

  // Which menus the toolbar offers and what is picked in each -- built
  // from the tasks the state filter admits, so the menus never offer a
  // value that would come back empty (taskFilters.js).
  const views = filterViews(inStateTasks, filters, config);
  const activeFilters = views.filter((v) => v.value !== "");
  const narrowed = q !== "" || activeFilters.length > 0;

  const matches = (t) => inState(t) && matchesFilters(t, activeFilters, q);
  const closedCount = tasks.filter((t) => t.state === "closed").length;

  const reorderEnabled = !!onReorder && sortBy === "manual";
  const sortedTasks = sortTasks(tasks, sortBy);

  const { topLevel, children } = groupByStack(sortedTasks, matches);
  const visibleIds = topLevel.flatMap((t) => [
    t.id,
    ...(children.get(t.id) || []).map((c) => c.id),
  ]);
  const allSelected =
    visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));

  const title =
    FILTER_TITLES[stateFilter] || STATE_LABELS[stateFilter] || stateFilter;

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
    onReorder(
      dragIds,
      idx > 0 ? visible[idx - 1] : null,
      idx < visible.length ? visible[idx] : null,
    );
    setDragIds(null);
    setOverId(null);
  };

  // clearNarrowing puts the list back to everything the sidebar admits
  // -- the one gesture for getting out of a search and half a dozen
  // menus, which is otherwise as many gestures as it took to get in.
  // The sort is not part of it: an order is not something a list has to
  // be rescued from.
  const clearNarrowing = () => onNarrow({ search: "", filters: {} });

  const startDrag = (t) => {
    const ids =
      selected.has(t.id) && selected.size > 1 ? selected : new Set([t.id]);
    setDragIds(topLevel.filter((x) => ids.has(x.id)).map((x) => x.id));
  };

  return (
    <main>
      <ListHeader title={title} count={visibleIds.length} />
      {tasks.length > 0 && (
        <ListToolbar>
          <ListSearchField
            placeholder="Search tasks…"
            value={search}
            onChange={setSearch}
          />
          <ListSortSelect
            id="task-sort"
            value={sortBy}
            onChange={setSortBy}
            options={SORTS}
          />
          {views
            .filter((v) => v.shown)
            .map(({ f, options, value }) => (
              <ListFilterSelect
                key={f.id}
                id={`task-filter-${f.id}`}
                label={f.label}
                anyLabel={f.anyLabel}
                value={value}
                onChange={(v) => setFilter(f.id, v)}
                options={options}
              />
            ))}
          {narrowed && (
            <Button
              size="small"
              title="Clear the search and every filter"
              onClick={clearNarrowing}
            >
              Clear
            </Button>
          )}
          {closedCount > 0 && stateFilter !== "closed" && (
            <FormControlLabel
              control={
                <Checkbox
                  size="small"
                  checked={showClosed}
                  onChange={(e) => setShowClosedOverride(e.target.checked)}
                />
              }
              label="Show closed tasks"
            />
          )}
        </ListToolbar>
      )}
      {visibleIds.length > 0 && (
        <div className="select-all">
          <FormControlLabel
            control={
              <Checkbox
                size="small"
                checked={allSelected}
                onChange={(e) => onSelectAll(visibleIds, e.target.checked)}
              />
            }
            label="Select all"
          />
        </div>
      )}
      <ul className="task-list">
        {topLevel.map((t) => (
          <li
            key={t.id}
            className={
              overId === t.id && dragIds && !dragIds.includes(t.id)
                ? "task-drop-target"
                : undefined
            }
            draggable={reorderEnabled}
            onDragStart={() => reorderEnabled && startDrag(t)}
            onDragEnd={() => {
              setDragIds(null);
              setOverId(null);
            }}
            onDragOver={(e) => {
              if (!dragIds || dragIds.includes(t.id)) return;
              e.preventDefault();
              setOverId(t.id);
            }}
            onDrop={(e) => {
              e.preventDefault();
              dropOn(t.id);
            }}
          >
            <TaskRow
              t={t}
              config={config}
              onOpenTask={onOpenTask}
              selected={selected}
              onToggleSelect={onToggleSelect}
              draggable={reorderEnabled}
              dragging={dragIds?.includes(t.id) ?? false}
            />
            {children.has(t.id) && (
              <ul className="task-sublist">
                {children.get(t.id).map((c) => (
                  <li key={c.id}>
                    <TaskRow
                      t={c}
                      config={config}
                      onOpenTask={onOpenTask}
                      selected={selected}
                      onToggleSelect={onToggleSelect}
                      dragPlaceholder={reorderEnabled}
                      nested
                    />
                  </li>
                ))}
              </ul>
            )}
          </li>
        ))}
        {reorderEnabled && dragIds && (
          <li
            className={`task-drop-end${overId === "__end__" ? " task-drop-target" : ""}`}
            onDragOver={(e) => {
              e.preventDefault();
              setOverId("__end__");
            }}
            onDrop={(e) => {
              e.preventDefault();
              dropOn(null);
            }}
          />
        )}
      </ul>
      {topLevel.length === 0 && (
        <ListEmpty>
          {q
            ? "No tasks match your search."
            : activeFilters.length > 0
              ? "No tasks match these filters."
              : !showClosed && closedCount > 0 && stateFilter !== "closed"
                ? "No tasks in this state (closed tasks are hidden)."
                : "No tasks in this state."}
        </ListEmpty>
      )}
    </main>
  );
}

// TaskRow is exported so any other view that lists tasks can render a
// row identical to this one instead of re-deriving the badge and chip
// markup. Selection and dragging are both opt-in: a caller with nowhere
// to put a batch-actions bar or a reorderable list just omits
// onToggleSelect/draggable and gets a plain row with no checkbox or drag
// handle rather than a dead one.
//
// nested says this row is already sitting in a .task-sublist under the
// task it was generated from, which is the only thing that makes a
// stacked task self-explaining -- so it is what decides whether the
// "merge fix"/"review" chip below is worth its space. Callers that list tasks
// flat (groupByStack's own fallback for a stacked task whose parent is
// filtered out or gone) leave it off and get the chip.
//
// A row carries no "show the prompt" button of its own any more: the
// whole prompt an agent was handed is a detail about one task, so it is
// reached from that task's own page (DetailOverlay's Prompt button)
// rather than from every row of every list a task can appear in.
//
// dragPlaceholder is for a row with no handle of its own in a list where
// the other rows have one -- a stacked merge fix or review, neither of
// which is ever reordered, since both are filed ahead of the backlog.
// Without it that row's badge, number and title would each sit a handle's
// width left of every other row's, so the column the handle occupies is
// held open and empty instead.
export function TaskRow({
  t,
  config,
  onOpenTask,
  selected,
  onToggleSelect,
  draggable,
  dragging,
  dragPlaceholder,
  nested,
}) {
  const phase = completionPhase(t);
  // What the run itself says it is doing, for as long as it is running
  // (state.js's runActivity). It sits between the title and the chips
  // rather than in the chip row: it is a sentence somebody wrote, not a
  // label off a fixed vocabulary, and it is the one thing on this row
  // that changes while you watch it.
  const activity = runActivity(t);
  return (
    <div
      className={`task-row${dragging ? " task-row-dragging" : ""}`}
      onClick={() => onOpenTask(t.id)}
    >
      {draggable ? (
        <DragIndicatorIcon
          className="task-drag-handle"
          fontSize="small"
          titleAccess="Drag to reorder"
          onClick={(e) => e.stopPropagation()}
        />
      ) : (
        dragPlaceholder && (
          <span className="task-drag-placeholder" aria-hidden="true" />
        )
      )}
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
        title={stateLabel(t)}
      >
        <StateDot
          state={t.state}
          title={stateLabel(t)}
          repairing={t.repairing}
        />
      </span>
      <span className="task-number">{t.id}</span>
      <span className="task-title">{t.title}</span>
      {activity && (
        <span
          className="task-activity"
          title={`${activity.bySetup ? "What grain is doing to get this run started" : "What this run says it is doing"}${activity.age ? `, as of ${activity.age === "now" ? "just now" : `${activity.age} ago`}` : ""}`}
        >
          {/* Whose sentence this is, said out loud rather than left to
              the phrasing: every status on this row until now was an
              agent's own, and grain narrating a run's setup is a
              different voice (state.js's runActivity). */}
          {activity.bySetup && <span className="task-activity-by">grain</span>}
          <span className="task-activity-note">{activity.note}</span>
          {activity.age && (
            <span className="task-activity-age">{activity.age}</span>
          )}
        </span>
      )}
      <span className="chips">
        {t.scheduled && (
          <Chip
            size="small"
            className="chip-scheduled"
            title="filed automatically by a schedule"
            label="scheduled"
          />
        )}
        {t.suiteRun && (
          <Chip
            size="small"
            className="chip-suite"
            title="filed automatically by a suite run"
            label="suite"
          />
        )}
        {t.stacked && !nested && (
          <Chip size="small" className="chip-stacked" {...stackedChip(t)} />
        )}
        {t.interactive && (
          <Chip
            size="small"
            className="chip-interactive"
            title="a live chat, not a background task"
            label="interactive"
          />
        )}
        {t.repo && <Chip size="small" label={t.repo} />}
        {(t.reads || []).map((repo) => (
          <Chip
            key={repo}
            size="small"
            variant="outlined"
            title="read-only"
            label={`${repo} (read)`}
          />
        ))}
        {t.capabilities.map((id) => (
          <Chip key={id} size="small" label={capabilityName(config, id)} />
        ))}
      </span>
      {phase && (
        <Chip
          size="small"
          color={phase.color}
          title={phase.title}
          label={phase.label}
        />
      )}
      {t.blocked && (
        <Chip
          size="small"
          color="error"
          title={`Waiting on ${t.blockedBy.join(", ")}`}
          label="Blocked"
        />
      )}
    </div>
  );
}
