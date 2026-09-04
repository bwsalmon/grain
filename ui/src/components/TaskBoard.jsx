import { useState } from "react";
import { Button, Checkbox, Chip, Typography } from "@mui/material";
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
  boardStates,
  groupIntoColumns,
  hiddenStates,
  loadColumns,
  saveColumns,
} from "../board.js";
import {
  ListEmpty,
  ListFilterSelect,
  ListHeader,
  ListSearchField,
  ListSortSelect,
  ListToolbar,
} from "./ListPrimitives.jsx";
import BoardColumnsOverlay from "./BoardColumnsOverlay.jsx";
import StateDot, { isLiveRunning } from "./StateDot.jsx";

// TaskBoard is the same backlog as TaskList, laid out as a Kanban board:
// one column per group of states, tasks as cards inside them
// (grain/task-287).
//
// It is a second *view*, not a second source of truth. The tasks, the
// search box, the sort orders and every attribute filter are the list's
// own (taskFilters.js), so narrowing to one repo means the same thing on
// both; what the board adds is that "which tasks show up" is also
// answered by the columns themselves, which are the operator's to
// arrange (board.js, BoardColumnsOverlay.jsx).
//
// # Why a card cannot be dragged into another column
//
// Because a task's state is not a field anybody sets. The daemon
// derives it from what has actually happened to the task -- whether a
// run is live, whether an attempt failed, whether a pull request has
// landed (model.StateOf, docs/data-model.md) -- so there is no write
// behind "drop this card in Running" for the UI to make. The gestures
// that really do move a task between these columns are the ones on the
// task itself (approve it, retry it, submit it, close it), which is why
// a card opens the task rather than pretending otherwise.
//
// Dragging *within* a column is real, and is the same edit the list's
// own drag makes: Store.Reorder on the backlog, in exactly the
// "between these two neighbours" terms TaskList uses, so a drag in a
// filtered column still lands correctly among the tasks it cannot see.
// Like the list, it is only offered under "Backlog order", since
// dragging a row while looking at "Newest first" would reorder
// something the screen is not showing.
export default function TaskBoard({
  tasks,
  config,
  onOpenTask,
  selected,
  onToggleSelect,
  onSelectAll,
  onReorder,
}) {
  // The columns this browser last saved, or the default board
  // (board.js). Held here rather than in App: nothing outside this view
  // means anything by them, and the board is mounted for as long as the
  // view is showing.
  const [columns, setColumns] = useState(() => loadColumns());
  const [editing, setEditing] = useState(false);

  // Same three local refinements as TaskList, and local for the same
  // reason: they refine the view currently on screen rather than
  // standing for a question about which tasks matter.
  const [search, setSearch] = useState("");
  const [sortBy, setSortBy] = useState("manual");
  const [filters, setFilters] = useState({});
  const setFilter = (id, value) =>
    setFilters((prev) => ({ ...prev, [id]: value }));

  const q = search.trim().toLowerCase();

  const saveColumnLayout = (next) => {
    setColumns(next);
    saveColumns(next);
  };

  // onBoard is what the board is about before the toolbar has its say:
  // the states the columns cover. It is the board's counterpart to the
  // list's state filter, and it is what the filter menus are built from,
  // so a board with no "Closed" column never offers a repo only closed
  // tasks carry.
  const covered = new Set(boardStates(columns));
  const onBoardTasks = tasks.filter((t) => covered.has(t.state));

  const views = filterViews(onBoardTasks, filters, config);
  const activeFilters = views.filter((v) => v.value !== "");
  const narrowed = q !== "" || activeFilters.length > 0;
  const matches = (t) => matchesFilters(t, activeFilters, q);

  const { cards, hidden } = groupIntoColumns(
    sortTasks(tasks, sortBy),
    columns,
    matches,
  );
  const shown = columns.reduce((n, c) => n + cards.get(c.id).length, 0);
  const offBoard = hiddenStates(columns);

  const reorderEnabled = !!onReorder && sortBy === "manual";

  // dragIds is what is being dragged, dragColumn the column it was
  // picked up in -- a drop is only accepted back into that same column,
  // for the reason in this file's own header. overId is the highlight
  // and never drives the move.
  const [dragIds, setDragIds] = useState(null);
  const [dragColumn, setDragColumn] = useState(null);
  const [overId, setOverId] = useState(null);

  const startDrag = (columnId, t) => {
    const ids =
      selected.has(t.id) && selected.size > 1 ? selected : new Set([t.id]);
    setDragIds(
      cards
        .get(columnId)
        .filter((x) => ids.has(x.id))
        .map((x) => x.id),
    );
    setDragColumn(columnId);
  };

  const endDrag = () => {
    setDragIds(null);
    setDragColumn(null);
    setOverId(null);
  };

  // dropOn resolves a drop onto targetId -- or onto the end of the
  // column, when targetId is null -- to the two neighbours it landed
  // between among that column's own visible cards, and hands them to
  // onReorder. The store places the dragged tasks between whatever those
  // names resolve to in the full backlog, so a card dropped at the top
  // of a column goes above the card that was there rather than to the
  // top of the whole backlog.
  const dropOn = (columnId, targetId) => {
    if (!dragIds || columnId !== dragColumn) return endDrag();
    const dragging = new Set(dragIds);
    const visible = cards
      .get(columnId)
      .map((t) => t.id)
      .filter((id) => !dragging.has(id));
    const idx = targetId === null ? visible.length : visible.indexOf(targetId);
    onReorder(
      dragIds,
      idx > 0 ? visible[idx - 1] : null,
      idx < visible.length ? visible[idx] : null,
    );
    endDrag();
  };

  const clearNarrowing = () => {
    setSearch("");
    setFilters({});
  };

  // A column's own "select all": the batch actions bar under the board
  // is the list's, and "run everything queued" is the reason somebody
  // opens a board in the first place.
  const toggleColumn = (columnId, checked) => {
    onSelectAll(
      cards.get(columnId).map((t) => t.id),
      checked,
    );
  };

  return (
    <main className="board">
      <ListHeader
        title="Board"
        count={shown}
        action={
          <Button
            size="small"
            sx={{ ml: "auto" }}
            onClick={() => setEditing(true)}
          >
            Columns
          </Button>
        }
      />
      {tasks.length > 0 && (
        <ListToolbar>
          <ListSearchField
            placeholder="Search tasks…"
            value={search}
            onChange={setSearch}
          />
          <ListSortSelect
            id="board-sort"
            value={sortBy}
            onChange={setSortBy}
            options={SORTS}
          />
          {views
            .filter((v) => v.shown)
            .map(({ f, options, value }) => (
              <ListFilterSelect
                key={f.id}
                id={`board-filter-${f.id}`}
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
        </ListToolbar>
      )}

      {/* What the board is deliberately not showing. Said once, under
          the toolbar, with the number of tasks it comes to -- so a board
          whose "Closed" column somebody removed doesn't quietly look
          like a deployment with fewer tasks in it than it has. */}
      {hidden > 0 && (
        <Typography
          variant="caption"
          color="text.secondary"
          component="p"
          className="board-hidden-note"
        >
          {hidden} task{hidden === 1 ? "" : "s"} in no column (
          {offBoard.map((s) => STATE_LABELS[s] || s).join(", ")}).{" "}
          <Button size="small" onClick={() => setEditing(true)}>
            Edit columns
          </Button>
        </Typography>
      )}

      <div className="board-columns">
        {columns.map((c) => {
          const columnCards = cards.get(c.id);
          const allSelected =
            columnCards.length > 0 &&
            columnCards.every((t) => selected.has(t.id));
          return (
            <section key={c.id} className="board-column">
              <header className="board-column-header">
                {onSelectAll && columnCards.length > 0 && (
                  <Checkbox
                    size="small"
                    checked={allSelected}
                    onChange={(e) => toggleColumn(c.id, e.target.checked)}
                    inputProps={{
                      "aria-label": `Select every task in ${c.title}`,
                    }}
                  />
                )}
                <span className="board-column-title">{c.title}</span>
                <span className="count">{columnCards.length}</span>
              </header>
              <ul className="board-column-cards">
                {columnCards.map((t) => (
                  <li
                    key={t.id}
                    className={
                      overId === t.id &&
                      dragIds &&
                      dragColumn === c.id &&
                      !dragIds.includes(t.id)
                        ? "board-drop-target"
                        : undefined
                    }
                    draggable={reorderEnabled}
                    onDragStart={() => reorderEnabled && startDrag(c.id, t)}
                    onDragEnd={endDrag}
                    onDragOver={(e) => {
                      if (
                        !dragIds ||
                        dragColumn !== c.id ||
                        dragIds.includes(t.id)
                      )
                        return;
                      e.preventDefault();
                      setOverId(t.id);
                    }}
                    onDrop={(e) => {
                      e.preventDefault();
                      dropOn(c.id, t.id);
                    }}
                  >
                    <BoardCard
                      t={t}
                      config={config}
                      onOpenTask={onOpenTask}
                      selected={selected}
                      onToggleSelect={onToggleSelect}
                      draggable={reorderEnabled}
                      dragging={dragIds?.includes(t.id) ?? false}
                      // The state dot earns its space only where a column
                      // holds more than one state -- in a "Running"
                      // column every card would carry the same dot.
                      showState={c.states.length > 1}
                    />
                  </li>
                ))}
                {reorderEnabled && dragIds && dragColumn === c.id && (
                  <li
                    className={`board-drop-end${overId === `__end__${c.id}` ? " board-drop-target" : ""}`}
                    onDragOver={(e) => {
                      e.preventDefault();
                      setOverId(`__end__${c.id}`);
                    }}
                    onDrop={(e) => {
                      e.preventDefault();
                      dropOn(c.id, null);
                    }}
                  />
                )}
              </ul>
              {columnCards.length === 0 && (
                <p className="board-column-empty">
                  {c.states.length === 0
                    ? "No states in this column."
                    : "Nothing here."}
                </p>
              )}
            </section>
          );
        })}
      </div>

      {columns.length === 0 && (
        <ListEmpty>
          This board has no columns. Add one with “Columns”.
        </ListEmpty>
      )}
      {columns.length > 0 && shown === 0 && (
        <ListEmpty>
          {narrowed
            ? "No tasks match this search and these filters."
            : "No tasks in any of these columns."}
        </ListEmpty>
      )}

      {editing && (
        <BoardColumnsOverlay
          columns={columns}
          onSave={saveColumnLayout}
          onClose={() => setEditing(false)}
        />
      )}
    </main>
  );
}

// BoardCard is a task as it appears on the board: the same facts
// TaskRow puts in a row, stacked into a card that survives a column a
// few hundred pixels wide. It is not TaskRow itself -- that row is one
// line of fixed columns (handle, number, title, chips) whose whole
// shape assumes the width of a page -- but it shows the same things and
// takes the same click, so a task reads the same either way.
export function BoardCard({
  t,
  config,
  onOpenTask,
  selected,
  onToggleSelect,
  draggable,
  dragging,
  showState = true,
}) {
  const phase = completionPhase(t);
  const activity = runActivity(t);
  // stateLabel rather than STATE_LABELS, and the repairing flag passed
  // through to the mark, so a card says "Repairing" in the queue's own
  // green wherever the same task's row in TaskList would.
  const label = stateLabel(t);
  return (
    <div
      className={`board-card${dragging ? " board-card-dragging" : ""}`}
      onClick={() => onOpenTask(t.id)}
    >
      <div className="board-card-top">
        {draggable && (
          <DragIndicatorIcon
            className="task-drag-handle"
            fontSize="small"
            titleAccess="Drag to reorder"
            onClick={(e) => e.stopPropagation()}
          />
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
        {showState && (
          <span
            className={`badge badge-icon badge-${t.state}${isLiveRunning(t.state) ? " badge-mark" : ""}`}
            title={label}
          >
            <StateDot state={t.state} title={label} repairing={t.repairing} />
          </span>
        )}
        <span className="task-number">{t.id}</span>
        {t.blocked && (
          <Chip
            size="small"
            color="error"
            title={`Waiting on ${t.blockedBy.join(", ")}`}
            label="Blocked"
          />
        )}
        {phase && (
          <Chip
            size="small"
            color={phase.color}
            title={phase.title}
            label={phase.label}
          />
        )}
      </div>
      <div className="board-card-title">{t.title}</div>
      {activity && (
        <div
          className="task-activity board-card-activity"
          title={`What this run says it is doing${activity.age ? `, as of ${activity.age === "now" ? "just now" : `${activity.age} ago`}` : ""}`}
        >
          <span className="task-activity-note">{activity.note}</span>
          {activity.age && (
            <span className="task-activity-age">{activity.age}</span>
          )}
        </div>
      )}
      <div className="board-card-chips">
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
        {t.stacked && (
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
        {(t.capabilities || []).map((id) => (
          <Chip key={id} size="small" label={capabilityName(config, id)} />
        ))}
      </div>
    </div>
  );
}
