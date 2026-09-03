import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import DebugOverlay from "./DebugOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

// bwsalmon/agents#640: Logs, Sandbox health and the reboot control moved
// here from Settings' own Debug tab, each as its own tab of this overlay
// (the same layout SettingsOverlay.jsx uses). Only the active tab's panel
// is mounted, so only it feeds its own GET /api/logs or GET /api/sandboxes
// call a response.
describe("DebugOverlay", () => {
  const noLogs = { enabled: false };
  const noSandboxes = { enabled: false };
  // A report from a deployment that has not done anything yet: every
  // field GET /api/metrics always sends, all at zero. MetricsPage.test
  // .jsx covers what the panel does with real numbers.
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

  it("shows Logs by default, with Sandbox health, Metrics and Restart as other tabs", async () => {
    api.mockResolvedValueOnce(noLogs);
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/no log sources configured|not available/i)).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Logs" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Sandbox health" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Metrics" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Restart" })).toBeInTheDocument();
    expect(api).toHaveBeenCalledTimes(1);
  });

  it("shows Sandbox health on its own tab", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce(noSandboxes);
    const user = userEvent.setup();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Sandbox health" }));

    expect(await screen.findByText(/no sandbox pool or host stats configured/i)).toBeInTheDocument();
  });

  // The throughput and latency report joined these rather than taking a
  // sidebar entry of its own, and fetches only once its tab is the
  // active one, the same as the panels above.
  it("shows Metrics on its own tab, over GET /api/metrics", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce(emptyMetrics);
    const user = userEvent.setup();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Metrics" }));

    expect(await screen.findByText("Throughput")).toBeInTheDocument();
    expect(api).toHaveBeenLastCalledWith("/api/metrics?window=7d&buckets=24");
  });

  // onOpenTask is the one link out of any of these panels: the metrics
  // backlog names the oldest queued task. App uses it to close this
  // overlay and open that task -- see App.test.jsx's own end of it.
  it("passes the metrics backlog's oldest queued task up to onOpenTask", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce({
      ...emptyMetrics,
      backlog: { byState: { queued: 1 }, queued: 1, oldestQueuedSeconds: 3600, oldestQueuedTaskId: "51" },
    });
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} onOpenTask={onOpenTask} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Metrics" }));
    await user.click(await screen.findByRole("button", { name: "task 51" }));

    expect(onOpenTask).toHaveBeenCalledWith("51");
  });

  it("shows the reboot control on the Restart tab when reboot is enabled", async () => {
    api.mockResolvedValueOnce(noLogs);
    const user = userEvent.setup();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Restart" }));

    expect(screen.getByRole("button", { name: "Reboot host" })).toBeInTheDocument();
  });

  it("does not show the reboot control on the Restart tab when reboot is not enabled", async () => {
    api.mockResolvedValueOnce(noLogs);
    const user = userEvent.setup();
    render(<DebugOverlay config={{ rebootEnabled: false }} onClose={() => {}} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Restart" }));

    expect(screen.queryByRole("button", { name: "Reboot host" })).not.toBeInTheDocument();
    expect(screen.getByText(/rebooting the host is not enabled/i)).toBeInTheDocument();
  });

  it("reboots the host after confirmation", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce({});
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);
    await user.click(screen.getByRole("tab", { name: "Restart" }));

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(api).toHaveBeenCalledWith("/api/host/reboot", { method: "POST" });
    vi.unstubAllGlobals();
  });

  it("does not reboot when the confirmation is declined", async () => {
    api.mockResolvedValueOnce(noLogs);
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(false));
    const user = userEvent.setup();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);
    await user.click(screen.getByRole("tab", { name: "Restart" }));

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(api).toHaveBeenCalledTimes(1);
    vi.unstubAllGlobals();
  });

  // bwsalmon/agents#581: a successful reboot cuts the connection out
  // from under the response before it finishes its round trip, which
  // the browser reports as a TypeError -- fetch's own shape for a
  // network-level failure (api.js never throws that type itself). That
  // used to surface as an error banner on the one action where it is
  // actually a sign of success, making the button look broken.
  it("does not show an error when the connection drops as the reboot itself would cause", async () => {
    api.mockResolvedValueOnce(noLogs).mockRejectedValueOnce(new TypeError("Failed to fetch"));
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    const showError = vi.fn();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={showError} />);
    await screen.findByText(/no log sources configured/i);
    await user.click(screen.getByRole("tab", { name: "Restart" }));

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(showError).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("still shows an error for a real reboot failure", async () => {
    api.mockResolvedValueOnce(noLogs).mockRejectedValueOnce(new Error("reboot is not available"));
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    const showError = vi.fn();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={showError} />);
    await screen.findByText(/no log sources configured/i);
    await user.click(screen.getByRole("tab", { name: "Restart" }));

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(showError).toHaveBeenCalledWith(new Error("reboot is not available"));
    vi.unstubAllGlobals();
  });
});
