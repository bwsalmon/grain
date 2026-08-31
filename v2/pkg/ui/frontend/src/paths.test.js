import { describe, expect, it } from "vitest";
import { buildPath, parsePath } from "./paths.js";

describe("parsePath", () => {
  it("treats / as the tasks view", () => {
    expect(parsePath("/")).toEqual({ view: "tasks" });
  });

  it("parses each sidebar destination", () => {
    for (const view of ["repos", "schedules", "templates", "logs", "sandboxes"]) {
      expect(parsePath(`/${view}`)).toEqual({ view });
    }
  });

  it("parses a task detail path", () => {
    expect(parsePath("/tasks/42")).toEqual({ view: "tasks", taskId: "42" });
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

  it("prefers an open task over the underlying view", () => {
    expect(buildPath({ view: "repos", taskId: "42" })).toBe("/tasks/42");
  });

  it("prefers settings over both a view and an open task", () => {
    expect(buildPath({ view: "repos", taskId: "42", showSettings: true })).toBe("/settings");
  });

  it("round-trips every path parsePath recognizes", () => {
    for (const path of ["/", "/repos", "/schedules", "/templates", "/logs", "/sandboxes", "/tasks/42", "/settings"]) {
      expect(buildPath(parsePath(path))).toBe(path);
    }
  });
});
