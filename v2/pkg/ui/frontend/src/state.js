// model.State's own vocabulary, in model.StateOf's precedence order.
export const STATE_ORDER = ["proposed", "queued", "running", "awaiting_reply", "completed", "closed"];

export const STATE_LABELS = {
  proposed: "Proposed",
  queued: "Queued",
  running: "Running",
  awaiting_reply: "Awaiting reply",
  completed: "Completed",
  closed: "Closed",
};

export function capabilityName(config, id) {
  const c = (config?.capabilities || []).find((c) => c.id === id);
  return c ? c.name : id;
}

// reposFromTasks groups tasks by their write target (Task.Repo, never
// Reads -- a read-only repo grants nothing and is not what a task
// "belongs to") into one row per repo, sorted alphabetically, for the
// repo page and its per-state counts. Tasks with no target (a proposal
// nobody has pointed at a repo yet) are omitted rather than grouped
// under a blank name.
export function reposFromTasks(tasks) {
  const byRepo = new Map();
  for (const t of tasks) {
    if (!t.repo) continue;
    if (!byRepo.has(t.repo)) {
      byRepo.set(t.repo, { repo: t.repo, total: 0, counts: {}, blocked: 0 });
    }
    const entry = byRepo.get(t.repo);
    entry.total += 1;
    entry.counts[t.state] = (entry.counts[t.state] || 0) + 1;
    if (t.blocked) entry.blocked += 1;
  }
  return [...byRepo.values()].sort((a, b) => a.repo.localeCompare(b.repo));
}
