import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import MetricsOverlay from "./MetricsOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

// grain/task-173: the throughput and latency report was the fourth tab
// of DebugOverlay and is its own sidebar destination now. This covers
// the wrapper only -- that the report is in a pane with a header naming
// it, and that the one link out of it reaches App. MetricsPage.test.jsx
// covers what the report itself does with the numbers.
describe("MetricsOverlay", () => {
  // A report from a deployment that has not done anything yet: every
  // field GET /api/metrics always sends, all at zero.
  const emptyMetrics = {
    since: "2026-08-27T00:00:00Z",
    until: "2026-09-03T00:00:00Z",
    windowSeconds: 604800,
    throughput: {
      tasksFiled: 0, tasksCompleted: 0, tasksClosed: 0, runsStarted: 0, runsFinished: 0,
      filedPerDay: 0, completedPerDay: 0, runsFinishedPerDay: 0, buckets: [],
    },
    latency: [],
    runs: { outcomes: {}, attemptsPerCompletion: 0, meanConcurrent: 0, maxConcurrent: 0, utilization: 0, live: 0 },
    backlog: { byState: {}, queued: 0, oldestQueuedSeconds: 0, oldestQueuedTaskId: "" },
  };

  afterEach(() => {
    api.mockReset();
  });

  it("shows the report over GET /api/metrics, in a pane headed Metrics", async () => {
    api.mockResolvedValueOnce(emptyMetrics);
    render(<MetricsOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText("Throughput")).toBeInTheDocument();
    expect(api).toHaveBeenCalledWith("/api/metrics?window=7d&buckets=24");

    // The pane's fixed header is what names it -- the panel itself no
    // longer prints a second "Metrics" title above the window picker.
    expect(document.querySelector(".MuiDialog-paper")).toHaveClass("MuiDialog-paperFullScreen");
    expect(document.querySelector(".overlay-pane-header"))
      .toContainElement(screen.getByRole("heading", { name: "Metrics" }));
    expect(screen.getAllByRole("heading", { name: "Metrics" })).toHaveLength(1);
  });

  // The one link out of the report: the backlog names the oldest queued
  // task. App uses it to close this pane and open that task -- see
  // App.test.jsx's own end of it.
  it("passes the backlog's oldest queued task up to onOpenTask", async () => {
    api.mockResolvedValueOnce({
      ...emptyMetrics,
      backlog: { byState: { queued: 1 }, queued: 1, oldestQueuedSeconds: 3600, oldestQueuedTaskId: "51" },
    });
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    render(<MetricsOverlay onClose={() => {}} onOpenTask={onOpenTask} showError={() => {}} />);

    await user.click(await screen.findByRole("button", { name: "task 51" }));

    expect(onOpenTask).toHaveBeenCalledWith("51");
  });
});
