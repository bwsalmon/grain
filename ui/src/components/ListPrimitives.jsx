import { useState } from "react";
import {
  FormControl,
  InputLabel,
  MenuItem,
  Select,
  TextField,
  Typography,
} from "@mui/material";
import DragIndicatorIcon from "@mui/icons-material/DragIndicator";

// ListHeader/ListToolbar/ListEmpty are the pieces TaskList, RepoList,
// TemplatesList, and SchedulesList already agreed to share by convention
// (bwsalmon/agents#561 made RepoList's rows match the other three's, but
// left each page hand-rolling its own .content-header/.task-list-toolbar
// markup). Pulling them out here means a fifth list page -- or a future
// edit to one of these four -- gets the shared shape by construction
// instead of by copying the right CSS class names onto the right divs.
//
// The fifth page (SuitesList) took the toolbar too in grain/task-327,
// which is also where ReorderableList at the bottom of this file comes
// from: the four non-task pages had no order of their own to offer, so
// their rows carried no drag handle and no sort menu and read as blank
// beside the task rows next door.
//
// Deliberately not included: the row itself. TaskRow stays exported from
// TaskList.jsx for any view that wants a task row identical to that
// list's own; the four pages' *own* rows carry different columns (a
// repo's per-state counts, a template's single title line, a schedule's
// second "next run" line) that don't reduce to one shape without either
// losing information or growing a prop for every page's special case.

// `icon` is the page's own item glyph (ItemGlyph.jsx), for the four
// pages that have one -- the same figure the nav entry that got here
// carries, at the size a heading can hold, so the icon in the rail and
// the page it opens are visibly the same thing. Ahead of the title and
// outside the heading itself, so it is decoration and the heading's
// accessible name stays the one word.
export function ListHeader({ title, count, action, icon, style }) {
  return (
    <div className="content-header" style={style}>
      {icon ? <span className="header-icon">{icon}</span> : null}
      <Typography
        variant="h6"
        component="h2"
        sx={{ m: 0, fontSize: "1rem", fontWeight: 600 }}
      >
        {title}
      </Typography>
      {count != null && <span className="count">{count}</span>}
      {action}
    </div>
  );
}

export function ListToolbar({ children }) {
  return <div className="task-list-toolbar">{children}</div>;
}

export function ListSearchField({ value, onChange, placeholder }) {
  return (
    <TextField
      size="small"
      placeholder={placeholder}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      sx={{ flex: 1, maxWidth: 320 }}
    />
  );
}

// ListSortSelect's options take the same { [id]: { label } } shape as
// each page's own local SORTS map, so callers keep the cmp functions
// (which this component has no need of) next to the labels instead of
// having to split one map into two.
export function ListSortSelect({ id, value, onChange, options }) {
  const labelId = `${id}-label`;
  return (
    <FormControl size="small" sx={{ minWidth: 170 }}>
      <InputLabel id={labelId}>Sort</InputLabel>
      <Select
        labelId={labelId}
        label="Sort"
        value={value}
        onChange={(e) => onChange(e.target.value)}
      >
        {Object.entries(options).map(([key, { label }]) => (
          <MenuItem key={key} value={key}>
            {label}
          </MenuItem>
        ))}
      </Select>
    </FormControl>
  );
}

// ListFilterSelect is ListSortSelect's counterpart for narrowing a list
// rather than ordering it: one attribute, its own label, and a first
// entry (value "") that leaves the attribute out of the question
// altogether.
//
// Options are an array of { value, label } rather than ListSortSelect's
// map, because a filter's values are read off whatever the list happens
// to hold -- a repo name, a capability id, an author -- so the caller,
// not an object literal's key order, decides what order they come in.
export function ListFilterSelect({
  id,
  label,
  anyLabel,
  value,
  onChange,
  options,
}) {
  const labelId = `${id}-label`;
  return (
    <FormControl size="small" sx={{ minWidth: 150 }}>
      <InputLabel id={labelId}>{label}</InputLabel>
      <Select
        labelId={labelId}
        label={label}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      >
        <MenuItem value="">{anyLabel}</MenuItem>
        {options.map((o) => (
          <MenuItem key={o.value} value={o.value}>
            {o.label}
          </MenuItem>
        ))}
      </Select>
    </FormControl>
  );
}

export function ListEmpty({ children }) {
  return <p className="empty">{children}</p>;
}

// ReorderableList is TaskList's own drag-to-reorder gesture -- a handle
// on every row, a rule drawn where the drop would land, a zone under the
// last row for "put it at the end" -- for the four list pages whose
// order is a display order rather than a backlog (listOrder.js,
// grain/task-327). It owns the <ul>, the <li>s and the drag state; the
// caller renders whatever its own row is, which is the part no two of
// these pages agree on.
//
// `reorder` is null on a list that is not currently in its custom order:
// no handles, nothing draggable, no drop zone. Same rule TaskList
// applies to the backlog (taskFilters.js's SORTS) -- while a list is
// sorted by name or by date, a drop has nowhere meaningful to land, so
// the gesture is withdrawn rather than left to silently rewrite an order
// the view no longer shows.
//
// The row is a render prop rather than a component prop so the handle
// can go *inside* it: the handle belongs in the row's own flex line,
// ahead of the name, and only the caller knows where that is.
export function ReorderableList({ className, items, idOf, reorder, children }) {
  // dragId is the row being dragged right now, or null. overId is
  // purely the drop-target highlight -- "__end__" for the trailing zone
  // -- and never drives the actual move, which only happens in onDrop.
  const [dragId, setDragId] = useState(null);
  const [overId, setOverId] = useState(null);
  const enabled = !!reorder;

  const stop = () => {
    setDragId(null);
    setOverId(null);
  };
  const drop = (beforeId) => {
    if (dragId !== null && dragId !== beforeId) reorder(dragId, beforeId);
    stop();
  };

  // One handle element for every row: it carries no per-row state, and
  // the click it swallows is the row's own onClick (which on all four of
  // these pages opens the thing the row names) rather than the drag.
  const handle = enabled ? (
    <DragIndicatorIcon
      className="task-drag-handle"
      fontSize="small"
      titleAccess="Drag to reorder"
      onClick={(e) => e.stopPropagation()}
    />
  ) : null;

  return (
    <ul className={className}>
      {items.map((item) => {
        const id = idOf(item);
        const over = dragId !== null && dragId !== id && overId === id;
        return (
          <li
            key={id}
            className={over ? "task-drop-target" : undefined}
            draggable={enabled}
            onDragStart={enabled ? () => setDragId(id) : undefined}
            onDragEnd={enabled ? stop : undefined}
            onDragOver={
              enabled
                ? (e) => {
                    if (dragId === null || dragId === id) return;
                    e.preventDefault();
                    setOverId(id);
                  }
                : undefined
            }
            onDrop={
              enabled
                ? (e) => {
                    e.preventDefault();
                    drop(id);
                  }
                : undefined
            }
          >
            {children(item, { handle, dragging: dragId === id })}
          </li>
        );
      })}
      {enabled && dragId !== null && (
        <li
          className={`task-drop-end${overId === "__end__" ? " task-drop-target" : ""}`}
          onDragOver={(e) => {
            e.preventDefault();
            setOverId("__end__");
          }}
          onDrop={(e) => {
            e.preventDefault();
            drop(null);
          }}
        />
      )}
    </ul>
  );
}
