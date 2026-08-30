// model.State's own vocabulary, in model.StateOf's precedence order.
export const STATE_ORDER = ["proposed", "queued", "running", "awaiting_reply", "failed", "completed", "closed"];

export const STATE_LABELS = {
  proposed: "Proposed",
  queued: "Queued",
  running: "Running",
  awaiting_reply: "Awaiting reply",
  // A task stops here, not "queued", once it has failed
  // model.MaxConsecutiveFailures runs in a row -- see StateFailed's own
  // doc comment. "Retry" (DetailOverlay's own Actions) is the only way
  // out.
  failed: "Failed",
  completed: "Completed",
  closed: "Closed",
};

export function capabilityName(config, id) {
  const c = (config?.capabilities || []).find((c) => c.id === id);
  return c ? c.name : id;
}

// repoRows unions config.targetRepos with every repo a task's write
// target names (Task.Repo, never Reads -- a read-only repo grants
// nothing and is not what a task "belongs to") into one row per repo,
// sorted alphabetically, for the repo page and its per-state counts.
// Tasks with no target (a proposal nobody has pointed at a repo yet) are
// omitted rather than grouped under a blank name.
//
// A targetRepos entry with no tasks yet still gets a (zero-count) row --
// bwsalmon/agents#473 moved adding/removing a target repo onto this
// page, which only works if a repo shows up here the moment it's added,
// before anything has ever run against it. `configured` tells a row
// apart from one that only exists because a task already targets it: the
// repos pane only offers to remove the former, since there is nothing to
// remove otherwise -- an unrestricted deployment's targetRepos is always
// empty, so every row it has is `configured: false`.
export function repoRows(config, tasks) {
  const byRepo = new Map();
  for (const repo of config?.targetRepos || []) {
    byRepo.set(repo, { repo, total: 0, counts: {}, blocked: 0, configured: true });
  }
  for (const t of tasks) {
    if (!t.repo) continue;
    if (!byRepo.has(t.repo)) {
      byRepo.set(t.repo, { repo: t.repo, total: 0, counts: {}, blocked: 0, configured: false });
    }
    const entry = byRepo.get(t.repo);
    entry.total += 1;
    entry.counts[t.state] = (entry.counts[t.state] || 0) + 1;
    if (t.blocked) entry.blocked += 1;
  }
  return [...byRepo.values()].sort((a, b) => a.repo.localeCompare(b.repo));
}

// knownRepos is the option list behind the repo dropdowns
// (bwsalmon/agents#447): config.targetRepos (what CreateTask actually
// enforces a task's repo against, when the deployment restricts it) union
// whatever repo any existing task already targets -- so an unrestricted
// deployment still gets a useful dropdown once it has filed at least one
// task, rather than staying a bare text field forever. Sorted and
// deduped the same way repoRows already sorts its own rows.
export function knownRepos(config, tasks) {
  const repos = new Set(config?.targetRepos || []);
  for (const t of tasks || []) {
    if (t.repo) repos.add(t.repo);
  }
  return [...repos].sort((a, b) => a.localeCompare(b));
}
