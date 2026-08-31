import { useState } from "react";
import { Button, Chip, FormControl, InputLabel, MenuItem, Select, TextField, Typography } from "@mui/material";
import TemplateOverlay from "./TemplateOverlay.jsx";

// SORTS mirrors TaskList's own toolbar Select (bwsalmon/agents#545): a
// template has no state or backlog order to sort by (it is never itself
// something that runs), so "manual" drops out and Name (A-Z) takes its
// place as the default -- the one property every template is guaranteed
// to have that reads meaningfully in a fixed order.
const SORTS = {
  name: { label: "Name (A–Z)", cmp: (a, b) => a.name.localeCompare(b.name) },
  newest: { label: "Newest first", cmp: (a, b) => new Date(b.createdAt || 0) - new Date(a.createdAt || 0) },
  oldest: { label: "Oldest first", cmp: (a, b) => new Date(a.createdAt || 0) - new Date(b.createdAt || 0) },
};

// TemplatesList is the templates page's main pane (bwsalmon/agents#545):
// a flat list of every template's key details -- name, target repo,
// task title -- with its own search/sort toolbar, TaskList's own shape.
// Nothing about any one template (description, base branch, reads,
// capabilities, editing, deleting) lives here any more; all of that
// moved into TemplateOverlay, opened either by the "+ New template"
// button or by clicking a row, so this list stays a list instead of
// also being a form.
export default function TemplatesList({ templates, config, onRefresh, showError }) {
  const [search, setSearch] = useState("");
  const [sortBy, setSortBy] = useState("name");
  const [showNew, setShowNew] = useState(false);
  const [editing, setEditing] = useState(null);

  const q = search.trim().toLowerCase();
  const matches = (t) =>
    q === "" || t.name.toLowerCase().includes(q) || t.title.toLowerCase().includes(q) || t.repo.toLowerCase().includes(q);

  const visible = templates.filter(matches).sort(SORTS[sortBy].cmp);

  return (
    <main>
      <div className="content-header">
        <Typography variant="h6" component="h2" sx={{ m: 0, fontSize: "1rem", fontWeight: 600 }}>Task templates</Typography>
        <span className="count">{visible.length}</span>
        <Button variant="contained" size="small" sx={{ ml: "auto" }} onClick={() => setShowNew(true)}>+ New template</Button>
      </div>
      {templates.length > 0 && (
        <div className="task-list-toolbar">
          <TextField
            size="small"
            placeholder="Search templates…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            sx={{ flex: 1, maxWidth: 320 }}
          />
          <FormControl size="small" sx={{ minWidth: 170 }}>
            <InputLabel id="template-sort-label">Sort</InputLabel>
            <Select
              labelId="template-sort-label"
              label="Sort"
              value={sortBy}
              onChange={(e) => setSortBy(e.target.value)}
            >
              {Object.entries(SORTS).map(([id, { label }]) => <MenuItem key={id} value={id}>{label}</MenuItem>)}
            </Select>
          </FormControl>
        </div>
      )}
      <ul className="template-list">
        {visible.map((tmpl) => (
          <li className="template-row" key={tmpl.id} onClick={() => setEditing(tmpl)}>
            <span className="template-name">{tmpl.name}</span>
            <Chip size="small" label={tmpl.repo} />
            <span className="template-title hint">{tmpl.title}</span>
          </li>
        ))}
      </ul>
      {templates.length === 0 && <p className="empty">No task templates.</p>}
      {templates.length > 0 && visible.length === 0 && <p className="empty">No templates match your search.</p>}

      {showNew && (
        <TemplateOverlay config={config} onClose={() => setShowNew(false)} onSaved={onRefresh} showError={showError} />
      )}
      {editing && (
        <TemplateOverlay template={editing} config={config} onClose={() => setEditing(null)} onSaved={onRefresh} showError={showError} />
      )}
    </main>
  );
}
