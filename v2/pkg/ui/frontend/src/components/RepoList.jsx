import { Chip, Typography } from "@mui/material";
import { STATE_LABELS, STATE_ORDER, reposFromTasks } from "../state.js";

// RepoList is the repo page: one row per repo tasks actually target,
// each showing how many tasks sit in every state so a repo with
// something stuck (awaiting_reply, or a pile of blocked work) stands out
// before anyone opens it. Clicking a row is the entry point into the
// repo-centric task list -- onOpenRepo scopes App's own task view to it.
export default function RepoList({ tasks, onOpenRepo }) {
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
          </li>
        ))}
      </ul>
      {repos.length === 0 && (
        <p className="empty">No repos yet -- tasks with a target repo will show up here.</p>
      )}
    </main>
  );
}
