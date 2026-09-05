import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "./App.jsx";
import api from "./api.js";

vi.mock("./api.js", () => ({ default: vi.fn() }));

const config = {
  defaultTarget: "acme/widgets",
  actor: "grain",
  capabilities: [],
  rebootEnabled: false,
};

const initialTasks = [
  {
    id: "1",
    title: "Fix bug",
    state: "queued",
    repo: "acme/widgets",
    blocked: false,
    capabilities: [],
    reads: [],
  },
  {
    id: "2",
    title: "Add feature",
    state: "proposed",
    repo: "acme/other",
    blocked: false,
    capabilities: [],
    reads: [],
  },
];

// metricsReport is the shape GET /api/metrics returns (pkg/ui's own
// MetricsReport), trimmed to what the Metrics pane reads. Its oldest
// queued task is "Fix bug", the one task in
// initialTasks that is actually queued, so the link out of the backlog
// has somewhere real to land.
const metricsReport = {
  since: "2026-08-27T00:00:00Z",
  until: "2026-09-03T00:00:00Z",
  windowSeconds: 604800,
  throughput: {
    tasksFiled: 2,
    tasksCompleted: 1,
    tasksClosed: 0,
    runsStarted: 2,
    runsFinished: 1,
    filedPerDay: 0.3,
    completedPerDay: 0.1,
    runsFinishedPerDay: 0.1,
    buckets: [],
  },
  latency: [
    {
      stage: "lead_time",
      label: "filed -> completed",
      description: "the whole of what whoever filed the task waited",
      n: 1,
      minSeconds: 60,
      p50Seconds: 60,
      p90Seconds: 60,
      p99Seconds: 60,
      maxSeconds: 60,
      meanSeconds: 60,
    },
  ],
  runs: {
    outcomes: { succeeded: 1 },
    attemptsPerCompletion: 1,
    meanConcurrent: 0.1,
    maxConcurrent: 3,
    utilization: 0.03,
    live: 0,
  },
  backlog: {
    byState: { queued: 1, proposed: 1 },
    queued: 1,
    oldestQueuedSeconds: 3600,
    oldestQueuedTaskId: "1",
  },
};

// setupApi wires a routing fake covering every endpoint App and the
// overlays it can open touch, backed by a mutable task list so actions
// that mutate (create, approve, ...) are reflected the next time the
// list is refetched -- the same way the real store behaves.
function setupApi(
  tasks = initialTasks,
  schedules = [],
  templates = [],
  suites = [],
) {
  let tasksState = [...tasks];
  let schedulesState = [...schedules];
  let templatesState = [...templates];
  const suitesState = [...suites];
  api.mockImplementation((path, opts) => {
    const method = opts?.method || "GET";
    if (path === "/api/config") return Promise.resolve(config);
    if (path === "/api/tasks" && method === "GET")
      return Promise.resolve(tasksState);
    if (path === "/api/tasks" && method === "POST") {
      const body = JSON.parse(opts.body);
      const newTask = {
        id: "3",
        title: body.title,
        state: "proposed",
        repo: body.repo || "",
        blocked: false,
        capabilities: [],
        reads: [],
      };
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
      return Promise.resolve({
        description: "",
        comments: [],
        capabilities: [],
        ...t,
      });
    }
    if (/^\/api\/tasks\/\w+\/(approve|submit|retry|close|reopen)$/.test(path))
      return Promise.resolve({});
    if (path === "/api/secrets") return Promise.resolve({ enabled: false });
    if (path === "/api/settings") return Promise.resolve({ configured: false });
    // A repo's own page (RepoPage, grain/task-111) reads both of these
    // on landing.
    if (/^\/api\/repos\/[^/]+\/[^/]+\/branches$/.test(path) && method === "GET")
      return Promise.resolve([]);
    if (
      /^\/api\/repos\/[^/]+\/[^/]+\/capabilities$/.test(path) &&
      method === "GET"
    ) {
      return Promise.resolve({
        repo: "",
        defaultCapabilities: [],
        deploymentDefaultCapabilities: [],
        effectiveDefaultCapabilities: [],
      });
    }
    if (/^\/api\/repos\/[^/]+\/[^/]+\/releases$/.test(path))
      return Promise.resolve([]);
    if (/^\/api\/repos\/[^/]+\/[^/]+\/releases\/[^/]+\/candidates$/.test(path))
      return Promise.resolve([]);
    if (/^\/api\/repos\/[^/]+\/[^/]+\/qualification-plan$/.test(path)) {
      return Promise.resolve({
        configured: false,
        repo: "",
        requireApproval: false,
        autoPromote: false,
        items: [],
      });
    }
    if (
      /^\/api\/repos\/[^/]+\/[^/]+\/candidates\/[^/]+\/qualification$/.test(
        path,
      )
    )
      return Promise.resolve(null);
    if (path === "/api/schedules" && method === "GET")
      return Promise.resolve(schedulesState);
    if (path === "/api/schedules" && method === "POST") {
      const body = JSON.parse(opts.body);
      // A templateId, given, drives title/description/etc the same way
      // ui.Client.CreateSchedule does server-side (schedules.go's own
      // scheduleContentFromTemplate) -- the fake needs to reproduce that
      // so a test creating a template-backed schedule sees the same
      // content a real server would hand back. Repo and base are never
      // among them (a template carries no target of its own), so they
      // always come from body, template or not.
      const fromTemplate = body.templateId
        ? templatesState.find((t) => t.id === body.templateId)
        : null;
      const newSchedule = {
        id: "sched-2",
        enabled: true,
        nextRunAt: "2026-08-30T00:00:00Z",
        ...(fromTemplate && {
          title: fromTemplate.title,
          description: fromTemplate.description,
          autoMerge: fromTemplate.autoMerge,
          capabilities: fromTemplate.capabilities,
          templateId: fromTemplate.id,
          templateName: fromTemplate.name,
        }),
        ...body,
      };
      schedulesState = [...schedulesState, newSchedule];
      return Promise.resolve(newSchedule);
    }
    const scheduleMatch = path.match(/^\/api\/schedules\/([\w-]+)$/);
    if (scheduleMatch && method === "PATCH") {
      const body = JSON.parse(opts.body);
      schedulesState = schedulesState.map((s) =>
        s.id === scheduleMatch[1] ? { ...s, ...body } : s,
      );
      return Promise.resolve(
        schedulesState.find((s) => s.id === scheduleMatch[1]),
      );
    }
    if (scheduleMatch && method === "DELETE") {
      schedulesState = schedulesState.filter((s) => s.id !== scheduleMatch[1]);
      return Promise.resolve(null);
    }
    if (path === "/api/templates" && method === "GET")
      return Promise.resolve(templatesState);
    if (path === "/api/templates" && method === "POST") {
      const body = JSON.parse(opts.body);
      const newTemplate = { id: "template-2", capabilities: [], ...body };
      templatesState = [...templatesState, newTemplate];
      return Promise.resolve(newTemplate);
    }
    const templateMatch = path.match(/^\/api\/templates\/([\w-]+)$/);
    if (templateMatch && method === "PATCH") {
      const body = JSON.parse(opts.body);
      templatesState = templatesState.map((t) =>
        t.id === templateMatch[1] ? { ...t, ...body } : t,
      );
      return Promise.resolve(
        templatesState.find((t) => t.id === templateMatch[1]),
      );
    }
    if (templateMatch && method === "DELETE") {
      templatesState = templatesState.filter((t) => t.id !== templateMatch[1]);
      return Promise.resolve(null);
    }
    // Suites (bwsalmon/agents#642). Both endpoints are fetched on
    // mount, and ui.Client.ListSuites/ListSuiteRuns both build their
    // result with make([]T, 0, ...), so a deployment with no suites gets
    // [] rather than null -- mirror that here. Falling through to this
    // fake's own null default instead would set App's suites state to
    // null, and Sidebar's `suites = []` parameter default only covers
    // undefined, so `suites.length` would take the whole app down.
    if (path === "/api/suites" && method === "GET")
      return Promise.resolve(suitesState);
    if (path === "/api/suite-runs" && method === "GET")
      return Promise.resolve([]);
    if (path === "/api/upgrade") return Promise.resolve({ enabled: false });
    if (path === "/api/logs") return Promise.resolve({ enabled: false });
    if (path === "/api/sandboxes") return Promise.resolve({ enabled: false });
    if (path.startsWith("/api/metrics")) return Promise.resolve(metricsReport);
    return Promise.resolve(null);
  });
  return {
    get tasksState() {
      return tasksState;
    },
    get schedulesState() {
      return schedulesState;
    },
    get templatesState() {
      return templatesState;
    },
  };
}

describe("App", () => {
  afterEach(() => {
    api.mockReset();
    vi.useRealTimers();
    // A repo page's List/Board switch is stored per browser (board.js),
    // so one test's choice would otherwise be the next test's view.
    localStorage.clear();
    // The address bar outlives a test's own render -- and now carries a
    // list's narrowing (grain/task-317) -- so put it back to "/" rather
    // than let one test's URL seed the next test's App.
    window.history.replaceState(null, "", "/");
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
    expect(
      screen.queryByText(/reconcile loop has stopped/i),
    ).not.toBeInTheDocument();
  });

  it("shows a banner when the deployment's reconciler has died (bwsalmon/agents#576)", async () => {
    setupApi();
    const realImpl = api.getMockImplementation();
    api.mockImplementation((path, opts) =>
      path === "/api/config"
        ? Promise.resolve({ ...config, reconcilerDown: true })
        : realImpl(path, opts),
    );

    render(<App />);

    expect(
      await screen.findByText(/reconcile loop has stopped/i),
    ).toBeInTheDocument();
  });

  // grain/task-132: an operator opening grain during a provider's
  // usage-limit window used to see a queue of ready tasks and nothing
  // running, with no explanation on screen at all.
  it("shows a banner while the agent's usage limit has dispatch paused", async () => {
    setupApi();
    const realImpl = api.getMockImplementation();
    const agentPause = {
      paused: true,
      until: "2026-09-03T17:00:00Z",
      reason: "claude: usage limit reached; resets at 2026-09-03T17:00:00Z",
      secondsRemaining: 7200,
    };
    api.mockImplementation((path, opts) =>
      path === "/api/config"
        ? Promise.resolve({ ...config, agentPause })
        : realImpl(path, opts),
    );

    render(<App />);

    expect(
      await screen.findByText(/agent usage limit reached/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/about 2h 0m/)).toBeInTheDocument();
  });

  it("shows no pause banner on a deployment that is dispatching", async () => {
    setupApi();
    render(<App />);

    await screen.findByText("Fix bug");
    expect(
      screen.queryByText(/agent usage limit reached/i),
    ).not.toBeInTheDocument();
  });

  // Both banners are pinned to the same strip, and a dead reconcile loop
  // is the larger fact: being told why a deployment that dispatches
  // nothing at all is not dispatching helps nobody.
  it("shows only the reconciler-down banner when both are true", async () => {
    setupApi();
    const realImpl = api.getMockImplementation();
    api.mockImplementation((path, opts) =>
      path === "/api/config"
        ? Promise.resolve({
            ...config,
            reconcilerDown: true,
            agentPause: {
              paused: true,
              until: "2026-09-03T17:00:00Z",
              secondsRemaining: 7200,
            },
          })
        : realImpl(path, opts),
    );

    render(<App />);

    expect(
      await screen.findByText(/reconcile loop has stopped/i),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/agent usage limit reached/i),
    ).not.toBeInTheDocument();
  });

  // The banner's own "Resume now" -- an operator who has just topped a
  // plan up has no reason to wait out a window that no longer applies.
  it("lifts the pause and re-reads the config when Resume now is clicked", async () => {
    setupApi();
    const realImpl = api.getMockImplementation();
    let paused = true;
    api.mockImplementation((path, opts) => {
      if (path === "/api/config") {
        return Promise.resolve(
          paused
            ? {
                ...config,
                agentPause: {
                  paused: true,
                  until: "2026-09-03T17:00:00Z",
                  secondsRemaining: 7200,
                },
              }
            : config,
        );
      }
      if (path === "/api/pause" && opts?.method === "DELETE") {
        paused = false;
        return Promise.resolve({
          enabled: true,
          lifted: true,
          pause: { paused: false },
        });
      }
      return realImpl(path, opts);
    });

    render(<App />);
    await screen.findByText(/agent usage limit reached/i);
    fireEvent.click(screen.getByRole("button", { name: /resume now/i }));

    await waitFor(() =>
      expect(api).toHaveBeenCalledWith("/api/pause", { method: "DELETE" }),
    );
    await waitFor(() =>
      expect(
        screen.queryByText(/agent usage limit reached/i),
      ).not.toBeInTheDocument(),
    );
  });

  // grain/task-69: the deployment's name in the tab strip, which is the
  // one piece of chrome the app cannot draw into. Name first, because a
  // narrow tab truncates its title from the end.
  it("titles the tab with the environment name when one is configured", async () => {
    setupApi();
    const realImpl = api.getMockImplementation();
    api.mockImplementation((path, opts) =>
      path === "/api/config"
        ? Promise.resolve({ ...config, environmentName: "staging" })
        : realImpl(path, opts),
    );

    render(<App />);

    await screen.findByText("Fix bug");
    await waitFor(() => expect(document.title).toBe("staging — grain"));
  });

  // grain/task-368: every absolute time under App is printed on the
  // deployment's own clock, which /api/config carries -- not on the
  // browser's, which is a different clock from the one the daemon fires
  // schedules against.
  it("prints times on the deployment's own clock, not the browser's", async () => {
    const schedule = {
      id: "sched-1",
      title: "Nightly dependency bump",
      description: "",
      repo: "acme/widgets",
      base: "",
      autoMerge: false,
      recurrence: { kind: "daily", timeOfDay: "09:00" },
      enabled: true,
      nextRunAt: "2026-08-29T16:00:00Z",
    };
    setupApi(initialTasks, [schedule]);
    const realImpl = api.getMockImplementation();
    api.mockImplementation((path, opts) =>
      path === "/api/config"
        ? Promise.resolve({ ...config, timeZone: "America/Los_Angeles" })
        : realImpl(path, opts),
    );
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Schedules/ }));
    await screen.findByText("Nightly dependency bump");

    const expected = new Date(schedule.nextRunAt).toLocaleString(undefined, {
      timeZone: "America/Los_Angeles",
    });
    expect(screen.getByText(/Next run/).textContent).toContain(expected);
    // And the schedule form labels its time field with that same zone,
    // since what is typed there is read on it.
    await user.click(screen.getByText("Nightly dependency bump"));
    expect(
      await screen.findByLabelText(/Time \(America\/Los_Angeles/),
    ).toBeInTheDocument();
  });

  it("leaves the tab titled just grain on an unnamed deployment", async () => {
    setupApi();
    render(<App />);

    await screen.findByText("Fix bug");
    await waitFor(() => expect(document.title).toBe("grain"));
  });

  it("shows a loading screen with the large mark until config loads (bwsalmon/agents#555)", async () => {
    setupApi();
    let resolveConfig;
    const configPromise = new Promise((resolve) => {
      resolveConfig = resolve;
    });
    const realImpl = api.getMockImplementation();
    api.mockImplementation((path, opts) =>
      path === "/api/config" ? configPromise : realImpl(path, opts),
    );

    render(<App />);

    expect(screen.getByTitle("grain")).toHaveAttribute("width", "320");
    expect(
      screen.queryByRole("button", { name: "+ New task" }),
    ).not.toBeInTheDocument();

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

    // A task's pane leaves by the back button in its top-left corner,
    // the same as a repo's page does (grain/task-177).
    await user.click(screen.getByRole("button", { name: "← Back" }));

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

  // grain/task-202: filing a task also writes which end of the backlog
  // new work joins, and the new-task form seeds its picker from that
  // (config.newestFirst). The config is otherwise fetched once, at
  // mount, so creating a task has to re-read it -- or the choice just
  // made would not show up until a reload.
  it("re-reads the config after a task is filed, so the backlog-end picker keeps the last choice", async () => {
    setupApi();
    const inner = api.getMockImplementation();
    let newestFirst = false;
    api.mockImplementation((path, opts) => {
      if (path === "/api/config")
        return Promise.resolve({ ...config, newestFirst });
      if (path === "/api/tasks" && opts?.method === "POST") {
        const body = JSON.parse(opts.body);
        if (typeof body.atFront === "boolean") newestFirst = body.atFront;
      }
      return inner(path, opts);
    });
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: "+ New task" }));
    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByLabelText("Add to backlog"));
    await user.click(screen.getByRole("option", { name: /^Front/ }));
    await user.click(screen.getByRole("button", { name: "Create task" }));
    await screen.findByText("Ship it");

    await user.click(screen.getByRole("button", { name: "+ New task" }));
    expect(screen.getByLabelText("Add to backlog")).toHaveTextContent(/^Front/);
  });

  it("shows an error banner when the initial load fails, and clears it after 5s", async () => {
    setupApi();
    api.mockRejectedValueOnce(new Error("config unavailable"));
    render(<App />);

    expect(await screen.findByText("config unavailable")).toBeInTheDocument();

    await waitFor(
      () => {
        expect(
          screen.queryByText("config unavailable"),
        ).not.toBeInTheDocument();
      },
      { timeout: 6000 },
    );
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
    expect(api).toHaveBeenCalledWith("/api/tasks/1/approve", {
      method: "POST",
    });
    expect(screen.queryByText(/selected/)).not.toBeInTheDocument();
  });

  it("drags a task onto another and reorders the list via the API (bwsalmon/agents#476)", async () => {
    setupApi();
    render(<App />);
    await screen.findByText("Fix bug");

    const titles = () =>
      [...document.querySelectorAll(".task-title")].map((el) => el.textContent);
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

  // grain/task-111: a repo row opens that repo's own page, which is
  // where its tasks are listed and where the URL lands.
  it("opens a repo's own page from the repo list, listing only that repo's tasks, and back out of it", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Repos/ }));
    expect(await screen.findByText("acme/other")).toBeInTheDocument();

    await user.click(screen.getByText("acme/other"));

    expect(
      await screen.findByRole("heading", { name: "acme/other" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Add feature")).toBeInTheDocument();
    expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
    expect(window.location.pathname).toBe("/repos/acme/other");

    await user.click(screen.getByRole("button", { name: /Repos$/ }));

    expect(await screen.findByText("acme/widgets")).toBeInTheDocument();
    expect(window.location.pathname).toBe("/repos");
  });

  // grain/task-321: the same board, over one repo's tasks, from that
  // repo's own page -- the switch is the page's, the tasks are the ones
  // its list would have shown.
  it("lays one repo's tasks out as a board from that repo's page", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Repos/ }));
    await user.click(await screen.findByText("acme/other"));
    await screen.findByRole("heading", { name: "acme/other" });

    // The page's own switch, not the rail's Board entry.
    await user.click(
      within(
        screen.getByRole("group", { name: /Show this repo's tasks/ }),
      ).getByRole("button", { name: "Board" }),
    );

    expect(
      screen.getByText("Add feature").closest(".board-column"),
    ).toHaveTextContent("Proposed");
    expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
    // Still the repo's page: the board is how it is showing its tasks,
    // not somewhere else to be.
    expect(
      screen.getByRole("heading", { name: "acme/other" }),
    ).toBeInTheDocument();
    expect(window.location.pathname).toBe("/repos/acme/other");
  });

  it("files a new task against the repo whose page is open", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Repos/ }));
    await user.click(await screen.findByText("acme/other"));
    await screen.findByRole("heading", { name: "acme/other" });

    await user.click(screen.getByRole("button", { name: "New task" }));
    expect(screen.getByLabelText(/Target repo/)).toHaveValue("acme/other");

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(await screen.findByText("Ship it")).toBeInTheDocument();
  });

  it("opens a repo's release pane from its page and back out of it", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Repos/ }));
    await user.click(await screen.findByText("acme/other"));
    await user.click(await screen.findByRole("button", { name: "Releases" }));

    expect(
      await screen.findByRole("heading", { name: "acme/other releases" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Releases" }),
    ).not.toBeInTheDocument();
    expect(window.location.pathname).toBe("/repos/acme/other/releases");

    await user.click(screen.getByRole("button", { name: /acme\/other$/ }));

    expect(
      await screen.findByRole("heading", { name: "acme/other" }),
    ).toBeInTheDocument();
    expect(window.location.pathname).toBe("/repos/acme/other");
  });

  // grain/task-287: the board is the same tasks as the list, in columns
  // -- so it is reached from the rail, has a URL of its own, and feeds
  // the same selection the batch-actions bar acts on.
  it("switches to the board, laying the same tasks out in columns", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: "Board" }));

    expect(
      await screen.findByRole("heading", { name: "Board" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Fix bug").closest(".board-column"),
    ).toHaveTextContent("Queued");
    expect(
      screen.getByText("Add feature").closest(".board-column"),
    ).toHaveTextContent("Proposed");
    expect(window.location.pathname).toBe("/board");
  });

  it("opens the board on a fresh load of /board", async () => {
    window.history.replaceState(null, "", "/board");
    setupApi();
    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Board" }),
    ).toBeInTheDocument();
    expect(window.location.pathname).toBe("/board");
  });

  it("selects a task from a board card and runs a batch action on it", async () => {
    window.history.replaceState(null, "", "/board");
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByRole("heading", { name: "Board" });

    await user.click(screen.getByRole("checkbox", { name: "Select 2" }));
    await user.click(screen.getByRole("button", { name: "Approve" }));

    await waitFor(() =>
      expect(api).toHaveBeenCalledWith("/api/tasks/2/approve", {
        method: "POST",
      }),
    );
  });

  it("switches to the schedules pane, showing its own list and count in the sidebar", async () => {
    const schedule = {
      id: "sched-1",
      title: "Nightly dependency bump",
      description: "",
      repo: "acme/widgets",
      base: "",
      autoMerge: false,
      recurrence: { kind: "everyNHours", everyNHours: 24 },
      enabled: true,
      nextRunAt: "2026-08-29T00:00:00Z",
    };
    setupApi(initialTasks, [schedule]);
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Schedules/ }));

    expect(
      await screen.findByRole("heading", { name: "Schedules" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Nightly dependency bump")).toBeInTheDocument();
    expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
  });

  it("edits a schedule from the schedules pane", async () => {
    const schedule = {
      id: "sched-1",
      title: "Nightly dependency bump",
      description: "",
      repo: "acme/widgets",
      base: "",
      autoMerge: false,
      recurrence: { kind: "everyNHours", everyNHours: 24 },
      enabled: true,
      nextRunAt: "2026-08-29T00:00:00Z",
    };
    setupApi(initialTasks, [schedule]);
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Schedules/ }));
    await user.click(await screen.findByText("Nightly dependency bump"));
    expect(
      await screen.findByRole("heading", { name: "Edit schedule" }),
    ).toBeInTheDocument();

    const titleField = screen.getByLabelText(/Title/);
    await user.clear(titleField);
    await user.type(titleField, "Weekly dependency bump");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(
      await screen.findByText("Weekly dependency bump"),
    ).toBeInTheDocument();
  });

  // grain/task-139: a schedule, a template and a suite are addressable
  // in their own right, the way a task already was -- which one is open
  // lives here in App rather than inside the list component, so
  // paths.js can name it. The URL going stale (an item deleted, or a
  // link to one that never existed) is App's to correct, since App is
  // what holds the list the id has to be in.
  it("gives an open schedule its own URL, and clears it again on close", async () => {
    const schedule = {
      id: "sched-1",
      title: "Nightly dependency bump",
      description: "",
      repo: "acme/widgets",
      base: "",
      autoMerge: false,
      recurrence: { kind: "everyNHours", everyNHours: 24 },
      enabled: true,
      nextRunAt: "2026-08-29T00:00:00Z",
    };
    setupApi(initialTasks, [schedule]);
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Schedules/ }));
    await user.click(await screen.findByText("Nightly dependency bump"));

    expect(
      await screen.findByRole("heading", { name: "Edit schedule" }),
    ).toBeInTheDocument();
    expect(window.location.pathname).toBe("/schedules/sched-1");

    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(
      screen.queryByRole("heading", { name: "Edit schedule" }),
    ).not.toBeInTheDocument();
    expect(window.location.pathname).toBe("/schedules");
  });

  it("opens the schedule /schedules/:id names on a fresh load", async () => {
    const schedule = {
      id: "sched-1",
      title: "Nightly dependency bump",
      description: "",
      repo: "acme/widgets",
      base: "",
      autoMerge: false,
      recurrence: { kind: "everyNHours", everyNHours: 24 },
      enabled: true,
      nextRunAt: "2026-08-29T00:00:00Z",
    };
    window.history.replaceState(null, "", "/schedules/sched-1");
    setupApi(initialTasks, [schedule]);
    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Edit schedule" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText(/Title/)).toHaveValue(
      "Nightly dependency bump",
    );
    expect(window.location.pathname).toBe("/schedules/sched-1");
  });

  it("opens the template /templates/:id names on a fresh load", async () => {
    const template = {
      id: "template-1",
      name: "Dependency bump",
      title: "Bump dependencies",
      description: "",
      autoMerge: false,
      capabilities: [],
    };
    window.history.replaceState(null, "", "/templates/template-1");
    setupApi(initialTasks, [], [template]);
    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Edit template" }),
    ).toBeInTheDocument();
    expect(window.location.pathname).toBe("/templates/template-1");
  });

  it("opens the suite /suites/:id names on a fresh load", async () => {
    const suite = {
      id: "suite-1",
      name: "Nightly sweep",
      items: [],
      mode: "until_clean",
      maxPasses: 5,
      requireApproval: false,
      autoMerge: true,
    };
    window.history.replaceState(null, "", "/suites/suite-1");
    setupApi(initialTasks, [], [], [suite]);
    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Edit suite" }),
    ).toBeInTheDocument();
    expect(window.location.pathname).toBe("/suites/suite-1");
  });

  it("falls back to the plain list when the URL names a schedule that isn't there", async () => {
    window.history.replaceState(null, "", "/schedules/gone");
    setupApi(initialTasks, []);
    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Schedules" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Edit schedule" }),
    ).not.toBeInTheDocument();
    // The stale id is dropped once the list it named has landed, so the
    // address bar stops pointing at a pane that isn't open.
    await waitFor(() => expect(window.location.pathname).toBe("/schedules"));
  });

  it("switches to the templates pane, showing its own list and count in the sidebar", async () => {
    const template = {
      id: "template-1",
      name: "Dependency bump",
      title: "Bump dependencies",
      description: "",
      autoMerge: false,
      capabilities: [],
    };
    setupApi(initialTasks, [], [template]);
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Templates/ }));

    expect(
      await screen.findByRole("heading", { name: "Templates" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Dependency bump")).toBeInTheDocument();
    expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
  });

  it("creates a schedule from a template, hiding the content fields", async () => {
    const template = {
      id: "template-1",
      name: "Dependency bump",
      title: "Bump dependencies",
      description: "",
      autoMerge: false,
      capabilities: [],
    };
    setupApi(initialTasks, [], [template]);
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: /^Schedules/ }));
    await user.click(
      await screen.findByRole("button", { name: "+ New schedule" }),
    );
    await screen.findByRole("heading", { name: "New schedule" });

    await user.click(screen.getByLabelText("Template"));
    await user.click(
      await screen.findByRole("option", { name: "Dependency bump" }),
    );

    expect(screen.queryByLabelText(/^Title/)).not.toBeInTheDocument();
    expect(
      screen.getByText(/come from the selected template/),
    ).toBeInTheDocument();
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");

    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(await screen.findByText("Bump dependencies")).toBeInTheDocument();
  });

  it.each([
    ["Settings", "Settings"],
    ["System", "System"],
    ["Metrics", "Metrics"],
  ])("opens the %s overlay from the sidebar", async (button, heading) => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: button }));

    expect(
      await screen.findByRole("heading", { name: heading }),
    ).toBeInTheDocument();
  });

  // Upgrade is a tab inside Settings rather than a sidebar entry
  // (bwsalmon/agents#456). Secrets was one too until grain/task-110 gave
  // each secret to whatever uses it: the agent credentials to Agents, a
  // capability's own to the row beside it on Capabilities, and the
  // remainder to "Other secrets" at the foot of that tab.
  it("opens Upgrade as a tab inside Settings, and keeps secrets with what uses them", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    expect(
      screen.queryByRole("button", { name: "Secrets" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Upgrade" }),
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Settings" }));
    expect(
      await screen.findByRole("heading", { name: "Settings" }),
    ).toBeInTheDocument();

    expect(
      screen.queryByRole("tab", { name: "Secrets" }),
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole("tab", { name: "Capabilities" }));
    expect(await screen.findByText("Other secrets")).toBeInTheDocument();
    expect(
      screen.getByText(
        /this UI was not started with a local secrets directory/i,
      ),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("tab", { name: "Upgrade" }));
    expect(
      await screen.findByText(/no -upgrade-src-dir configured/i),
    ).toBeInTheDocument();
  });

  // bwsalmon/agents#640: Logs and Sandbox health live together on their
  // own "System" sidebar entry, not inside Settings.
  it("shows Logs and Sandbox health on the System overlay, not as their own sidebar entries", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    expect(
      screen.queryByRole("button", { name: "Logs" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Sandbox health" }),
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "System" }));

    expect(
      await screen.findByText(/no log sources configured/i),
    ).toBeInTheDocument();

    // Sandbox health is a tab of that overlay, not a second pane rendered
    // beside Logs (bwsalmon/agents#640 split them) -- only the active
    // tab's panel is mounted, so reaching it means clicking it, the same
    // way SystemOverlay.test.jsx's own "shows Sandbox health on its own
    // tab" does.
    await user.click(screen.getByRole("tab", { name: "Sandbox health" }));

    expect(
      await screen.findByText(/no sandbox pool or host stats configured/i),
    ).toBeInTheDocument();
  });

  // grain/task-173: Metrics is a sidebar entry of its own rather than a
  // tab of the System pane -- a throughput report is what somebody opens
  // when nothing is wrong, which is the opposite of what the rest of
  // that pane is for.
  it("opens Metrics from its own sidebar entry, not from a tab of the System pane", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: "System" }));
    expect(
      await screen.findByText(/no log sources configured/i),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("tab", { name: "Metrics" }),
    ).not.toBeInTheDocument();
  });

  // The metrics report's backlog names the oldest queued task, and the
  // useful thing to do with that is go and look at it. Two stacked
  // dialogs would put the task behind the pane the click came from, so
  // App closes the Metrics overlay on the way through.
  it("opens the oldest queued task from the Metrics pane, leaving that pane behind", async () => {
    setupApi();
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText("Fix bug");

    await user.click(screen.getByRole("button", { name: "Metrics" }));
    await user.click(await screen.findByRole("button", { name: "task 1" }));

    expect(await screen.findByText("1 Fix bug")).toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Metrics" }),
    ).not.toBeInTheDocument();
  });

  // grain/task-317: how the list is narrowed is App's state and the
  // URL's, so a narrowed list is somewhere you can link to, reload into
  // and come back to -- rather than a set of menus that reset the next
  // time TaskList mounts.
  describe("a narrowed task list", () => {
    it("puts the search text in the address bar as it is typed", async () => {
      setupApi();
      const user = userEvent.setup();
      render(<App />);
      await screen.findByText("Fix bug");

      await user.type(screen.getByPlaceholderText("Search tasks…"), "feature");

      expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
      expect(screen.getByText("Add feature")).toBeInTheDocument();
      expect(window.location.pathname + window.location.search).toBe(
        "/?q=feature",
      );
    });

    it("opens a narrowed list from the URL alone", async () => {
      window.history.replaceState(null, "", "/?repo=acme/other");
      setupApi();
      render(<App />);

      expect(await screen.findByText("Add feature")).toBeInTheDocument();
      expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
      expect(window.location.search).toBe("?repo=acme/other");
    });

    it("names the picked repo in the URL when it is chosen from the menu", async () => {
      setupApi();
      const user = userEvent.setup();
      render(<App />);
      await screen.findByText("Fix bug");

      await user.click(screen.getByLabelText("Repo"));
      await user.click(
        within(screen.getByRole("listbox")).getByText("acme/other"),
      );

      expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
      await waitFor(() =>
        expect(window.location.search).toBe("?repo=acme/other"),
      );
    });

    // A link somebody kept naming a repo that has since gone quiet, or
    // a capability that was removed: the list it names is the whole
    // list, not an empty one (filterViews reads a choice it cannot
    // offer as "any").
    it("shows the whole list for a link naming a repo no task carries", async () => {
      window.history.replaceState(null, "", "/?repo=acme/gone");
      setupApi();
      render(<App />);

      expect(await screen.findByText("Fix bug")).toBeInTheDocument();
      expect(screen.getByText("Add feature")).toBeInTheDocument();
    });

    it("keeps the narrowing across a trip to another view and back", async () => {
      setupApi();
      const user = userEvent.setup();
      render(<App />);
      await screen.findByText("Fix bug");

      await user.type(screen.getByPlaceholderText("Search tasks…"), "feature");
      await user.click(screen.getByRole("button", { name: /^Repos/ }));
      await screen.findByText("acme/widgets");
      await user.click(screen.getByRole("button", { name: /^All tasks/ }));

      expect(await screen.findByText("Add feature")).toBeInTheDocument();
      expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
      expect(window.location.search).toBe("?q=feature");
    });

    // The board asks taskFilters.js's question the same way the list
    // does, so the answer comes with you rather than being asked again.
    it("carries the narrowing onto the board", async () => {
      setupApi();
      const user = userEvent.setup();
      render(<App />);
      await screen.findByText("Fix bug");

      await user.type(screen.getByPlaceholderText("Search tasks…"), "feature");
      await user.click(screen.getByRole("button", { name: "Board" }));

      await screen.findByRole("heading", { name: "Board" });
      expect(screen.getByText("Add feature")).toBeInTheDocument();
      expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
      expect(window.location.pathname + window.location.search).toBe(
        "/board?q=feature",
      );
    });

    // Narrowing replaces rather than pushes -- a history step per
    // keystroke would make Back mean "delete a letter" -- but a
    // narrowed URL is still an entry the Back button restores in full
    // once something else has navigated away from it.
    it("restores the narrowing when the browser navigates back to it", async () => {
      window.history.replaceState(null, "", "/?q=feature");
      setupApi();
      const user = userEvent.setup();
      render(<App />);
      await screen.findByText("Add feature");

      await user.click(screen.getByRole("button", { name: /^Repos/ }));
      await screen.findByText("acme/widgets");
      expect(window.location.pathname).toBe("/repos");

      window.history.replaceState(null, "", "/?q=feature");
      fireEvent.popState(window);

      expect(await screen.findByText("Add feature")).toBeInTheDocument();
      expect(screen.queryByText("Fix bug")).not.toBeInTheDocument();
      expect(screen.getByPlaceholderText("Search tasks…")).toHaveValue(
        "feature",
      );
    });

    it("clears the narrowing, and the query, with the toolbar's Clear", async () => {
      window.history.replaceState(null, "", "/?q=feature");
      setupApi();
      const user = userEvent.setup();
      render(<App />);
      await screen.findByText("Add feature");

      await user.click(screen.getByRole("button", { name: "Clear" }));

      expect(await screen.findByText("Fix bug")).toBeInTheDocument();
      expect(window.location.pathname + window.location.search).toBe("/");
    });
  });

  it("polls the task list on an interval", async () => {
    setupApi();
    render(<App />);
    await screen.findByText("Fix bug");

    const callsBefore = api.mock.calls.filter(
      (c) => c[0] === "/api/tasks",
    ).length;

    await waitFor(
      () => {
        const callsAfter = api.mock.calls.filter(
          (c) => c[0] === "/api/tasks",
        ).length;
        expect(callsAfter).toBeGreaterThan(callsBefore);
      },
      { timeout: 4000 },
    );
  }, 6000);
});
