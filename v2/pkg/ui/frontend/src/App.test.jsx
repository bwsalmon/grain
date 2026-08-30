import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "./App.jsx";
import api from "./api.js";

vi.mock("./api.js", () => ({ default: vi.fn() }));

const config = { defaultTarget: "acme/widgets", actor: "grain", capabilities: [], rebootEnabled: false };

const initialTasks = [
  { id: "1", title: "Fix bug", state: "queued", repo: "acme/widgets", blocked: false, capabilities: [], reads: [] },
  { id: "2", title: "Add feature", state: "proposed", repo: "acme/other", blocked: false, capabilities: [], reads: [] },
];

// setupApi wires a routing fake covering every endpoint App and the
// overlays it can open touch, backed by a mutable task list so actions
// that mutate (create, approve, ...) are reflected the next time the
// list is refetched -- the same way the real store behaves.
function setupApi(tasks = initialTasks, schedules = []) {
  let tasksState = [...tasks];
  let schedulesState = [...schedules];
  api.mockImplementation((path, opts) => {
    const method = opts?.method || "GET";
    if (path === "/api/config") return Promise.resolve(config);
    if (path === "/api/tasks" && method === "GET") return Promise.resolve(tasksState);
    if (path === "/api/tasks" && method === "POST") {
      const body = JSON.parse(opts.body);
      const newTask = { id: "3", title: body.title, state: "proposed", repo: body.repo || "", blocked: false, capabilities: [], reads: [] };
      tasksState = [...tasksState, newTask];
      return Promise.resolve(newTask);
    }
    const detailMatch = path.match(/^\/api\/tasks\/(\w+)$/);
    if (detailMatch) {
      const t = tasksState.find((t) => t.id === detailMatch[1]);
      return Promise.resolve({ description: "", comments: [], capabilities: [], ...t });
    }
    if (/^\/api\/tasks\/\w+\/(approve|submit|retry|close|reopen)$/.test(path)) return Promise.resolve({});
    if (path === "/api/secrets") return Promise.resolve({ enabled: false });
    if (path === "/api/settings") return Promise.resolve({ configured: false });
    if (/^\/api\/repos\/[^/]+\/[^/]+\/release-config$/.test(path)) {
      return Promise.resolve({ configured: false, prodBranch: "", rcBranch: "", releaseBranchPrefix: "", majorVersion: 0 });
    }
    if (/^\/api\/repos\/[^/]+\/[^/]+\/candidates$/.test(path)) return Promise.resolve([]);
    if (path === "/api/schedules" && method === "GET") return Promise.resolve(schedulesState);
    if (path === "/api/schedules" && method === "POST") {
      const body = JSON.parse(opts.body);
      const newSchedule = { id: "sched-2", enabled: true, nextRunAt: "2026-08-30T00:00:00Z", ...body };
      schedulesState = [...schedulesState, newSchedule];
      return Promise.resolve(newSchedule);
    }
    const scheduleMatch = path.match(/^\/api\/schedules\/([\w-]+)$/);
    if (scheduleMatch && method === "PATCH") {
      const body = JSON.parse(opts.body);
      schedulesState = schedulesState.map((s) => (s.id === scheduleMatch[1] ? { ...s, ...body } : s));
      return Promise.resolve(schedulesState.find((s) => s.id === scheduleMatch[1]));
    }
    if (scheduleMatch && method === "DELETE") {
      schedulesState = schedulesState.filter((s) => s.id !== scheduleMatch[1]);
      return Promise.resolve(null);
    }
    if (path === "/api/upgrade") return Promise.resolve({ enabled: false });
    if (path === "/api/logs") return Promise.resolve({ enabled: false });
    return Promise.resolve(null);
  });
  return { get tasksState() { return tasksState; }, get schedulesState() { return schedulesState; } };
}

describe("App", () => {
  afterEach(() => {
    api.mockReset();
    vi.useRealTimers();
  });

  it("loads config and the task list on mount", async () => {
    setupApi();
    render(<App />);

    expect(await screen.findByText("Fix bug")).toBeInTheDocument();
    expect(screen.getByText("Add feature")).toBeInTheDocument();
    expect(api).toHaveBeenCalledWith("/api/config");
    expect(api).toHaveBeenCalledWith("/api/tasks");
  });

  it("opens a task's detail overlay on click and closes it", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByText("Fix bug"));

    expect(await screen.findByText("1 Fix bug")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Close dialog" }));

    expect(screen.queryByText("1 Fix bug")).not.toBeInTheDocument();
  });

  it("creates a new task and shows it in the refreshed list", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: "+ New task" }));
    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(await screen.findByText("Ship it")).toBeInTheDocument();
  });

  it("shows an error banner when the initial load fails, and clears it after 5s", async () => {
    setupApi();
    api.mockRejectedValueOnce(new Error("config unavailable"));
    render(<App />);

    expect(await screen.findByText("config unavailable")).toBeInTheDocument();

    await waitFor(() => {
      expect(screen.queryByText("config unavailable")).not.toBeInTheDocument();
    }, { timeout: 6000 });
  }, 8000);

  it("selects a task, runs a batch approve, and clears the selection on success", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    const checkboxes = screen.getAllByRole("checkbox");
    await user.click(checkboxes[1]);

    expect(screen.getByText("1 selected")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Approve" }));

    expect(await screen.findByText("Fix bug")).toBeInTheDocument();
    expect(api).toHaveBeenCalledWith("/api/tasks/1/approve", { method: "POST" });
    expect(screen.queryByText(/selected/)).not.toBeInTheDocument();
  });

  it("switches to the repo view, scopes the task list from a repo click, and clears the scope", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Repos/ }));
    expect(await screen.findByText("acme/other")).toBeInTheDocument();

    await user.click(screen.getByText("acme/other"));

    expect(await screen.findByText("Add feature")).toBeInTheDocument();
    expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
    expect(screen.getByText(/Repo: acme\/other/)).toBeInTheDocument();

    await user.click(screen.getByTitle("Clear repo filter"));

    expect(await screen.findByText("Fix bug")).toBeInTheDocument();
    expect(screen.queryByText(/Repo: acme\/other/)).not.toBeInTheDocument();
  });

  it("opens a repo's release pane from the repo view and back out of it", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Repos/ }));
    const row = (await screen.findByText("acme/other")).closest("li");
    await user.click(within(row).getByRole("button", { name: "Releases" }));

    expect(await screen.findByRole("heading", { name: "acme/other releases" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Releases" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /^Repos/ }));

    expect(await screen.findByText("acme/other")).toBeInTheDocument();
  });

  it("switches to the schedules pane, showing its own list and count in the sidebar", async () => {
    const schedule = { id: "sched-1", title: "Nightly dependency bump", description: "", repo: "acme/widgets", base: "", autoMerge: false, recurrence: { kind: "everyNHours", everyNHours: 24 }, enabled: true, nextRunAt: "2026-08-29T00:00:00Z" };
    setupApi(initialTasks, [schedule]);
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Scheduled tasks/ }));

    expect(await screen.findByRole("heading", { name: "Scheduled tasks" })).toBeInTheDocument();
    expect(screen.getByText("Nightly dependency bump")).toBeInTheDocument();
    expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
  });

  it("edits a schedule from the schedules pane", async () => {
    const schedule = { id: "sched-1", title: "Nightly dependency bump", description: "", repo: "acme/widgets", base: "", autoMerge: false, recurrence: { kind: "everyNHours", everyNHours: 24 }, enabled: true, nextRunAt: "2026-08-29T00:00:00Z" };
    setupApi(initialTasks, [schedule]);
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Scheduled tasks/ }));
    await screen.findByText("Nightly dependency bump");

    await user.click(screen.getByRole("button", { name: "Edit" }));
    const titleField = screen.getAllByLabelText(/Title/)[0];
    await user.clear(titleField);
    await user.type(titleField, "Weekly dependency bump");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Weekly dependency bump")).toBeInTheDocument();
  });

  it.each([
    ["Secrets", "Secrets"],
    ["Settings", "Settings"],
    ["Upgrade", "Upgrade"],
  ])("opens the %s overlay from the sidebar", async (button, heading) => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: button }));

    expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
  });

  it("switches to the logs page, hiding the task list", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: "Logs" }));

    expect(await screen.findByRole("heading", { name: "Logs" })).toBeInTheDocument();
    expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
  });

  it("polls the task list on an interval", async () => {
    setupApi();
    render(<App />);
    await screen.findByText("Fix bug");

    const callsBefore = api.mock.calls.filter((c) => c[0] === "/api/tasks").length;

    await waitFor(() => {
      const callsAfter = api.mock.calls.filter((c) => c[0] === "/api/tasks").length;
      expect(callsAfter).toBeGreaterThan(callsBefore);
    }, { timeout: 4000 });
  }, 6000);
});
