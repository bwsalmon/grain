import { useState } from "react";
import { Button, Chip, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import { STATE_LABELS, STATE_ORDER, repoRows } from "../state.js";

// RepoList is the repo page: one row per known repo -- every
// config.targetRepos entry, plus any repo tasks target that isn't one --
// each showing how many tasks sit in every state so a repo with
// something stuck (awaiting_reply, or a pile of blocked work) stands out
// before anyone opens it. Clicking a row is the entry point into the
// repo-centric task list -- onOpenRepo scopes App's own task view to it.
// The Releases button is a second entry point into the same row (hence
// stopPropagation, so it doesn't also fire onOpenRepo): release
// management is a property of the repo (bwsalmon/agents#459), not a
// deployment-wide action, so it lives here rather than behind a sidebar
// button reachable from anywhere.
//
// Adding and removing a target repo (bwsalmon/agents#473) lives here
// too now, replacing the "Target repos" list Settings used to bury this
// behind: a repo is a thing this page is about, not a deployment knob.
// Remove only appears on a row that is actually in config.targetRepos
// (repoRows' own `configured`) -- a row that only exists because a task
// already targets it has nothing to remove, and removing a configured
// repo doesn't make the row disappear either as long as a task still
// targets it, so the two facts (targeted, configured) stay visibly
// independent rather than the button pretending removal always clears
// the row.
export default function RepoList({ tasks, config, onOpenRepo, onOpenReleases, onRefreshConfig, showError }) {
  const [newRepo, setNewRepo] = useState("");
  const repos = repoRows(config, tasks);

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
      <Stack component="form" direction="row" spacing={1} onSubmit={addRepo} sx={{ px: "1.5rem", pt: "1.2rem" }}>
        <TextField
          value={newRepo}
          onChange={(evt) => setNewRepo(evt.target.value)}
          placeholder="owner/name"
          size="small"
          autoComplete="off"
        />
        <Button type="submit" variant="outlined">Add repo</Button>
      </Stack>
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
            {r.configured && (
              <Button size="small" variant="outlined" color="error" onClick={(evt) => removeRepo(evt, r.repo)}>
                Remove
              </Button>
            )}
          </li>
        ))}
      </ul>
      {repos.length === 0 && (
        <p className="empty">No repos yet -- add one above, or file a task with a target repo.</p>
      )}
    </main>
  );
}
