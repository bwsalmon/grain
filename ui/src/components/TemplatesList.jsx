import { useState } from "react";
import { Button, Chip } from "@mui/material";
import { knownRepos } from "../state.js";
import TemplateOverlay from "./TemplateOverlay.jsx";
import {
  ListEmpty,
  ListHeader,
  ListSearchField,
  ListSortSelect,
  ListToolbar,
} from "./ListPrimitives.jsx";
import ItemGlyph from "./ItemGlyph.jsx";

// SORTS mirrors TaskList's own toolbar Select (bwsalmon/agents#545): a
// template has no state or backlog order to sort by (it is never itself
// something that runs), so "manual" drops out and Name (A-Z) takes its
// place as the default -- the one property every template is guaranteed
// to have that reads meaningfully in a fixed order.
const SORTS = {
  name: { label: "Name (A–Z)", cmp: (a, b) => a.name.localeCompare(b.name) },
  newest: {
    label: "Newest first",
    cmp: (a, b) => new Date(b.createdAt || 0) - new Date(a.createdAt || 0),
  },
  oldest: {
    label: "Oldest first",
    cmp: (a, b) => new Date(a.createdAt || 0) - new Date(b.createdAt || 0),
  },
};

// TemplatesList is the templates page's main pane (bwsalmon/agents#545):
// a flat list of every template's key details -- name, task title, and
// the repo it is bound to for the templates that are bound to one at
// all (grain/task-285; most are not, and show no chip) -- with its own
// search/sort toolbar, TaskList's own shape. Nothing about any one
// template (description, reads, capabilities, editing, deleting) lives
// here any more; all of that moved into TemplateOverlay, opened either
// by the "+ New template" button or by clicking a row, so this list
// stays a list instead of also being a form.
//
// Which template is open is App.jsx's state (openTemplateId), not this
// component's, so the URL can name it -- SchedulesList's own doc
// comment on why (grain/task-139).
export default function TemplatesList({
  templates,
  config,
  tasks,
  openTemplateId,
  onOpenTemplate,
  onRefresh,
  showError,
}) {
  const [search, setSearch] = useState("");
  const [sortBy, setSortBy] = useState("name");
  const [showNew, setShowNew] = useState(false);
  // Behind both of the overlay's repo pickers: the read-only repos it
  // reads, and the one repo it can optionally be bound to. Same list the
  // repo dropdowns elsewhere offer (SchedulesList, SuitesList).
  const repoOptions = knownRepos(config, tasks);
  const editing = templates.find((t) => t.id === openTemplateId) || null;

  const q = search.trim().toLowerCase();
  const matches = (t) =>
    q === "" ||
    t.name.toLowerCase().includes(q) ||
    t.title.toLowerCase().includes(q) ||
    (t.repo || "").toLowerCase().includes(q);

  const visible = templates.filter(matches).sort(SORTS[sortBy].cmp);

  return (
    <main>
      <ListHeader
        title="Templates"
        icon={<ItemGlyph kind="templates" size={20} />}
        count={visible.length}
        action={
          <Button
            variant="contained"
            size="small"
            sx={{ ml: "auto" }}
            onClick={() => setShowNew(true)}
          >
            + New template
          </Button>
        }
      />
      {templates.length > 0 && (
        <ListToolbar>
          <ListSearchField
            placeholder="Search templates…"
            value={search}
            onChange={setSearch}
          />
          <ListSortSelect
            id="template-sort"
            value={sortBy}
            onChange={setSortBy}
            options={SORTS}
          />
        </ListToolbar>
      )}
      <ul className="template-list">
        {visible.map((tmpl) => (
          <li
            className="template-row"
            key={tmpl.id}
            onClick={() => onOpenTemplate(tmpl.id)}
          >
            <span className="template-name">{tmpl.name}</span>
            {tmpl.repo && (
              <Chip
                size="small"
                label={tmpl.base ? `${tmpl.repo} @ ${tmpl.base}` : tmpl.repo}
              />
            )}
            <span className="template-title hint">{tmpl.title}</span>
          </li>
        ))}
      </ul>
      {templates.length === 0 && <ListEmpty>No templates.</ListEmpty>}
      {templates.length > 0 && visible.length === 0 && (
        <ListEmpty>No templates match your search.</ListEmpty>
      )}

      {showNew && (
        <TemplateOverlay
          repoOptions={repoOptions}
          config={config}
          onClose={() => setShowNew(false)}
          onSaved={onRefresh}
          showError={showError}
        />
      )}
      {editing && (
        <TemplateOverlay
          template={editing}
          repoOptions={repoOptions}
          config={config}
          onClose={() => onOpenTemplate(null)}
          onSaved={onRefresh}
          showError={showError}
        />
      )}
    </main>
  );
}
