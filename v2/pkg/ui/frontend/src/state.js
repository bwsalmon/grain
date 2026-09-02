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

// completionPhase names what a completed task is waiting on next --
// nothing yet, a human's Submit click, the merge queue, or (once the
// queue has tried and failed to land it automatically) a human again --
// the distinction bwsalmon/agents#494 asked for. model.State itself
// stops at "completed" for a task's entire post-run life (model.StateOf
// never reads a PR's own health), so this is a second axis rendered
// beside the state badge rather than folded into it, the same treatment
// Blocked already gets.
//
// Returns null for anything not "completed", or completed with no pull
// request yet (a task whose run needed no code change has nothing left
// to wait on).
export function completionPhase(t) {
  if (t.state !== "completed" || !t.pullRequest) return null;
  if (t.mergeQueueBlockedAt) {
    return {
      label: "Merge blocked",
      color: "error",
      title: "The merge queue tried to land this automatically and gave up -- push a fix by hand, or close it.",
    };
  }
  if (!t.autoMerge) {
    return { label: "Awaiting submit", color: "warning", title: "Submit to put this on the merge queue." };
  }
  return { label: "Queued to merge", color: "info", title: "On the merge queue -- merges automatically once its checks pass." };
}

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

// A task in one of these states is no evidence of where a repo's work
// currently lives, so lastBaseForRepo below does not suggest its base.
// The case that matters is a base branch that has been merged and
// deleted: the run pointed at it fails ("base branch 'x' does not exist
// on owner/repo", orchestrator's own prepareCheckout), and without this
// that failed task is then the newest one carrying that base -- so the
// dead branch is suggested again for the next task, which fails the same
// way, which makes it the newest again. The suggestion outlives the
// branch forever, and every task filed from it is dead on arrival.
const STALE_BASE_STATES = ["failed", "closed"];

// lastBaseForRepo is the branch NewTaskOverlay prefills "Base branch"
// with once a repo is picked (bwsalmon/agents#641): whatever base the
// most recently created task targeting that repo used, so a repo whose
// work currently lives off a release branch (or any other non-default
// base) doesn't make every new task against it retype that branch name.
// Empty when nothing on record for that repo ever set a base (the
// ordinary case of building off the deployment's default branch).
//
// It is a suggestion, not a check: nothing here can know whether a
// branch still exists on GitHub -- that is a fact only GitHub holds, and
// this package deliberately never asks it. What it can do is not repeat
// a base that has already failed, which is what STALE_BASE_STATES above
// is for.
export function lastBaseForRepo(tasks, repo) {
  if (!repo) return "";
  let latest = null;
  for (const t of tasks || []) {
    if (t.repo !== repo || !t.base) continue;
    if (STALE_BASE_STATES.includes(t.state)) continue;
    if (!latest || new Date(t.createdAt || 0) > new Date(latest.createdAt || 0)) latest = t;
  }
  return latest ? latest.base : "";
}

// frameworkLabel names an agent framework the way the UI talks about it
// -- model.AgentFrameworkAntigravity/AgentFrameworkClaude are wire
// values, not display text, and two panes (the per-task picker on New
// task, the "Agent framework" row on a task's detail) would otherwise
// each spell them out and drift. "gemini" is the former wire value for
// Antigravity and still arrives from a store written before the rename,
// so it falls through to the same label rather than being special-cased.
export function frameworkLabel(framework) {
  return framework === "claude" ? "Claude" : "Antigravity";
}
