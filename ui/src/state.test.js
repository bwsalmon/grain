import { describe, expect, it } from "vitest";
import { RETIRED_CAPABILITY_HINT, capabilityName, capabilityRows, completionPhase, defaultCapabilitiesFor, knownRepos, lastBaseForRepo, repoRows, unionCapabilities } from "./state.js";

describe("completionPhase", () => {
  it("returns null for a task that is not completed", () => {
    expect(completionPhase({ state: "queued", pullRequest: "acme/widgets#1" })).toBeNull();
  });

  it("returns null for a completed task with no pull request", () => {
    expect(completionPhase({ state: "completed", pullRequest: "" })).toBeNull();
  });

  it("reports awaiting submit for a completed task with a PR and no auto-merge", () => {
    const phase = completionPhase({ state: "completed", pullRequest: "acme/widgets#1", autoMerge: false });
    expect(phase.label).toBe("Awaiting submit");
  });

  it("reports queued to merge once auto-merge is set", () => {
    const phase = completionPhase({ state: "completed", pullRequest: "acme/widgets#1", autoMerge: true });
    expect(phase.label).toBe("Queued to merge");
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
