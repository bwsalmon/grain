import { useState } from "react";
import AddIcon from "@mui/icons-material/Add";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import ChevronRightIcon from "@mui/icons-material/ChevronRight";
import { Button, Chip, IconButton, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import { STATE_LABELS, STATE_ORDER, repoRows } from "../state.js";
import { TaskRow } from "./TaskList.jsx";
import { ListEmpty, ListHeader, ListSearchField, ListToolbar } from "./ListPrimitives.jsx";

// RepoList is the repo page: one row per known repo -- every
// config.targetRepos entry, plus any repo tasks target that isn't one --
// each showing how many tasks sit in every state so a repo with
// something stuck (awaiting_reply, or a pile of blocked work) stands out
// before anyone opens it. Clicking a row is the entry point into the
// repo-centric task list -- onOpenRepo scopes App's own task view to it.
//
// The chevron (bwsalmon/agents#474) is a second way into the same
// tasks: it folds them out right here, for a quick look that doesn't
// leave the repo page, without replacing the deeper task-list view the
// row itself still opens. The Releases and "+" buttons are further
// entry points into the same row (hence stopPropagation on both, so
// neither also fires onOpenRepo or the chevron toggle): release
// management is a property of the repo (bwsalmon/agents#459), and
// filing a task against it is the repo page's own shortcut for not
// retyping the repo you're already looking at, so both live here
// rather than behind a sidebar button reachable from anywhere.
//
// Adding and removing a target repo (bwsalmon/agents#473) lives here
// too, replacing the "Target repos" list Settings used to bury this
// behind: a repo is a thing this page is about, not a deployment knob.
// Remove only appears on a row that is actually in config.targetRepos
// (repoRows' own `configured`) -- a row that only exists because a task
// already targets it has nothing to remove, and removing a configured
// repo doesn't make the row disappear either as long as a task still
// targets it, so the two facts (targeted, configured) stay visibly
// independent rather than the button pretending removal always clears
// the row.
//
// The header/toolbar/row shape below mirrors TaskList's own
// (bwsalmon/agents#561): a .content-header with title, count, and this
// page's own primary action (the add-repo form, in place of TaskList's
// filter title or TemplatesList/SchedulesList's "+ New X" button) above
// a .task-list-toolbar search box, then flat divider rows instead of
// this list's old card-per-repo look -- so the four list pages read as
// one design instead of four.
export default function RepoList({ tasks, config, onOpenRepo, onOpenReleases, onOpenTask, onNewTask, onRefreshConfig, showError }) {
  const [newRepo, setNewRepo] = useState("");
  const [search, setSearch] = useState("");
  const repos = repoRows(config, tasks);
  const [expanded, setExpanded] = useState(() => new Set());

  const q = search.trim().toLowerCase();
  const visible = repos.filter((r) => q === "" || r.repo.toLowerCase().includes(q));

  const toggleExpanded = (repo) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(repo)) next.delete(repo); else next.add(repo);
      return next;
    });
  };

  const addRepo = async (evt) => {
    evt.preventDefault();
    const repo = newRepo.trim();
    if (repo === "") return;
    try {
      await api("/api/repos", { method: "POST", body: JSON.stringify({ repo }) });
      setNewRepo("");
      await onRefreshConfig();
    } catch (err) {
      showError(err);
    }
  };

  const removeRepo = async (evt, repo) => {
    evt.stopPropagation();
    if (!confirm(`Remove ${repo} from target repos? Tasks that already target it are not affected, but new tasks won't be able to until it's added back.`)) return;
    try {
      const [owner, name] = repo.split("/");
      await api(`/api/repos/${owner}/${name}`, { method: "DELETE" });
      await onRefreshConfig();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <main>
      <ListHeader
        title="Repos"
        count={visible.length}
        style={{ alignItems: "center" }}
        action={(
          <Stack component="form" direction="row" spacing={1} onSubmit={addRepo} sx={{ ml: "auto" }}>
            <TextField
              value={newRepo}
              onChange={(evt) => setNewRepo(evt.target.value)}
              placeholder="owner/name"
              size="small"
              autoComplete="off"
            />
            <Button type="submit" variant="outlined" size="small">Add repo</Button>
          </Stack>
        )}
      />
      {repos.length > 0 && (
        <ListToolbar>
          <ListSearchField placeholder="Search repos…" value={search} onChange={setSearch} />
        </ListToolbar>
      )}
      <ul className="repo-list">
        {visible.map((r) => {
          const isOpen = expanded.has(r.repo);
          return (
            <li key={r.repo}>
              <div className="repo-list-row" onClick={() => onOpenRepo(r.repo)}>
                <IconButton
                  size="small"
                  aria-label={isOpen ? `Hide tasks for ${r.repo}` : `Show tasks for ${r.repo}`}
                  onClick={(evt) => { evt.stopPropagation(); toggleExpanded(r.repo); }}
                >
                  {isOpen ? <ExpandMoreIcon fontSize="small" /> : <ChevronRightIcon fontSize="small" />}
                </IconButton>
                <span className="repo-list-name">{r.repo}</span>
                <span className="chips">
                  {STATE_ORDER.filter((s) => r.counts[s]).map((s) => (
                    <Chip key={s} size="small" className={`badge badge-${s}`} label={`${STATE_LABELS[s]} ${r.counts[s]}`} />
                  ))}
                  {r.blocked > 0 && <Chip size="small" color="error" label={`Blocked ${r.blocked}`} />}
                </span>
                <Typography variant="caption" color="text.secondary" whiteSpace="nowrap">
                  {r.total} task{r.total === 1 ? "" : "s"}
                </Typography>
                <IconButton
                  size="small"
                  aria-label={`New task under ${r.repo}`}
                  title={`New task under ${r.repo}`}
                  onClick={(evt) => { evt.stopPropagation(); onNewTask(r.repo); }}
                >
                  <AddIcon fontSize="small" />
                </IconButton>
                <Button
                  size="small"
                  variant="outlined"
                  onClick={(evt) => { evt.stopPropagation(); onOpenReleases(r.repo); }}
                >
                  Releases
                </Button>
                {r.configured && (
                  <Button size="small" variant="outlined" color="error" onClick={(evt) => removeRepo(evt, r.repo)}>
                    Remove
                  </Button>
                )}
              </div>
              {isOpen && (
                <ul className="task-sublist">
                  {tasks.filter((t) => t.repo === r.repo).map((t) => (
                    <li key={t.id}>
                      <TaskRow t={t} config={config} onOpenTask={onOpenTask} />
                    </li>
                  ))}
                </ul>
              )}
            </li>
          );
        })}
      </ul>
      {repos.length === 0 && (
        <ListEmpty>No repos yet -- add one above, or file a task with a target repo.</ListEmpty>
      )}
      {repos.length > 0 && visible.length === 0 && (
        <ListEmpty>No repos match your search.</ListEmpty>
      )}
    </main>
  );
}
