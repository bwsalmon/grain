import { describe, expect, it } from "vitest";
import { capabilityName, completionPhase, knownRepos, repoRows } from "./state.js";

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

describe("repoRows", () => {
  it("gives a targetRepos entry with no tasks a zero-count, configured row", () => {
    const config = { targetRepos: ["acme/widgets"] };
    const rows = repoRows(config, []);
    expect(rows).toEqual([{ repo: "acme/widgets", total: 0, counts: {}, blocked: 0, configured: true }]);
  });

  it("marks a repo only known through its tasks as unconfigured", () => {
    const tasks = [{ repo: "acme/other", state: "queued", blocked: false }];
    const rows = repoRows({ targetRepos: [] }, tasks);
    expect(rows).toEqual([{ repo: "acme/other", total: 1, counts: { queued: 1 }, blocked: 0, configured: false }]);
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
    expect(widgets).toEqual({ repo: "acme/widgets", total: 1, counts: { queued: 1 }, blocked: 1, configured: true });
  });

  it("returns an empty list when nothing is configured or targeted yet", () => {
    expect(repoRows(null, [])).toEqual([]);
  });
});
