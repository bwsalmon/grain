import { describe, expect, it } from "vitest";
import { RETIRED_CAPABILITY_HINT, capabilityName, capabilityRows, capabilityUnavailableHint, closablePullRequest, completionPhase, defaultCapabilitiesFor, knownRepos, lastBaseForRepo, orphanedPullRequest, relativeAge, repoRows, runActivity, stateLabel, unionCapabilities } from "./state.js";

describe("completionPhase", () => {
  it("returns null for a task whose run is not over", () => {
    expect(completionPhase({ state: "queued", pullRequest: "acme/widgets#1" })).toBeNull();
  });

  it("returns null for a completed task with no pull request", () => {
    expect(completionPhase({ state: "completed", pullRequest: "" })).toBeNull();
  });

  // Waiting on a human's Submit click is a state now
  // (STATE_LABELS.awaiting_submit), not a chip: the badge says it, so
  // there is nothing left for a chip beside it to correct.
  it("puts up no chip for a task waiting on a Submit click", () => {
    expect(completionPhase({ state: "awaiting_submit", pullRequest: "acme/widgets#1", autoMerge: false })).toBeNull();
  });

  // The state badge beside it already reads "Queued for merge"
  // (STATE_LABELS.completed), so the ordinary case has no correction to
  // make and puts up no chip either.
  it("returns null once auto-merge is set and the queue has it", () => {
    expect(completionPhase({ state: "completed", pullRequest: "acme/widgets#1", autoMerge: true })).toBeNull();
  });

  it("reports merge blocked once the merge queue has given up, even with auto-merge set", () => {
    const phase = completionPhase({
      state: "completed",
      pullRequest: "acme/widgets#1",
      autoMerge: true,
      mergeQueueBlockedAt: "2026-08-01T00:00:00Z",
    });
    expect(phase.label).toBe("Merge blocked");
  });
});

describe("capabilityName", () => {
  const config = { capabilities: [{ id: "web-search", name: "Web search" }] };

  it("returns the matching capability's name", () => {
    expect(capabilityName(config, "web-search")).toBe("Web search");
  });

  it("falls back to the id when no capability matches", () => {
    expect(capabilityName(config, "unknown")).toBe("unknown");
  });

  it("falls back to the id when config is null", () => {
    expect(capabilityName(null, "web-search")).toBe("web-search");
  });

  it("falls back to the id when config has no capabilities", () => {
    expect(capabilityName({}, "web-search")).toBe("web-search");
  });
});

describe("capabilityRows", () => {
  const offered = [{ id: "gcp-key", name: "GCP key" }, { id: "gemini-key", name: "Gemini key" }];

  it("is the offered listing untouched when everything selected has a row", () => {
    expect(capabilityRows(offered, ["gcp-key"])).toEqual(offered);
  });

  it("appends a row for a selected id with none, so it can be unticked", () => {
    expect(capabilityRows(offered, ["gcp-key", "scratch-repo"])).toEqual([
      ...offered,
      { id: "scratch-repo", name: "scratch-repo", description: RETIRED_CAPABILITY_HINT, retired: true },
    ]);
  });

  it("appends nothing for an id that is offered but not selected", () => {
    expect(capabilityRows(offered, [])).toEqual(offered);
  });

  it("tolerates a missing listing or selection", () => {
    expect(capabilityRows(undefined, undefined)).toEqual([]);
    expect(capabilityRows(null, ["scratch-repo"])).toEqual([
      { id: "scratch-repo", name: "scratch-repo", description: RETIRED_CAPABILITY_HINT, retired: true },
    ]);
  });
});

describe("capabilityUnavailableHint", () => {
  it("names every gap a capability this deployment cannot honour still has", () => {
    const hint = capabilityUnavailableHint({
      id: "gemini-key", ready: false, needs: ["GCP project", "gcp-key-minter"],
    });
    expect(hint).toContain("GCP project");
    expect(hint).toContain("gcp-key-minter");
    expect(hint).toContain("fail to dispatch");
  });

  it("still warns when the gap has no name attached to it", () => {
    expect(capabilityUnavailableHint({ id: "gemini-key", ready: false })).toContain("Not ready");
  });

  it("says nothing about a ready capability", () => {
    expect(capabilityUnavailableHint({ id: "gemini-key", ready: true })).toBe("");
  });

  // A build or an endpoint that computes no readiness leaves the field
  // off entirely, and "unknown" must not read as a warning against every
  // capability on the list.
  it("says nothing when readiness is unknown", () => {
    expect(capabilityUnavailableHint({ id: "gemini-key" })).toBe("");
    expect(capabilityUnavailableHint({ id: "x", retired: true })).toBe("");
    expect(capabilityUnavailableHint(undefined)).toBe("");
  });
});

describe("knownRepos", () => {
  it("unions targetRepos and repos already seen on tasks, sorted and deduped", () => {
    const config = { targetRepos: ["acme/widgets", "acme/other"] };
    const tasks = [{ repo: "acme/other" }, { repo: "acme/newer" }, { repo: "" }];
    expect(knownRepos(config, tasks)).toEqual(["acme/newer", "acme/other", "acme/widgets"]);
  });

  it("returns an empty list when nothing is configured or targeted yet", () => {
    expect(knownRepos(null, [])).toEqual([]);
    expect(knownRepos(null, null)).toEqual([]);
  });

  it("falls back to tasks alone on an unrestricted deployment", () => {
    expect(knownRepos({ targetRepos: [] }, [{ repo: "acme/widgets" }])).toEqual(["acme/widgets"]);
  });
});

describe("lastBaseForRepo", () => {
  it("returns the most recently created task's base for that repo", () => {
    const tasks = [
      { repo: "acme/widgets", base: "release/1", createdAt: "2026-08-01T00:00:00Z" },
      { repo: "acme/widgets", base: "release/2", createdAt: "2026-08-02T00:00:00Z" },
    ];
    expect(lastBaseForRepo(tasks, "acme/widgets")).toBe("release/2");
  });

  it("returns empty when the most recent task on record left base unset, even if an older one set one", () => {
    const tasks = [
      { repo: "acme/widgets", base: "release/1", createdAt: "2026-08-01T00:00:00Z" },
      { repo: "acme/widgets", base: "", createdAt: "2026-08-02T00:00:00Z" },
    ];
    expect(lastBaseForRepo(tasks, "acme/widgets")).toBe("");
  });

  it("skips a failed or closed task and falls back to an older one", () => {
    const tasks = [
      { repo: "acme/widgets", base: "release/1", createdAt: "2026-08-01T00:00:00Z", state: "completed" },
      { repo: "acme/widgets", base: "release/2", createdAt: "2026-08-02T00:00:00Z", state: "failed" },
    ];
    expect(lastBaseForRepo(tasks, "acme/widgets")).toBe("release/1");
  });

  // A schedule, a suite pass, the merge queue and an agent's own
  // propose_task all pick a base for their own reasons -- none of it is
  // the human's choice of where the next hand-filed task starts.
  it("skips a task an agent or automation filed and falls back to an older one", () => {
    const tasks = [
      { repo: "acme/widgets", base: "release/1", createdAt: "2026-08-01T00:00:00Z", authorKind: "human" },
      { repo: "acme/widgets", base: "suite/run-7", createdAt: "2026-08-02T00:00:00Z", authorKind: "automation" },
      { repo: "acme/widgets", base: "grain/task-3", createdAt: "2026-08-03T00:00:00Z", authorKind: "agent" },
    ];
    expect(lastBaseForRepo(tasks, "acme/widgets")).toBe("release/1");
  });

  it("returns empty when every task on record for the repo was filed by an agent or automation", () => {
    const tasks = [
      { repo: "acme/widgets", base: "suite/run-7", createdAt: "2026-08-02T00:00:00Z", authorKind: "automation" },
    ];
    expect(lastBaseForRepo(tasks, "acme/widgets")).toBe("");
  });

  it("still suggests a task carrying no authorKind at all", () => {
    const tasks = [{ repo: "acme/widgets", base: "release/1", createdAt: "2026-08-01T00:00:00Z" }];
    expect(lastBaseForRepo(tasks, "acme/widgets")).toBe("release/1");
  });

  it("returns empty when no repo is given", () => {
    expect(lastBaseForRepo([{ repo: "acme/widgets", base: "release/1" }], "")).toBe("");
  });

  it("returns empty when the repo has no tasks on record", () => {
    expect(lastBaseForRepo([], "acme/widgets")).toBe("");
  });
});

describe("repoRows", () => {
  it("gives a targetRepos entry with no tasks a zero-count, configured row", () => {
    const config = { targetRepos: ["acme/widgets"] };
    const rows = repoRows(config, []);
    expect(rows).toEqual([{ repo: "acme/widgets", total: 0, counts: {}, blocked: 0, configured: true, defaults: false }]);
  });

  it("marks a repo only known through its tasks as unconfigured", () => {
    const tasks = [{ repo: "acme/other", state: "queued", blocked: false }];
    const rows = repoRows({ targetRepos: [] }, tasks);
    expect(rows).toEqual([
      { repo: "acme/other", total: 1, counts: { queued: 1 }, blocked: 0, configured: false, defaults: false },
    ]);
  });

  it("unions targetRepos and task repos, sorted, without duplicating an entry that is both", () => {
    const config = { targetRepos: ["acme/widgets", "acme/other"] };
    const tasks = [
      { repo: "acme/widgets", state: "queued", blocked: true },
      { repo: "acme/newer", state: "completed", blocked: false },
      { repo: "", state: "proposed", blocked: false },
    ];
    const rows = repoRows(config, tasks);
    expect(rows.map((r) => r.repo)).toEqual(["acme/newer", "acme/other", "acme/widgets"]);

    const widgets = rows.find((r) => r.repo === "acme/widgets");
    expect(widgets).toEqual({
      repo: "acme/widgets", total: 1, counts: { queued: 1 }, blocked: 1, configured: true, defaults: false,
    });
  });

  // A repo can hold defaults of its own without being allow-listed and
  // without any task targeting it (ui.(*Client).SetRepoDefaultCapabilities
  // deliberately doesn't require either), and this page is the only place
  // that set can be edited -- so it has to have a row here.
  it("gives a repo that only carries defaults of its own a row of its own", () => {
    const config = { targetRepos: [], repoDefaultCapabilities: { "acme/orphan": ["gcp-key"] } };
    const rows = repoRows(config, []);
    expect(rows).toEqual([
      { repo: "acme/orphan", total: 0, counts: {}, blocked: 0, configured: false, defaults: true },
    ]);
  });

  // The same argument for the other kind of per-repo configuration: a
  // repo whose only presence here is standing instructions of its own
  // (ui.configResponse.ReposWithPromptExtension, grain/task-114), which
  // reach every run against it and can only be read or edited here.
  it("gives a repo that only carries a prompt extension a row of its own", () => {
    const config = { targetRepos: [], reposWithPromptExtension: ["acme/orphan"] };
    const rows = repoRows(config, []);
    expect(rows).toEqual([
      { repo: "acme/orphan", total: 0, counts: {}, blocked: 0, configured: false, defaults: true },
    ]);
  });

  // And the third kind (grain/task-154): a repo whose only presence here
  // is the setup command grain runs in every checkout it makes there
  // (ui.configResponse.ReposWithSetupCommand), which is as invisible
  // from anywhere else as the two above.
  it("gives a repo that only carries a setup command a row of its own", () => {
    const config = { targetRepos: [], reposWithSetupCommand: ["acme/orphan"] };
    const rows = repoRows(config, []);
    expect(rows).toEqual([
      { repo: "acme/orphan", total: 0, counts: {}, blocked: 0, configured: false, defaults: true },
    ]);
  });

  it("does not duplicate a repo that carries defaults and is also allow-listed or targeted", () => {
    const config = {
      targetRepos: ["acme/widgets"],
      repoDefaultCapabilities: { "acme/widgets": ["gcp-key"], "acme/other": ["gcp-key"] },
    };
    const tasks = [{ repo: "acme/other", state: "queued", blocked: false }];
    const rows = repoRows(config, tasks);
    expect(rows.map((r) => r.repo)).toEqual(["acme/other", "acme/widgets"]);
    expect(rows.find((r) => r.repo === "acme/widgets")).toEqual({
      repo: "acme/widgets", total: 0, counts: {}, blocked: 0, configured: true, defaults: true,
    });
    expect(rows.find((r) => r.repo === "acme/other")).toEqual({
      repo: "acme/other", total: 1, counts: { queued: 1 }, blocked: 0, configured: false, defaults: true,
    });
  });

  // An absent key and an empty list mean the same thing on /api/config --
  // this repo adds nothing -- so an empty one is not a repo to list.
  it("gives no row to a repoDefaultCapabilities key with an empty set", () => {
    expect(repoRows({ repoDefaultCapabilities: { "acme/orphan": [] } }, [])).toEqual([]);
  });

  it("returns an empty list when nothing is configured or targeted yet", () => {
    expect(repoRows(null, [])).toEqual([]);
  });
});

describe("unionCapabilities", () => {
  it("appends the second layer to the first, deduped and in order", () => {
    expect(unionCapabilities(["gemini-key"], ["gcp-key", "gemini-key"])).toEqual(["gemini-key", "gcp-key"]);
  });

  it("treats a missing layer as nothing to add", () => {
    expect(unionCapabilities(undefined, undefined)).toEqual([]);
    expect(unionCapabilities(["gcp-key"], null)).toEqual(["gcp-key"]);
  });
});

describe("defaultCapabilitiesFor", () => {
  const config = {
    defaultCapabilities: ["gemini-key"],
    repoDefaultCapabilities: { "acme/widgets": ["gcp-key"] },
  };

  it("adds the repo's own defaults to the deployment's", () => {
    expect(defaultCapabilitiesFor(config, "acme/widgets")).toEqual(["gemini-key", "gcp-key"]);
  });

  it("gives a repo with none of its own the deployment's alone", () => {
    expect(defaultCapabilitiesFor(config, "acme/gadgets")).toEqual(["gemini-key"]);
  });

  // No repo picked, or a deliberately repo-less task: there is no second
  // layer to key on, the same answer CreateTask gives a task whose
  // Target is nil.
  it("gives a task with no repo the deployment's alone", () => {
    expect(defaultCapabilitiesFor(config, "")).toEqual(["gemini-key"]);
  });

  it("survives a config that has never been loaded", () => {
    expect(defaultCapabilitiesFor(null, "acme/widgets")).toEqual([]);
  });
});

describe("orphanedPullRequest", () => {
  it("names the pull request a closed task has left open", () => {
    expect(orphanedPullRequest({ state: "closed", pullRequest: "acme/widgets#1" })).toBe("acme/widgets#1");
  });

  // The ordinary ending, not an orphan: a merged pull request closes its
  // own task (orchestrator.recordPullRequestEvents sets ClosedAt
  // alongside PrMergedAt), so the state and the link alone cannot tell
  // the two apart.
  it("says nothing about a task closed because its pull request merged", () => {
    expect(orphanedPullRequest({
      state: "closed",
      pullRequest: "acme/widgets#1",
      pullRequestEvents: [{ kind: "opened" }, { kind: "merged" }],
    })).toBeNull();
  });

  it("says nothing about a pull request that closed without merging", () => {
    expect(orphanedPullRequest({
      state: "closed",
      pullRequest: "acme/widgets#1",
      pullRequestEvents: [{ kind: "closed" }],
    })).toBeNull();
  });

  it("says nothing about a task that is not closed, or has no pull request", () => {
    expect(orphanedPullRequest({ state: "completed", pullRequest: "acme/widgets#1" })).toBeNull();
    expect(orphanedPullRequest({ state: "closed" })).toBeNull();
  });
});

// The same pull request a moment earlier, while closing the task is still
// a choice: this is what decides whether the Close button is offered the
// checkbox that closes it too.
describe("closablePullRequest", () => {
  it("names the pull request closing this task would orphan", () => {
    expect(closablePullRequest({ state: "completed", pullRequest: "acme/widgets#1" })).toBe("acme/widgets#1");
    expect(closablePullRequest({ state: "running", pullRequest: "acme/widgets#1" })).toBe("acme/widgets#1");
  });

  it("names nothing when there is no open pull request to close", () => {
    expect(closablePullRequest({ state: "completed" })).toBeNull();
    expect(closablePullRequest({
      state: "completed",
      pullRequest: "acme/widgets#1",
      pullRequestEvents: [{ kind: "merged" }],
    })).toBeNull();
    expect(closablePullRequest({
      state: "completed",
      pullRequest: "acme/widgets#1",
      pullRequestEvents: [{ kind: "closed" }],
    })).toBeNull();
  });

  // A task already closed has no close to make the choice at: the warning
  // orphanedPullRequest drives is what that task gets instead.
  it("names nothing on a task that is already closed", () => {
    expect(closablePullRequest({ state: "closed", pullRequest: "acme/widgets#1" })).toBeNull();
  });
});

// A task the merge queue is repairing is running (or queued) like any
// other attempt, and the state alone cannot say which kind of work it is
// -- so the label does, and StateDot colours the mark to match.
describe("stateLabel", () => {
  it("uses the plain state label when nothing is being repaired", () => {
    expect(stateLabel({ state: "running" })).toBe("Running");
    expect(stateLabel({ state: "queued" })).toBe("Queued");
    expect(stateLabel({ state: "completed" })).toBe("Queued for merge");
  });

  it("says what a merge-queue repair is doing instead", () => {
    expect(stateLabel({ state: "running", repairing: true })).toBe("Repairing");
    expect(stateLabel({ state: "queued", repairing: true })).toBe("Queued for repair");
  });

  // Only those two states are ever a repair in flight, so anything else
  // arriving with the flag set keeps the badge honest rather than
  // inventing a phrase for a combination the backend does not produce.
  it("leaves any other state's label alone", () => {
    expect(stateLabel({ state: "awaiting_reply", repairing: true })).toBe("Awaiting reply");
  });

  it("falls back to the raw state for one it has no label for", () => {
    expect(stateLabel({ state: "invented" })).toBe("invented");
  });
});

describe("runActivity", () => {
  const at = (msAgo) => new Date(Date.UTC(2026, 8, 4, 12, 0, 0) - msAgo).toISOString();
  const now = Date.UTC(2026, 8, 4, 12, 0, 0);

  it("gives back what a running task's own agent said, and how old it is", () => {
    expect(runActivity({ state: "running", activity: "waiting for CI", activityAt: at(4 * 60_000) }, now))
      .toEqual({ note: "waiting for CI", age: "4m", bySetup: false });
  });

  // grain writes this field too, for the stretch before the agent's
  // first turn (orchestrator.setupNotes) -- and says so, since every
  // other phrase that appears here is something an agent wrote.
  it("carries whether the phrase is grain's own rather than the run's", () => {
    expect(runActivity({
      state: "running", activity: "building a sandbox", activityAt: at(30_000), activityBySetup: true,
    }, now)).toEqual({ note: "building a sandbox", age: "now", bySetup: true });
  });

  // The API only carries a synopsis for a live run, but a poll's answer
  // can be a few seconds old -- and a phrase left beside a "Completed"
  // badge reads as a run still going.
  it("says nothing once the task has stopped running", () => {
    expect(runActivity({ state: "completed", activity: "waiting for CI", activityAt: at(0) }, now)).toBeNull();
    expect(runActivity({ state: "failed", activity: "waiting for CI", activityAt: at(0) }, now)).toBeNull();
  });

  it("says nothing for a running task whose agent has not said anything", () => {
    expect(runActivity({ state: "running" }, now)).toBeNull();
  });

  // A row written before the timestamp column existed has a note and no
  // time; showing the note alone beats inventing an age for it.
  it("shows a note with no timestamp on its own", () => {
    expect(runActivity({ state: "running", activity: "reading the code" }, now))
      .toEqual({ note: "reading the code", age: null, bySetup: false });
  });
});

describe("relativeAge", () => {
  const now = Date.UTC(2026, 8, 4, 12, 0, 0);
  const ago = (ms) => new Date(now - ms).toISOString();

  it("is coarse on purpose: now, minutes, hours, days", () => {
    expect(relativeAge(ago(5_000), now)).toBe("now");
    expect(relativeAge(ago(90_000), now)).toBe("1m");
    expect(relativeAge(ago(3 * 3_600_000), now)).toBe("3h");
    expect(relativeAge(ago(50 * 3_600_000), now)).toBe("2d");
  });

  // A browser clock a second ahead of the daemon's is not worth showing
  // anybody a negative age over.
  it("never renders a future timestamp as negative", () => {
    expect(relativeAge(new Date(now + 5_000).toISOString(), now)).toBe("now");
  });
});
