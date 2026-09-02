import { useEffect, useRef, useState } from "react";
import { ClickAwayListener, ListItemText, MenuItem, MenuList, Paper, Popper, TextField, Typography } from "@mui/material";

// TaskPicker is a search box that resolves free text to a task: type to
// filter by id or title, click (or arrow down + Enter) to pick one. It
// replaces the raw "type a task id and hope you copied it right" inputs
// that used to sit wherever a task needed to reference another one.
//
// It holds no selection state of its own -- onPick fires once per pick
// and the field clears, so a caller wanting a persistent choice (a chip,
// a form field) keeps that in its own state and renders it alongside.
// That keeps a single-pick "attach immediately" use (DetailOverlay's
// dependency add) and a multi-pick "build a list, submit once" use
// (NewTaskOverlay's dependsOn) working off the same component.
export default function TaskPicker({ tasks, exclude = [], onPick, placeholder = "Search tasks…", autoFocus = false }) {
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [highlight, setHighlight] = useState(0);
  const anchorRef = useRef(null);

  const excludeSet = new Set(exclude);
  const q = query.trim().toLowerCase();
  const matches = q === "" ? [] : tasks
    .filter((t) => !excludeSet.has(t.id))
    .filter((t) => t.id.toLowerCase().includes(q) || t.title.toLowerCase().includes(q))
    .slice(0, 8);

  useEffect(() => { setHighlight(0); }, [query]);

  const pick = (t) => {
    onPick(t);
    setQuery("");
    setOpen(false);
  };

  const onKeyDown = (e) => {
    if (e.key === "Escape") { setOpen(false); return; }
    if (matches.length === 0) return;
    if (e.key === "ArrowDown") { e.preventDefault(); setHighlight((h) => Math.min(h + 1, matches.length - 1)); }
    else if (e.key === "ArrowUp") { e.preventDefault(); setHighlight((h) => Math.max(h - 1, 0)); }
    else if (e.key === "Enter") { e.preventDefault(); pick(matches[highlight]); }
  };

  const showResults = open && q !== "";

  return (
    <ClickAwayListener onClickAway={() => setOpen(false)}>
      <div ref={anchorRef} style={{ position: "relative" }}>
        <TextField
          size="small"
          fullWidth
          placeholder={placeholder}
          value={query}
          autoFocus={autoFocus}
          autoComplete="off"
          onChange={(e) => { setQuery(e.target.value); setOpen(true); }}
          onFocus={() => setOpen(true)}
          onKeyDown={onKeyDown}
        />
        <Popper open={showResults} anchorEl={anchorRef.current} placement="bottom-start" style={{ width: anchorRef.current?.offsetWidth, zIndex: 1300 }}>
          <Paper variant="outlined" sx={{ mt: 0.5, maxHeight: 220, overflowY: "auto" }}>
            <MenuList dense>
              {matches.length === 0 && (
                <MenuItem disabled>
                  <Typography variant="body2" color="text.secondary">No matching tasks</Typography>
                </MenuItem>
              )}
              {matches.map((t, i) => (
                <MenuItem
                  key={t.id}
                  selected={i === highlight}
                  onClick={() => pick(t)}
                  onMouseEnter={() => setHighlight(i)}
                >
                  <ListItemText
                    primary={t.title}
                    secondary={t.id}
                    primaryTypographyProps={{ noWrap: true }}
                  />
                </MenuItem>
              ))}
            </MenuList>
          </Paper>
        </Popper>
      </div>
    </ClickAwayListener>
  );
}
