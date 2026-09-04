import { useState } from "react";
import { Box, Button, FormControl, IconButton, InputLabel, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import ArrowDownwardIcon from "@mui/icons-material/ArrowDownward";
import ArrowUpwardIcon from "@mui/icons-material/ArrowUpward";
import DeleteOutlineIcon from "@mui/icons-material/DeleteOutline";
import { STATE_LABELS, STATE_ORDER } from "../state.js";
import { defaultColumns, hiddenStates, newColumn, normalizeColumns } from "../board.js";
import Overlay from "./Overlay.jsx";

// BoardColumnsOverlay edits the board's columns: what they are called,
// which states each one collects, what order they run in left to right
// (board.js).
//
// A centered dialog rather than a pane, by Overlay.jsx's own rule: this
// is one form you fill in and dismiss, taken over the board you are
// already looking at, not a destination with a URL of its own.
//
// The whole edit is held here and handed back once, on Save -- so
// Cancel, or Escape, or a click on the backdrop, leaves the board
// exactly as it was however far through a rearrangement somebody got.
// Nothing here writes to localStorage either; TaskBoard does that with
// what Save hands it, which keeps "what is on screen" and "what is
// stored" the same decision in one place.
export default function BoardColumnsOverlay({ columns, onSave, onClose }) {
  const [draft, setDraft] = useState(() => columns.map((c) => ({ ...c, states: [...c.states] })));

  const update = (id, change) => setDraft((prev) => prev.map((c) => (c.id === id ? { ...c, ...change } : c)));

  // A state belongs to exactly one column: assigning it here takes it
  // out of whichever column had it. The alternative -- letting two
  // columns claim "Running" and quietly showing the task in the leftmost
  // -- would render a board that disagrees with the editor that made it.
  const setStates = (id, states) => setDraft((prev) => prev.map((c) => {
    if (c.id === id) return { ...c, states };
    return { ...c, states: c.states.filter((s) => !states.includes(s)) };
  }));

  const move = (index, delta) => setDraft((prev) => {
    const next = [...prev];
    const to = index + delta;
    if (to < 0 || to >= next.length) return prev;
    [next[index], next[to]] = [next[to], next[index]];
    return next;
  });

  const remove = (id) => setDraft((prev) => prev.filter((c) => c.id !== id));

  const add = () => setDraft((prev) => [...prev, newColumn()]);

  const save = () => {
    // normalizeColumns is the same pass a stored layout goes through on
    // the way in, so an edit can't produce a board a reload wouldn't:
    // an emptied title falls back to the states' own labels, and a
    // draft somebody deleted every column of falls back to the default
    // rather than saving a board with nothing on it.
    onSave(normalizeColumns(draft) || defaultColumns());
    onClose();
  };

  // What the board will not be showing once this is saved, said while
  // there is still a column to put it in. Closed tasks are the ordinary
  // case (the default board has no column for them) so this is
  // information rather than a warning.
  const offBoard = hiddenStates(draft);

  return (
    <Overlay onClose={onClose} wide>
      <Typography variant="h6" component="h2" sx={{ mb: 0.5 }}>Board columns</Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        Each column collects the states you give it, left to right. A state can only be in one
        column, and a state in no column is left off the board altogether — which is how closed
        tasks stay out of the way by default. Kept in this browser, like the theme.
      </Typography>

      <Stack spacing={1.2}>
        {draft.map((c, i) => (
          <Box key={c.id} sx={{ display: "flex", alignItems: "center", gap: 1 }}>
            <TextField
              size="small"
              label="Title"
              value={c.title}
              onChange={(e) => update(c.id, { title: e.target.value })}
              sx={{ width: 200 }}
              inputProps={{ "aria-label": `Column ${i + 1} title` }}
            />
            <FormControl size="small" sx={{ flex: 1, minWidth: 220 }}>
              <InputLabel id={`board-col-${c.id}-label`}>States</InputLabel>
              <Select
                multiple
                labelId={`board-col-${c.id}-label`}
                label="States"
                value={c.states}
                onChange={(e) => setStates(c.id, typeof e.target.value === "string" ? e.target.value.split(",") : e.target.value)}
                renderValue={(picked) => (picked.length === 0 ? "No states" : picked.map((s) => STATE_LABELS[s] || s).join(", "))}
                displayEmpty
              >
                {STATE_ORDER.map((s) => (
                  <MenuItem key={s} value={s}>{STATE_LABELS[s] || s}</MenuItem>
                ))}
              </Select>
            </FormControl>
            <IconButton size="small" aria-label={`Move ${c.title} left`} disabled={i === 0} onClick={() => move(i, -1)}>
              <ArrowUpwardIcon fontSize="small" sx={{ transform: "rotate(-90deg)" }} />
            </IconButton>
            <IconButton size="small" aria-label={`Move ${c.title} right`} disabled={i === draft.length - 1} onClick={() => move(i, 1)}>
              <ArrowDownwardIcon fontSize="small" sx={{ transform: "rotate(-90deg)" }} />
            </IconButton>
            <IconButton size="small" aria-label={`Remove ${c.title}`} onClick={() => remove(c.id)}>
              <DeleteOutlineIcon fontSize="small" />
            </IconButton>
          </Box>
        ))}
      </Stack>

      {draft.length === 0 && (
        <Typography variant="body2" color="text.secondary" sx={{ mt: 1 }}>
          No columns. Saving now puts the default board back.
        </Typography>
      )}

      {offBoard.length > 0 && (
        <Typography variant="caption" color="text.secondary" component="p" sx={{ mt: 1.5 }}>
          Off the board: {offBoard.map((s) => STATE_LABELS[s] || s).join(", ")}.
        </Typography>
      )}

      <Box sx={{ display: "flex", gap: 1, mt: 2.5 }}>
        <Button size="small" onClick={add}>+ Add column</Button>
        <Button size="small" onClick={() => setDraft(defaultColumns())}>Reset to default</Button>
        <Box sx={{ flex: 1 }} />
        <Button size="small" onClick={onClose}>Cancel</Button>
        <Button size="small" variant="contained" onClick={save}>Save</Button>
      </Box>
    </Overlay>
  );
}
