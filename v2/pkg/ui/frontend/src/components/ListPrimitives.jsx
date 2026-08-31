import { FormControl, InputLabel, MenuItem, Select, TextField, Typography } from "@mui/material";

// ListHeader/ListToolbar/ListEmpty are the pieces TaskList, RepoList,
// TemplatesList, and SchedulesList already agreed to share by convention
// (bwsalmon/agents#561 made RepoList's rows match the other three's, but
// left each page hand-rolling its own .content-header/.task-list-toolbar
// markup). Pulling them out here means a fifth list page -- or a future
// edit to one of these four -- gets the shared shape by construction
// instead of by copying the right CSS class names onto the right divs.
//
// Deliberately not included: the row itself. TaskRow (still exported
// from TaskList.jsx) already covers the one case where two of these
// pages render an identical row (RepoList's per-repo task sublist); the
// four pages' *own* rows carry different columns (a repo's chevron and
// action buttons, a template's single title line, a schedule's second
// "next run" line) that don't reduce to one shape without either losing
// information or growing a prop for every page's special case.
export function ListHeader({ title, count, action, style }) {
  return (
    <div className="content-header" style={style}>
      <Typography variant="h6" component="h2" sx={{ m: 0, fontSize: "1rem", fontWeight: 600 }}>{title}</Typography>
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
        {Object.entries(options).map(([key, { label }]) => <MenuItem key={key} value={key}>{label}</MenuItem>)}
      </Select>
    </FormControl>
  );
}

export function ListEmpty({ children }) {
  return <p className="empty">{children}</p>;
}
