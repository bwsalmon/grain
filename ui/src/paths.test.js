import { describe, expect, it } from "vitest";
import { buildPath, parsePath } from "./paths.js";

describe("parsePath", () => {
  it("treats / as the tasks view", () => {
    expect(parsePath("/")).toEqual({ view: "tasks" });
  });

  it("parses each sidebar destination", () => {
    for (const view of ["board", "repos", "schedules", "templates"]) {
      expect(parsePath(`/${view}`)).toEqual({ view });
    }
  });

  it("falls back to tasks for the retired logs/sandboxes paths", () => {
    expect(parsePath("/logs")).toEqual({ view: "tasks" });
    expect(parsePath("/sandboxes")).toEqual({ view: "tasks" });
  });

  it("parses a task detail path", () => {
    expect(parsePath("/tasks/42")).toEqual({ view: "tasks", taskId: "42" });
  });

  it("parses a repo page path, and its releases pane", () => {
    expect(parsePath("/repos/acme/widgets")).toEqual({
      view: "repos",
      repo: "acme/widgets",
    });
    expect(parsePath("/repos/acme/widgets/releases")).toEqual({
      view: "repos",
      repo: "acme/widgets",
      showReleases: true,
    });
  });

  it("falls back to the repo list for an owner with no repo name", () => {
    expect(parsePath("/repos/acme")).toEqual({ view: "repos" });
  });

  it("parses an open schedule, template and suite", () => {
    expect(parsePath("/schedules/sched-1")).toEqual({
      view: "schedules",
      scheduleId: "sched-1",
    });
    expect(parsePath("/templates/template-1")).toEqual({
      view: "templates",
      templateId: "template-1",
    });
    expect(parsePath("/suites/suite-1")).toEqual({
      view: "suites",
      suiteId: "suite-1",
    });
  });

  it("parses the settings, system and metrics paths", () => {
    expect(parsePath("/settings")).toEqual({
      view: "tasks",
      showSettings: true,
    });
    expect(parsePath("/system")).toEqual({ view: "tasks", showSystem: true });
    expect(parsePath("/metrics")).toEqual({ view: "tasks", showMetrics: true });
  });

  it("falls back to tasks for an unknown path", () => {
    expect(parsePath("/nope")).toEqual({ view: "tasks" });
  });

  it("tolerates a trailing slash", () => {
    expect(parsePath("/repos/")).toEqual({ view: "repos" });
  });

  // grain/task-317: how a task view is narrowed rides in the query, so
  // "the failed gcp-key tasks on acme/widgets" is a link rather than a
  // set of gestures somebody repeats.
  describe("a narrowed task view", () => {
    it("reads the search, the sort and every filter out of the query", () => {
      expect(
        parsePath("/", "?q=ci&sort=title&repo=acme/widgets&capability=gcp-key"),
      ).toEqual({
        view: "tasks",
        narrowing: {
          search: "ci",
          sortBy: "title",
          filters: { repo: "acme/widgets", capability: "gcp-key" },
        },
      });
    });

    it("reports no narrowing at all for a query that asks for none", () => {
      expect(parsePath("/", "")).toEqual({ view: "tasks" });
      expect(parsePath("/", "?nothing=here")).toEqual({ view: "tasks" });
    });

    it("narrows the board and a repo's own page the same way", () => {
      expect(parsePath("/board", "?q=ci")).toEqual({
        view: "board",
        narrowing: { search: "ci", sortBy: "manual", filters: {} },
      });
      expect(parsePath("/repos/acme/widgets", "?author=grain")).toEqual({
        view: "repos",
        repo: "acme/widgets",
        narrowing: {
          search: "",
          sortBy: "manual",
          filters: { author: "grain" },
        },
      });
    });

    // A query on a path that isn't showing a task view means nothing --
    // the same treatment /settings already gives the view it was opened
    // over, which it doesn't carry either.
    it("ignores a query on a path with no task view on it", () => {
      expect(parsePath("/settings", "?q=ci")).toEqual({
        view: "tasks",
        showSettings: true,
      });
      expect(parsePath("/tasks/42", "?q=ci")).toEqual({
        view: "tasks",
        taskId: "42",
      });
      expect(parsePath("/schedules", "?repo=acme/widgets")).toEqual({
        view: "schedules",
      });
      expect(parsePath("/repos", "?repo=acme/widgets")).toEqual({
        view: "repos",
      });
      expect(parsePath("/repos/acme/widgets/releases", "?q=ci")).toEqual({
        view: "repos",
        repo: "acme/widgets",
        showReleases: true,
      });
    });

    it("falls back to the backlog's own order for a sort it has never heard of", () => {
      expect(parsePath("/", "?sort=whatever")).toEqual({ view: "tasks" });
      expect(parsePath("/", "?q=ci&sort=whatever")).toEqual({
        view: "tasks",
        narrowing: { search: "ci", sortBy: "manual", filters: {} },
      });
    });

    // Nothing here can tell whether "acme/gone" is still a repo any
    // task carries -- that is a question about the tasks. A stale link
    // parses as written and the view resolves it (filterViews reads a
    // choice it cannot offer as "any").
    it("takes a filter value on trust, however stale", () => {
      expect(parsePath("/", "?repo=acme/gone")).toEqual({
        view: "tasks",
        narrowing: {
          search: "",
          sortBy: "manual",
          filters: { repo: "acme/gone" },
        },
      });
    });

    it("reads 'has none of these' as the answer it is", () => {
      expect(parsePath("/", "?repo=__none__").narrowing.filters).toEqual({
        repo: "__none__",
      });
    });
  });
});

describe("buildPath", () => {
  it("builds / for the tasks view", () => {
    expect(buildPath({ view: "tasks" })).toBe("/");
  });

  it("builds a plain path for the other sidebar views", () => {
    expect(buildPath({ view: "repos" })).toBe("/repos");
    expect(buildPath({ view: "board" })).toBe("/board");
  });

  it("builds a repo's own path, and its releases pane's", () => {
    expect(buildPath({ view: "repos", repo: "acme/widgets" })).toBe(
      "/repos/acme/widgets",
    );
    expect(
      buildPath({ view: "repos", repo: "acme/widgets", showReleases: true }),
    ).toBe("/repos/acme/widgets/releases");
  });

  it("ignores an open repo outside the repos view", () => {
    expect(buildPath({ view: "tasks", repo: "acme/widgets" })).toBe("/");
  });

  it("builds an open schedule's, template's and suite's own path", () => {
    expect(buildPath({ view: "schedules", scheduleId: "sched-1" })).toBe(
      "/schedules/sched-1",
    );
    expect(buildPath({ view: "templates", templateId: "template-1" })).toBe(
      "/templates/template-1",
    );
    expect(buildPath({ view: "suites", suiteId: "suite-1" })).toBe(
      "/suites/suite-1",
    );
  });

  it("ignores an open item outside its own view", () => {
    expect(buildPath({ view: "templates", scheduleId: "sched-1" })).toBe(
      "/templates",
    );
  });

  it("prefers an open task over the underlying view", () => {
    expect(buildPath({ view: "repos", taskId: "42" })).toBe("/tasks/42");
  });

  it("prefers settings over both a view and an open task", () => {
    expect(buildPath({ view: "repos", taskId: "42", showSettings: true })).toBe(
      "/settings",
    );
  });

  it("builds the system and metrics panes' own paths", () => {
    expect(buildPath({ view: "tasks", showSystem: true })).toBe("/system");
    expect(buildPath({ view: "repos", taskId: "42", showMetrics: true })).toBe(
      "/metrics",
    );
  });

  describe("a narrowed task view", () => {
    const narrowing = {
      search: "ci",
      sortBy: "title",
      filters: { repo: "acme/widgets", capability: "gcp-key" },
    };

    it("writes the narrowing into the query, search first and filters in FILTERS' order", () => {
      expect(buildPath({ view: "tasks", narrowing })).toBe(
        "/?q=ci&sort=title&repo=acme/widgets&capability=gcp-key",
      );
    });

    it("leaves an un-narrowed list at its own path", () => {
      expect(
        buildPath({
          view: "tasks",
          narrowing: { search: "", sortBy: "manual", filters: {} },
        }),
      ).toBe("/");
    });

    it("narrows the board and a repo's own page too", () => {
      expect(buildPath({ view: "board", narrowing: { search: "ci" } })).toBe(
        "/board?q=ci",
      );
      expect(
        buildPath({
          view: "repos",
          repo: "acme/widgets",
          narrowing: { search: "ci" },
        }),
      ).toBe("/repos/acme/widgets?q=ci");
    });

    // The narrowing is dropped for as long as something covers the list
    // -- App goes on holding it, and closing the pane puts it back in
    // the address bar, the same way the view behind /settings does.
    it("drops the narrowing from a path with no task view on it", () => {
      expect(buildPath({ view: "tasks", narrowing, showSettings: true })).toBe(
        "/settings",
      );
      expect(buildPath({ view: "tasks", narrowing, taskId: "42" })).toBe(
        "/tasks/42",
      );
      expect(buildPath({ view: "schedules", narrowing })).toBe("/schedules");
      expect(buildPath({ view: "repos", narrowing })).toBe("/repos");
      expect(
        buildPath({
          view: "repos",
          repo: "acme/widgets",
          showReleases: true,
          narrowing,
        }),
      ).toBe("/repos/acme/widgets/releases");
    });

    it("round-trips every narrowed path parsePath recognizes", () => {
      const paths = [
        "/?q=ci",
        "/?sort=title",
        "/?repo=acme/widgets&capability=gcp-key",
        "/?q=ci&sort=oldest&repo=__none__&base=main&author=grain",
        "/?origin=schedule&kind=interactive&autoMerge=on",
        "/board?q=ci&repo=acme/widgets",
        "/repos/acme/widgets?q=ci&sort=newest",
      ];
      for (const path of paths) {
        const [pathname, search] = path.split("?");
        expect(buildPath(parsePath(pathname, `?${search}`))).toBe(path);
      }
    });
  });

  it("round-trips every path parsePath recognizes", () => {
    const paths = [
      "/",
      "/board",
      "/repos",
      "/schedules",
      "/templates",
      "/suites",
      "/tasks/42",
      "/settings",
      "/system",
      "/metrics",
      "/repos/acme/widgets",
      "/repos/acme/widgets/releases",
      "/schedules/sched-1",
      "/templates/template-1",
      "/suites/suite-1",
    ];
    for (const path of paths) {
      expect(buildPath(parsePath(path))).toBe(path);
    }
  });
});
