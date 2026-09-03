import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

// metricsReport is the shape GET /api/metrics returns (pkg/ui's own
// MetricsReport), trimmed to what the Metrics panel of the Debug
// overlay reads. Its oldest queued task is "Fix bug", the one task in
// initialTasks that is actually queued, so the link out of the backlog
// has somewhere real to land.
const metricsReport = {
  since: "2026-08-27T00:00:00Z",
  until: "2026-09-03T00:00:00Z",
  windowSeconds: 604800,
  throughput: {
    tasksFiled: 2, tasksCompleted: 1, tasksClosed: 0, runsStarted: 2, runsFinished: 1,
    filedPerDay: 0.3, completedPerDay: 0.1, runsFinishedPerDay: 0.1, buckets: [],
  },
  latency: [{
    stage: "lead_time", label: "filed -> completed", description: "the whole of what whoever filed the task waited",
    n: 1, minSeconds: 60, p50Seconds: 60, p90Seconds: 60, p99Seconds: 60, maxSeconds: 60, meanSeconds: 60,
  }],
  runs: { outcomes: { succeeded: 1 }, attemptsPerCompletion: 1, meanConcurrent: 0.1, maxConcurrent: 3, utilization: 0.03, live: 0 },
  backlog: { byState: { queued: 1, proposed: 1 }, queued: 1, oldestQueuedSeconds: 3600, oldestQueuedTaskId: "1" },
};

// setupApi wires a routing fake covering every endpoint App and the
// overlays it can open touch, backed by a mutable task list so actions
// that mutate (create, approve, ...) are reflected the next time the
// list is refetched -- the same way the real store behaves.
function setupApi(tasks = initialTasks, schedules = [], templates = []) {
  let tasksState = [...tasks];
  let schedulesState = [...schedules];
  let templatesState = [...templates];
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
    if (path === "/api/tasks/reorder" && method === "POST") {
      const { ids, afterId, beforeId } = JSON.parse(opts.body);
      const rest = tasksState.filter((t) => !ids.includes(t.id));
      const moved = ids.map((id) => tasksState.find((t) => t.id === id));
      const idx = afterId ? rest.findIndex((t) => t.id === afterId) + 1 : 0;
      tasksState = [...rest.slice(0, idx), ...moved, ...rest.slice(idx)];
      return Promise.resolve(tasksState);
    }
    const detailMatch = path.match(/^\/api\/tasks\/(\w+)$/);
    if (detailMatch) {
      const t = tasksState.find((t) => t.id === detailMatch[1]);
      return Promise.resolve({ description: "", comments: [], capabilities: [], ...t });
    }
    if (/^\/api\/tasks\/\w+\/(approve|submit|retry|close|reopen)$/.test(path)) return Promise.resolve({});
    if (path === "/api/secrets") return Promise.resolve({ enabled: false });
    if (path === "/api/settings") return Promise.resolve({ configured: false });
    if (/^\/api\/repos\/[^/]+\/[^/]+\/releases$/.test(path)) return Promise.resolve([]);
    if (/^\/api\/repos\/[^/]+\/[^/]+\/releases\/[^/]+\/candidates$/.test(path)) return Promise.resolve([]);
    if (/^\/api\/repos\/[^/]+\/[^/]+\/qualification-plan$/.test(path)) {
      return Promise.resolve({ configured: false, repo: "", requireApproval: false, autoPromote: false, items: [] });
    }
    if (/^\/api\/repos\/[^/]+\/[^/]+\/candidates\/[^/]+\/qualification$/.test(path)) return Promise.resolve(null);
    if (path === "/api/schedules" && method === "GET") return Promise.resolve(schedulesState);
    if (path === "/api/schedules" && method === "POST") {
      const body = JSON.parse(opts.body);
      // A templateId, given, drives title/description/etc the same way
      // ui.Client.CreateSchedule does server-side (schedules.go's own
      // scheduleContentFromTemplate) -- the fake needs to reproduce that
      // so a test creating a template-backed schedule sees the same
      // content a real server would hand back. Repo and base are never
      // among them (a template carries no target of its own), so they
      // always come from body, template or not.
      const fromTemplate = body.templateId ? templatesState.find((t) => t.id === body.templateId) : null;
      const newSchedule = {
        id: "sched-2", enabled: true, nextRunAt: "2026-08-30T00:00:00Z",
        ...(fromTemplate && {
          title: fromTemplate.title, description: fromTemplate.description,
          autoMerge: fromTemplate.autoMerge, capabilities: fromTemplate.capabilities,
          templateId: fromTemplate.id, templateName: fromTemplate.name,
        }),
        ...body,
      };
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
    if (path === "/api/templates" && method === "GET") return Promise.resolve(templatesState);
    if (path === "/api/templates" && method === "POST") {
      const body = JSON.parse(opts.body);
      const newTemplate = { id: "template-2", capabilities: [], ...body };
      templatesState = [...templatesState, newTemplate];
      return Promise.resolve(newTemplate);
    }
    const templateMatch = path.match(/^\/api\/templates\/([\w-]+)$/);
    if (templateMatch && method === "PATCH") {
      const body = JSON.parse(opts.body);
      templatesState = templatesState.map((t) => (t.id === templateMatch[1] ? { ...t, ...body } : t));
      return Promise.resolve(templatesState.find((t) => t.id === templateMatch[1]));
    }
    if (templateMatch && method === "DELETE") {
      templatesState = templatesState.filter((t) => t.id !== templateMatch[1]);
      return Promise.resolve(null);
    }
    // Task suites (bwsalmon/agents#642). Both endpoints are fetched on
    // mount, and ui.Client.ListSuites/ListSuiteRuns both build their
    // result with make([]T, 0, ...), so a deployment with no suites gets
    // [] rather than null -- mirror that here. Falling through to this
    // fake's own null default instead would set App's suites state to
    // null, and Sidebar's `suites = []` parameter default only covers
    // undefined, so `suites.length` would take the whole app down.
    if (path === "/api/suites" && method === "GET") return Promise.resolve([]);
    if (path === "/api/suite-runs" && method === "GET") return Promise.resolve([]);
    if (path === "/api/upgrade") return Promise.resolve({ enabled: false });
    if (path === "/api/logs") return Promise.resolve({ enabled: false });
    if (path === "/api/sandboxes") return Promise.resolve({ enabled: false });
    if (path.startsWith("/api/metrics")) return Promise.resolve(metricsReport);
    return Promise.resolve(null);
  });
  return {
    get tasksState() { return tasksState; },
    get schedulesState() { return schedulesState; },
    get templatesState() { return templatesState; },
  };
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

  it("shows no reconciler-down banner by default", async () => {
    setupApi();
    render(<App />);

    await screen.findByText("Fix bug");
    expect(screen.queryByText(/reconcile loop has stopped/i)).not.toBeInTheDocument();
  });

  it("shows a banner when the deployment's reconciler has died (bwsalmon/agents#576)", async () => {
    setupApi();
    const realImpl = api.getMockImplementation();
    api.mockImplementation((path, opts) =>
      (path === "/api/config" ? Promise.resolve({ ...config, reconcilerDown: true }) : realImpl(path, opts)));

    render(<App />);

    expect(await screen.findByText(/reconcile loop has stopped/i)).toBeInTheDocument();
  });

  it("shows a loading screen with the large mark until config loads (bwsalmon/agents#555)", async () => {
    setupApi();
    let resolveConfig;
    const configPromise = new Promise((resolve) => { resolveConfig = resolve; });
    const realImpl = api.getMockImplementation();
    api.mockImplementation((path, opts) => (path === "/api/config" ? configPromise : realImpl(path, opts)));

    render(<App />);

    expect(screen.getByTitle("grain")).toHaveAttribute("width", "320");
    expect(screen.queryByRole("button", { name: "+ New task" })).not.toBeInTheDocument();

    resolveConfig(config);

    await screen.findByText("Fix bug");
    // 32: the sidebar's mark, the smallest size the v2 glyphs still read
    // as shapes at (Sidebar.jsx).
    expect(screen.getByTitle("grain")).toHaveAttribute("width", "32");
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
    await user.click(screen.getByLabelText(/No repo/));
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

  it("drags a task onto another and reorders the list via the API (bwsalmon/agents#476)", async () => {
    setupApi();
    render(<App />);
    await screen.findByText("Fix bug");

    const titles = () => [...document.querySelectorAll(".task-title")].map((el) => el.textContent);
    expect(titles()).toEqual(["Fix bug", "Add feature"]);

    fireEvent.dragStart(screen.getByText("Add feature").closest("li"));
    fireEvent.dragOver(screen.getByText("Fix bug").closest("li"));
    fireEvent.drop(screen.getByText("Fix bug").closest("li"));

    expect(api).toHaveBeenCalledWith("/api/tasks/reorder", {
      method: "POST",
      body: JSON.stringify({ ids: ["2"], afterId: undefined, beforeId: "1" }),
    });
    await waitFor(() => expect(titles()).toEqual(["Add feature", "Fix bug"]));
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

  it("folds a repo's tasks open from the repo view and files a new task against it", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Repos/ }));
    await screen.findByText("acme/other");

    await user.click(screen.getByRole("button", { name: "Show tasks for acme/other" }));
    expect(await screen.findByText("Add feature")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Hide tasks for acme/other" }));
    expect(screen.queryByText("Add feature")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "New task under acme/other" }));
    expect(screen.getByLabelText(/Target repo/)).toHaveValue("acme/other");

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    await user.click(screen.getByRole("button", { name: "Show tasks for acme/other" }));
    expect(await screen.findByText("Ship it")).toBeInTheDocument();
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
    await user.click(await screen.findByText("Nightly dependency bump"));
    expect(await screen.findByRole("heading", { name: "Edit schedule" })).toBeInTheDocument();

    const titleField = screen.getByLabelText(/Title/);
    await user.clear(titleField);
    await user.type(titleField, "Weekly dependency bump");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Weekly dependency bump")).toBeInTheDocument();
  });

  it("switches to the templates pane, showing its own list and count in the sidebar", async () => {
    const template = { id: "template-1", name: "Dependency bump", title: "Bump dependencies", description: "", autoMerge: false, capabilities: [] };
    setupApi(initialTasks, [], [template]);
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Task templates/ }));

    expect(await screen.findByRole("heading", { name: "Task templates" })).toBeInTheDocument();
    expect(screen.getByText("Dependency bump")).toBeInTheDocument();
    expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
  });

  it("creates a schedule from a template, hiding the content fields", async () => {
    const template = { id: "template-1", name: "Dependency bump", title: "Bump dependencies", description: "", autoMerge: false, capabilities: [] };
    setupApi(initialTasks, [], [template]);
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Scheduled tasks/ }));
    await user.click(await screen.findByRole("button", { name: "+ New schedule" }));
    await screen.findByRole("heading", { name: "New schedule" });

    await user.click(screen.getByLabelText("Template"));
    await user.click(await screen.findByRole("option", { name: "Dependency bump" }));

    expect(screen.queryByLabelText(/^Title/)).not.toBeInTheDocument();
    expect(screen.getByText(/come from the selected template/)).toBeInTheDocument();
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");

    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(await screen.findByText("Bump dependencies")).toBeInTheDocument();
  });

  it.each([
    ["Settings", "Settings"],
    ["Debugging", "Debug"],
  ])("opens the %s overlay from the sidebar", async (button, heading) => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: button }));

    expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
  });

  it("opens Secrets and Upgrade as tabs inside Settings rather than their own sidebar entries", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    expect(screen.queryByRole("button", { name: "Secrets" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Upgrade" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Settings" }));
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();

    await user.click(screen.getByRole("tab", { name: "Secrets" }));
    expect(await screen.findByText(/this UI was not started with a local secrets directory/i)).toBeInTheDocument();

    await user.click(screen.getByRole("tab", { name: "Upgrade" }));
    expect(await screen.findByText(/no -upgrade-src-dir configured/i)).toBeInTheDocument();
  });

  // bwsalmon/agents#640: Logs and Sandbox health live together on their
  // own "Debugging" sidebar entry, not inside Settings.
  it("shows Logs and Sandbox health on the Debugging overlay, not as their own sidebar entries", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    expect(screen.queryByRole("button", { name: "Logs" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sandbox health" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Debugging" }));

    expect(await screen.findByText(/no log sources configured/i)).toBeInTheDocument();

    // Sandbox health is a tab of that overlay, not a second pane rendered
    // beside Logs (bwsalmon/agents#640 split them) -- only the active
    // tab's panel is mounted, so reaching it means clicking it, the same
    // way DebugOverlay.test.jsx's own "shows Sandbox health on its own
    // tab" does.
    await user.click(screen.getByRole("tab", { name: "Sandbox health" }));

    expect(await screen.findByText(/no sandbox pool or host stats configured/i)).toBeInTheDocument();
  });

  // The metrics report's backlog names the oldest queued task, and the
  // useful thing to do with that is go and look at it. Two stacked
  // dialogs would put the task behind the pane the click came from, so
  // App closes the Debug overlay on the way through.
  it("opens the oldest queued task from the Metrics panel, leaving the Debug overlay behind", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: "Debugging" }));
    await user.click(await screen.findByRole("tab", { name: "Metrics" }));
    await user.click(await screen.findByRole("button", { name: "task 1" }));

    expect(await screen.findByText("1 Fix bug")).toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Metrics" })).not.toBeInTheDocument();
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
