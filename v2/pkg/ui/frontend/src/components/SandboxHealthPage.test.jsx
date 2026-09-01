import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import SandboxHealthPage, { appendHistory } from "./SandboxHealthPage.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

describe("SandboxHealthPage", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("shows the header and a spinner while the initial fetch is in flight", async () => {
    let resolve;
    api.mockReturnValueOnce(new Promise((r) => { resolve = r; }));
    render(<SandboxHealthPage showError={() => {}} />);

    expect(screen.getByText("Sandbox health")).toBeInTheDocument();
    expect(screen.getByRole("progressbar")).toBeInTheDocument();

    resolve({ enabled: true, sandboxes: [], host: null });
    expect(await screen.findByText("No runs in flight, so no sandboxes exist.")).toBeInTheDocument();
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();
  });

  it("shows a note instead of a table when nothing is configured", async () => {
    api.mockResolvedValueOnce({ enabled: false });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText(/not available/i)).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("shows the host's load average and memory", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [],
      host: { loadAverage1: 0.5, loadAverage5: 0.4, loadAverage15: 0.3, memoryUsedMB: 512, memoryTotalMB: 1024 },
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText(/0\.50 \/ 0\.40 \/ 0\.30/)).toBeInTheDocument();
    expect(screen.getByText(/512 \/ 1024 MB/)).toBeInTheDocument();
  });

  it("lists every sandbox with its status", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [
        { sandbox: "t1-r1", backend: "kontur", name: "g-t1-r1", ready: true, loadAverage: "0.1 0.2 0.3", memoryUsedMB: 100, memoryTotalMB: 200 },
        { sandbox: "t2-r1", backend: "kontur", name: "g-t2-r1", ready: false, error: "connection refused" },
      ],
      host: null,
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText("g-t1-r1")).toBeInTheDocument();
    expect(screen.getByText("ready")).toBeInTheDocument();
    expect(screen.getByText("connection refused")).toBeInTheDocument();
    expect(screen.getByText("0.1 0.2 0.3")).toBeInTheDocument();
    expect(screen.getByText("100 / 200 MB")).toBeInTheDocument();

    // Every row belongs to a run, and says which -- the substance of what
    // changed here, not just a renamed field. The table used to lead with
    // a slot number that meant nothing outside the dispatch loop; it
    // leads with the run id now, which is also what model.Run.Sandbox
    // records, so a row joins straight onto the run it belongs to.
    expect(screen.getByRole("columnheader", { name: "Run" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "t1-r1" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "t2-r1" })).toBeInTheDocument();
  });

  it("shows a host error without hiding the sandbox list", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [{ sandbox: "t1-r1", backend: "host", name: "/tmp/sandbox-0", ready: true }],
      hostError: "not on linux",
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText(/not on linux/)).toBeInTheDocument();
    expect(screen.getByText("/tmp/sandbox-0")).toBeInTheDocument();
  });

  it("re-fetches when Refresh is clicked", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, sandboxes: [], host: null })
      .mockResolvedValueOnce({ enabled: true, sandboxes: [{ sandbox: "t1-r1", backend: "host", name: "/tmp/s0", ready: true }], host: null });
    const user = userEvent.setup();
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText("No runs in flight, so no sandboxes exist.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Refresh" }));

    expect(await screen.findByText("/tmp/s0")).toBeInTheDocument();
  });

  it("labels the host and per-sandbox trend charts", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [{ sandbox: "t1-r1", backend: "kontur", name: "g-t1-r1", ready: true, loadAverage: "0.1 0.2 0.3", memoryUsedMB: 100, memoryTotalMB: 200 }],
      host: { loadAverage1: 0.5, loadAverage5: 0.4, loadAverage15: 0.3, memoryUsedMB: 512, memoryTotalMB: 1024 },
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText("CPU (1 min load average)")).toBeInTheDocument();
    expect(screen.getByText("Memory (MB)")).toBeInTheDocument();
    expect(screen.getByText("CPU trend")).toBeInTheDocument();
    expect(screen.getByText("Memory trend")).toBeInTheDocument();
  });

  it("accumulates trend history across polls", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, sandboxes: [], host: { loadAverage1: 0.5, loadAverage5: 0.4, loadAverage15: 0.3, memoryUsedMB: 100, memoryTotalMB: 1000 } })
      .mockResolvedValueOnce({ enabled: true, sandboxes: [], host: { loadAverage1: 0.6, loadAverage5: 0.4, loadAverage15: 0.3, memoryUsedMB: 200, memoryTotalMB: 1000 } });
    const user = userEvent.setup();
    render(<SandboxHealthPage showError={() => {}} />);

    await screen.findByText("CPU (1 min load average)");
    expect(screen.getAllByLabelText("Not enough data yet")).toHaveLength(2);

    await user.click(screen.getByRole("button", { name: "Refresh" }));

    expect(await screen.findByLabelText("Trend, latest value 0.6")).toBeInTheDocument();
  });
});

describe("appendHistory", () => {
  const empty = { host: { cpu: [], mem: [] }, sandboxes: {} };

  // Keyed by the sandbox's own name, which is the run's id -- not by a
  // slot number, and not by array position. Two sandboxes in one poll
  // therefore get a series each, which is what makes a per-sandbox trend
  // chart follow the right sandbox across polls as runs come and go.
  it("keeps a separate series per sandbox, keyed by name", () => {
    const result = appendHistory(empty, {
      sandboxes: [
        { sandbox: "t1-r1", ready: true, loadAverage: "0.25 0.5 0.75", memoryUsedMB: 40 },
        { sandbox: "t2-r1", ready: true, loadAverage: "1.5 1.0 0.5", memoryUsedMB: 90 },
      ],
    });
    expect(Object.keys(result.sandboxes).sort()).toEqual(["t1-r1", "t2-r1"]);
    expect(result.sandboxes["t1-r1"]).toEqual({ cpu: [0.25], mem: [40] });
    expect(result.sandboxes["t2-r1"]).toEqual({ cpu: [1.5], mem: [90] });
  });

  it("skips a poll with no host stats", () => {
    expect(appendHistory(empty, { enabled: true, sandboxes: [] })).toEqual(empty);
  });

  it("appends host CPU and memory samples", () => {
    const result = appendHistory(empty, { host: { loadAverage1: 1.5, memoryUsedMB: 300 }, sandboxes: [] });
    expect(result.host).toEqual({ cpu: [1.5], mem: [300] });
  });

  it("skips a sandbox that is not ready", () => {
    const result = appendHistory(empty, { sandboxes: [{ sandbox: "t1-r1", ready: false, error: "boom" }] });
    expect(result.sandboxes["t1-r1"]).toEqual({ cpu: [], mem: [] });
  });

  it("appends a ready sandbox's load average and memory", () => {
    const result = appendHistory(empty, {
      sandboxes: [{ sandbox: "t1-r1", ready: true, loadAverage: "0.25 0.5 0.75", memoryUsedMB: 40, memoryTotalMB: 100 }],
    });
    expect(result.sandboxes["t1-r1"]).toEqual({ cpu: [0.25], mem: [40] });
  });

  it("drops a sandbox's history once it stops appearing in a poll", () => {
    const withOne = appendHistory(empty, {
      sandboxes: [{ sandbox: "t1-r1", ready: true, loadAverage: "0.25 0.5 0.75", memoryUsedMB: 40 }],
    });
    expect(Object.keys(withOne.sandboxes)).toEqual(["t1-r1"]);

    // t1-r1's run has ended and a different run's sandbox is now live --
    // a sandbox name is never reused, so a poll that no longer mentions
    // t1-r1 means it is gone for good, not merely idle.
    const withNext = appendHistory(withOne, {
      sandboxes: [{ sandbox: "t2-r1", ready: true, loadAverage: "1.0 1.0 1.0", memoryUsedMB: 10 }],
    });
    expect(Object.keys(withNext.sandboxes)).toEqual(["t2-r1"]);
  });

  it("caps each series at 60 samples", () => {
    let history = empty;
    for (let i = 0; i < 65; i++) {
      history = appendHistory(history, { host: { loadAverage1: i, memoryUsedMB: i }, sandboxes: [] });
    }
    expect(history.host.cpu).toHaveLength(60);
    expect(history.host.cpu[0]).toBe(5);
    expect(history.host.cpu[59]).toBe(64);
  });
});
