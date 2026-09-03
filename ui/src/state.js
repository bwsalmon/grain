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
  // A run ending is not the work landing. model.StateOf holds a task
  // here from the moment its run finishes until its pull request is
  // merged or closed -- that is the one thing that sets Observation.
  // ClosedAt for a completed task (orchestrator.recordPullRequestEvents)
  // and so the only way out of this state -- which makes "waiting on the
  // merge queue", not "done", what it actually describes. "Completed"
  // read as done-and-landed, the one thing it never means.
  //
  // It is the ordinary reading and not the only one: a completed task
  // nobody has submitted, and one the queue has tried and given up on,
  // are not on the queue at all, and completionPhase below is the chip
  // that says so beside the badge. A completed task with no pull request
  // to land (an investigation whose run needed no code change) is the
  // one case nothing corrects -- there is no phase to report, and it is
  // rare enough not to be worth a second state of its own.
  completed: "Queued for merge",
  closed: "Closed",
};

// completionPhase names what a completed task is waiting on next when
// that is not what its state badge already says -- a human's Submit
// click, or (once the queue has tried and failed to land it
// automatically) a human again -- the distinction bwsalmon/agents#494
// asked for. model.State itself stops at "completed" for a task's entire
// post-run life (model.StateOf never reads a PR's own health), so this
// is a second axis rendered beside the state badge rather than folded
// into it, the same treatment Blocked already gets.
//
// Returns null for anything not "completed"; for completed with no pull
// request yet (a task whose run needed no code change has nothing left
// to wait on); and for the ordinary case of a pull request sitting on
// the merge queue, which is exactly what STATE_LABELS.completed says on
// the badge this chip renders beside -- a chip repeating it word for
// word would be the same fact twice rather than the correction to it
// the other two are.
export function completionPhase(t) {
  if (t.state !== "completed" || !t.pullRequest) return null;
  if (t.mergeQueueBlockedAt) {
    return {
      label: "Merge blocked",
      color: "error",
      title: "The merge queue gave up on landing this automatically -- its own comment says why. Sort it out by hand, or close it.",
    };
  }
  if (!t.autoMerge) {
    return { label: "Awaiting submit", color: "warning", title: "Not on the merge queue yet -- Submit to put it there." };
  }
  return null;
}

export function capabilityName(config, id) {
  const c = (config?.capabilities || []).find((c) => c.id === id);
  return c ? c.name : id;
}

// RETIRED_CAPABILITY_HINT labels a capability that is selected somewhere
// but no longer offered -- worded as an instruction because turning it
// off is the only thing its row is there for (capabilityRows below).
export const RETIRED_CAPABILITY_HINT = "No longer offered -- untick to remove it";

// capabilityRows is the listing any capability picker has to be built
// from: the rows this build offers, plus a row of its own for each
// selected id that has none.
//
// Every stored capability set -- a task's own grants, the deployment's
// defaults (model.Config.DefaultCapabilities) and a repo's
// (model.RepoConfig.DefaultCapabilities) -- is reported as stored, so an
// id retired since somebody chose it stays in the set an operator is
// looking at (ui.OfferedCapabilities' own "scratch-repo", now
// github-sandbox). That is deliberate: ui.Settings.DefaultCapabilities
// says as much, "an operator can only clear one they can see". But a MUI
// multiple Select only ever deselects a value through that value's own
// MenuItem, so an id with no row is a chip nothing can untick, and it is
// sent straight back on the next save -- which ui.UpdateSettings and
// ui.SetRepoDefaultCapabilities both refuse outright as "unknown
// capability", pinning the whole pane until the id goes away. The extra
// row exists purely to be turned off, which is what makes that save
// possible again.
//
// `retired` is set on those rows so a picker can label one as something
// to switch off rather than something to switch on. It is a function of
// its own because all three pickers need the same rows for the same
// reason, and one that quietly stopped adding them would be a pane
// nobody could save.
export function capabilityRows(offered, selected) {
  const rows = offered || [];
  return rows.concat(
    (selected || [])
      .filter((id) => !rows.some((c) => c.id === id))
      .map((id) => ({ id, name: id, description: RETIRED_CAPABILITY_HINT, retired: true })),
  );
}

// capabilityUnavailableHint is what a picker row says about a capability
// this deployment is not currently configured to honour -- the
// `ready`/`needs` pair GET /api/config carries per row
// (ui.Capability.Ready), turned into the sentence a human ticking the
// box needs to read.
//
// It exists because the readiness of a capability and the act of
// attaching one used to live on opposite sides of the app: Settings'
// Capabilities tab knew that gemini-key on a deployment with no GCP
// project could not work, and the picker offered it anyway. The first
// symptom was the task itself failing to dispatch, minutes later, with a
// message about a refused capability and no hint that the fix was two
// panes away in Settings.
//
// Only `ready === false` counts, never a falsy one: a row from a build
// or an endpoint that computes no readiness at all leaves the field
// absent, and "unknown" must read as no hint rather than as a warning
// against every capability on the list.
//
// It warns and does not disable. A row that cannot work today is still
// one an operator may deliberately tick -- filing the task first and
// pasting the secret second is an ordinary order to do things in -- and
// a picker that refused would also leave a capability already attached
// with no row to untick it from (capabilityRows' own reasoning).
export function capabilityUnavailableHint(c) {
  if (!c || c.ready !== false) return "";
  const needs = c.needs || [];
  const gap = needs.length > 0 ? ` -- needs ${needs.join(", ")}` : " on this deployment";
  return `Not ready${gap}. A task holding this will fail to dispatch until that is set (Settings > Capabilities).`;
}

// unionCapabilities composes the two layers of default capabilities the
// way ui.(*Client).defaultCapabilities does server-side: base (the
// deployment's own set) with extra (one repo's) appended, deduped,
// deployment-first.
//
// Union is the whole composition rule, and the only one -- a repo adds
// to what the deployment defaults and can never subtract from it
// (model.RepoConfig.DefaultCapabilities has why "everything except
// gcp-key here" is deferred rather than spelled some other way). It is a
// function of its own so the new-task form (which resolves a repo out of
// the config it already has) and the repos page (which resolves the same
// union from unsaved ticks, to say what Save is about to do) cannot
// drift into two different answers.
export function unionCapabilities(base, extra) {
  const ids = [...(base || [])];
  for (const id of extra || []) {
    if (!ids.includes(id)) ids.push(id);
  }
  return ids;
}

// defaultCapabilitiesFor is the set of capability ids a task filed
// against repo would start out holding -- the frontend's own reading of
// what ui.(*Client).defaultCapabilities resolves server-side, from the
// two layers GET /api/config reports: config.defaultCapabilities (the
// deployment's, chosen on Settings > Capabilities) and
// config.repoDefaultCapabilities[repo] (that repo's own, chosen on the
// repos page).
//
// Union, deployment first, deduped: a repo adds to what the deployment
// defaults and can never subtract from it (model.RepoConfig.
// DefaultCapabilities has why "everything except gcp-key here" is
// deferred rather than spelled some other way here). A falsy repo -- no
// repo picked yet, or a deliberately repo-less task -- has no second
// layer to add, which is the same answer CreateTask gives a task whose
// Target is nil.
//
// This is only ever a form's starting state. What a task is filed with is
// what the request names (ui.CreateTaskRequest.Capabilities); computing
// the same union here is what lets the boxes be ticked, and unticked,
// before the task exists.
export function defaultCapabilitiesFor(config, repo) {
  return unionCapabilities(
    config?.defaultCapabilities,
    repo ? config?.repoDefaultCapabilities?.[repo] : null,
  );
}

// repoRows unions the three things that can make this deployment know a
// repo at all -- config.targetRepos, every repo a task's write target
// names (Task.Repo, never Reads: a read-only repo grants nothing and is
// not what a task "belongs to"), and every repo carrying configuration
// of its own, which today is a default capability set
// (config.repoDefaultCapabilities), standing instructions
// (config.reposWithPromptExtension), or both -- into one row per repo, sorted
// alphabetically, for the repo page and its per-state counts. Tasks with
// no target (a proposal nobody has pointed at a repo yet) are omitted
// rather than grouped under a blank name.
//
// A targetRepos entry with no tasks yet still gets a (zero-count) row --
// bwsalmon/agents#473 moved adding/removing a target repo onto this
// page, which only works if a repo shows up here the moment it's added,
// before anything has ever run against it. `configured` tells a row
// apart from one that only exists because a task already targets it: the
// repos pane only offers to remove the former, since there is nothing to
// remove otherwise -- an unrestricted deployment's targetRepos is always
// empty, so every row it has is `configured: false`.
//
// The third source is there for the same reason, one step further out.
// Neither ui.(*Client).SetRepoDefaultCapabilities nor
// SetRepoPromptExtension requires a repo to be allow-listed (their own
// doc comments: a repo can be configured before it is allowed, and keeps
// its configuration after it is removed), so a repo can hold stored
// configuration while matching neither of the other two -- and this page
// is the only place that configuration can be edited, so dropping the
// row would leave it real, really applied to every task filed there, and
// unreachable. `grain repo list` (grain/task-36) reads the same sources
// for the same reason; the page and the CLI are meant to agree on which
// repos this deployment knows about.
//
// `defaults` is whether the repo carries configuration of its own, which
// on a row that is neither configured nor targeted is the only reason it
// is here at all -- the repos pane says so rather than leaving an empty
// row with nothing to explain it.
export function repoRows(config, tasks) {
  const byRepo = new Map();
  const row = (repo) => {
    if (!byRepo.has(repo)) {
      byRepo.set(repo, {
        repo,
        total: 0,
        counts: {},
        blocked: 0,
        configured: false,
        defaults: (config?.repoDefaultCapabilities?.[repo] || []).length > 0
          || (config?.reposWithPromptExtension || []).includes(repo),
      });
    }
    return byRepo.get(repo);
  };
  for (const repo of config?.targetRepos || []) {
    row(repo).configured = true;
  }
  // An absent key and an empty list mean the same thing here (nothing
  // added), the way ui.configResponse.RepoDefaultCapabilities says they
  // do, so an empty one is not a repo to put a row up for.
  for (const [repo, ids] of Object.entries(config?.repoDefaultCapabilities || {})) {
    if ((ids || []).length > 0) row(repo);
  }
  // The other half of that same third source: a repo whose only
  // configuration is standing instructions of its own
  // (ui.configResponse.ReposWithPromptExtension, which lists exactly the
  // repos that have some).
  for (const repo of config?.reposWithPromptExtension || []) {
    row(repo);
  }
  for (const t of tasks) {
    if (!t.repo) continue;
    const entry = row(t.repo);
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
//
// Two sources, not repoRows' three: a repo that only carries defaults of
// its own is not somewhere a task can be filed today (targeting a repo
// off a non-empty allowlist parks it before it dispatches,
// ui.(*Client).parkOffAllowlist), and offering it here would read as
// this deployment inviting a task it is going to park. It still gets a
// row on the repos page, which is where its defaults are edited.
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
export const STALE_BASE_STATES = ["failed", "closed"];

// suggestsBase reports whether a task is evidence of where a repo's work
// currently lives -- the filter behind both lastBaseForRepo below and
// NewTaskOverlay's own "does this repo have any history to prefill
// from?" check, which have to agree on what counts or the overlay
// prefills from a task lastBaseForRepo already looked past.
//
// Two kinds of task are no evidence. One is a task in a
// STALE_BASE_STATES state (above). The other is a task nobody filed by
// hand: a schedule firing, a suite pass, the merge queue stacking a fix,
// an agent's own propose_task. Those pick a base for their own reasons
// -- a suite run stacks every task in a pass against one throwaway
// branch, a stacked fix targets the branch of the pull request it
// repairs -- and none of it says anything about where the human filing
// the next task means to start. Letting them set the suggestion means a
// scheduled job that ran overnight silently redirects tomorrow morning's
// hand-filed task onto a branch nobody chose.
//
// authorKind is model.PrincipalKind on the wire (Task.AuthorKind).
// Missing is treated as human: a task from a store or a caller that
// never recorded one is unknown provenance, not known-automated, and
// dropping it would quietly discard history rather than protect it.
export function suggestsBase(task) {
  if (STALE_BASE_STATES.includes(task.state)) return false;
  return !task.authorKind || task.authorKind === "human";
}

// lastBaseForRepo is the branch NewTaskOverlay prefills "Base branch"
// with once a repo is picked (bwsalmon/agents#641): whatever base the
// most recently created task targeting that repo used, so a repo whose
// work currently lives off a release branch (or any other non-default
// base) doesn't make every new task against it retype that branch name.
// Empty when the most recent task on record for that repo left base
// unset too -- an empty base is itself a value worth repeating (the
// deployment's default branch), not a gap to look past for an older
// task's base.
//
// It is a suggestion, not a check: nothing here can know whether a
// branch still exists on GitHub -- that is a fact only GitHub holds, and
// this package deliberately never asks it. What it can do is not repeat
// a base that has already failed, or one no human chose in the first
// place, which is what suggestsBase above is for.
export function lastBaseForRepo(tasks, repo) {
  if (!repo) return "";
  let latest = null;
  for (const t of tasks || []) {
    if (t.repo !== repo) continue;
    if (!suggestsBase(t)) continue;
    if (!latest || new Date(t.createdAt || 0) > new Date(latest.createdAt || 0)) latest = t;
  }
  return latest ? latest.base || "" : "";
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
