import { Button, Chip, Typography } from "@mui/material";
import { STATE_LABELS, STATE_ORDER, reposFromTasks } from "../state.js";

// RepoList is the repo page: one row per repo tasks actually target,
// each showing how many tasks sit in every state so a repo with
// something stuck (awaiting_reply, or a pile of blocked work) stands out
// before anyone opens it. Clicking a row is the entry point into the
// repo-centric task list -- onOpenRepo scopes App's own task view to it.
// The Releases button is a second entry point into the same row (hence
// stopPropagation, so it doesn't also fire onOpenRepo): release
// management is a property of the repo (bwsalmon/agents#459), not a
// deployment-wide action, so it lives here rather than behind a sidebar
// button reachable from anywhere.
export default function RepoList({ tasks, onOpenRepo, onOpenReleases }) {
  const repos = reposFromTasks(tasks);

  return (
    <main>
      <ul className="repo-list">
        {repos.map((r) => (
          <li key={r.repo} onClick={() => onOpenRepo(r.repo)}>
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
            <Button
              size="small"
              variant="outlined"
              onClick={(evt) => { evt.stopPropagation(); onOpenReleases(r.repo); }}
            >
              Releases
            </Button>
          </li>
        ))}
      </ul>
      {repos.length === 0 && (
        <p className="empty">No repos yet -- tasks with a target repo will show up here.</p>
      )}
    </main>
  );
}
