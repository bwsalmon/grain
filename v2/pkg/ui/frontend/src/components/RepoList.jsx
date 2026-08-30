import { useState } from "react";
import AddIcon from "@mui/icons-material/Add";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import ChevronRightIcon from "@mui/icons-material/ChevronRight";
import { Button, Chip, IconButton, Typography } from "@mui/material";
import { STATE_LABELS, STATE_ORDER, reposFromTasks } from "../state.js";
import { TaskRow } from "./TaskList.jsx";

// RepoList is the repo page: one row per repo tasks actually target,
// each showing how many tasks sit in every state so a repo with
// something stuck (awaiting_reply, or a pile of blocked work) stands out
// before anyone opens it. Clicking a row is the entry point into the
// repo-centric task list -- onOpenRepo scopes App's own task view to it.
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
export default function RepoList({ tasks, config, onOpenRepo, onOpenReleases, onOpenTask, onNewTask }) {
  const repos = reposFromTasks(tasks);
  const [expanded, setExpanded] = useState(() => new Set());

  const toggleExpanded = (repo) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(repo)) next.delete(repo); else next.add(repo);
      return next;
    });
  };

  return (
    <main>
      <ul className="repo-list">
        {repos.map((r) => {
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
        <p className="empty">No repos yet -- tasks with a target repo will show up here.</p>
      )}
    </main>
  );
}
