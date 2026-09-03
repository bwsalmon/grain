import { describe, expect, it } from "vitest";
import { buildPath, parsePath } from "./paths.js";

describe("parsePath", () => {
  it("treats / as the tasks view", () => {
    expect(parsePath("/")).toEqual({ view: "tasks" });
  });

  it("parses each sidebar destination", () => {
    for (const view of ["repos", "schedules", "templates"]) {
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
    expect(parsePath("/repos/acme/widgets")).toEqual({ view: "repos", repo: "acme/widgets" });
    expect(parsePath("/repos/acme/widgets/releases")).toEqual({
      view: "repos", repo: "acme/widgets", showReleases: true,
    });
  });

  it("falls back to the repo list for an owner with no repo name", () => {
    expect(parsePath("/repos/acme")).toEqual({ view: "repos" });
  });

  it("parses an open schedule, template and suite", () => {
    expect(parsePath("/schedules/sched-1")).toEqual({ view: "schedules", scheduleId: "sched-1" });
    expect(parsePath("/templates/template-1")).toEqual({ view: "templates", templateId: "template-1" });
    expect(parsePath("/suites/suite-1")).toEqual({ view: "suites", suiteId: "suite-1" });
  });

  it("parses the settings path", () => {
    expect(parsePath("/settings")).toEqual({ view: "tasks", showSettings: true });
  });

  it("falls back to tasks for an unknown path", () => {
    expect(parsePath("/nope")).toEqual({ view: "tasks" });
  });

  it("tolerates a trailing slash", () => {
    expect(parsePath("/repos/")).toEqual({ view: "repos" });
  });
});

describe("buildPath", () => {
  it("builds / for the tasks view", () => {
    expect(buildPath({ view: "tasks" })).toBe("/");
  });

  it("builds a plain path for the other sidebar views", () => {
    expect(buildPath({ view: "repos" })).toBe("/repos");
  });

  it("builds a repo's own path, and its releases pane's", () => {
    expect(buildPath({ view: "repos", repo: "acme/widgets" })).toBe("/repos/acme/widgets");
    expect(buildPath({ view: "repos", repo: "acme/widgets", showReleases: true }))
      .toBe("/repos/acme/widgets/releases");
  });

  it("ignores an open repo outside the repos view", () => {
    expect(buildPath({ view: "tasks", repo: "acme/widgets" })).toBe("/");
  });

  it("builds an open schedule's, template's and suite's own path", () => {
    expect(buildPath({ view: "schedules", scheduleId: "sched-1" })).toBe("/schedules/sched-1");
    expect(buildPath({ view: "templates", templateId: "template-1" })).toBe("/templates/template-1");
    expect(buildPath({ view: "suites", suiteId: "suite-1" })).toBe("/suites/suite-1");
  });

  it("ignores an open item outside its own view", () => {
    expect(buildPath({ view: "templates", scheduleId: "sched-1" })).toBe("/templates");
  });

  it("prefers an open task over the underlying view", () => {
    expect(buildPath({ view: "repos", taskId: "42" })).toBe("/tasks/42");
  });

  it("prefers settings over both a view and an open task", () => {
    expect(buildPath({ view: "repos", taskId: "42", showSettings: true })).toBe("/settings");
  });

  it("round-trips every path parsePath recognizes", () => {
    const paths = [
      "/", "/repos", "/schedules", "/templates", "/suites", "/tasks/42", "/settings",
      "/repos/acme/widgets", "/repos/acme/widgets/releases",
      "/schedules/sched-1", "/templates/template-1", "/suites/suite-1",
    ];
    for (const path of paths) {
      expect(buildPath(parsePath(path))).toBe(path);
    }
  });
});
