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
    api.mockReturnValueOnce(
      new Promise((r) => {
        resolve = r;
      }),
    );
    render(<SandboxHealthPage showError={() => {}} />);

    expect(screen.getByText("Sandbox health")).toBeInTheDocument();
    expect(screen.getByRole("progressbar")).toBeInTheDocument();

    resolve({ enabled: true, sandboxes: [], host: null });
    expect(
      await screen.findByText("No runs in flight, so no sandboxes exist."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();
  });

  it("shows a note instead of a table when nothing is configured", async () => {
    api.mockResolvedValueOnce({ enabled: false });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText(/not available/i)).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("shows the host's load average, memory and every disk it reports", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [],
      host: {
        loadAverage1: 0.5,
        loadAverage5: 0.4,
        loadAverage15: 0.3,
        memoryUsedMB: 512,
        memoryTotalMB: 1024,
        disks: [
          {
            holds: ["store"],
            path: "/var/lib/grain",
            usedMB: 4096,
            totalMB: 20480,
          },
          {
            holds: ["sandboxes", "docker"],
            path: "/var/lib/grain-sandbox",
            usedMB: 61440,
            totalMB: 102400,
          },
        ],
      },
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(
      await screen.findByText(/0\.50 \/ 0\.40 \/ 0\.30/),
    ).toBeInTheDocument();
    expect(screen.getByText(/512 \/ 1024 MB/)).toBeInTheDocument();
    // Shown in GB rather than in the MB it arrives as: a data disk is
    // counted in tens of gigabytes, and "4096 / 20480 MB" answers "how
    // full is it" worse than "4.0 / 20.0 GB" does.
    expect(screen.getByText("4.0 / 20.0 GB")).toBeInTheDocument();
    // The sandbox volume, which the pane showed no figure for at all
    // while it had one "disk" number taken from the store's filesystem
    // (grain/task-148) -- and which is the one that actually fills.
    expect(screen.getByText("60.0 / 100.0 GB")).toBeInTheDocument();
    expect(screen.getByText("/var/lib/grain-sandbox")).toBeInTheDocument();
    // One row, not two, for a sandbox root and a docker data root the
    // daemon found to be the same filesystem -- named for both.
    expect(
      screen.getByRole("cell", { name: "sandboxes, docker" }),
    ).toBeInTheDocument();
  });

  it("says why a disk it cannot read has no figure", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [],
      host: {
        loadAverage1: 0.5,
        loadAverage5: 0.4,
        loadAverage15: 0.3,
        memoryUsedMB: 512,
        memoryTotalMB: 1024,
        disks: [
          {
            holds: ["store"],
            path: "/var/lib/grain",
            usedMB: 4096,
            totalMB: 20480,
          },
          {
            holds: ["sandboxes"],
            path: "/var/lib/grain-sandbox",
            error:
              "sysstat: statfs /var/lib/grain-sandbox: no such file or directory",
          },
        ],
      },
    });
    render(<SandboxHealthPage showError={() => {}} />);

    // A volume that has stopped answering is exactly what an operator
    // opens this pane for, so the row says so rather than going quiet --
    // and the disks either side of it keep their figures.
    expect(
      await screen.findByText(/statfs \/var\/lib\/grain-sandbox/),
    ).toBeInTheDocument();
    expect(screen.getByText("4.0 / 20.0 GB")).toBeInTheDocument();
  });

  it("lists every sandbox with its status", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [
        {
          sandbox: "t1-r1",
          backend: "kontur",
          name: "g-t1-r1",
          ready: true,
          loadAverage: "0.1 0.2 0.3",
          memoryUsedMB: 100,
          memoryTotalMB: 200,
          diskUsedMB: 3072,
          diskTotalMB: 20480,
        },
        {
          sandbox: "t2-r1",
          backend: "kontur",
          name: "g-t2-r1",
          ready: false,
          error: "connection refused",
        },
      ],
      host: null,
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText("g-t1-r1")).toBeInTheDocument();
    expect(screen.getByText("ready")).toBeInTheDocument();
    expect(screen.getByText("connection refused")).toBeInTheDocument();
    expect(screen.getByText("0.1 0.2 0.3")).toBeInTheDocument();
    expect(screen.getByText("100 / 200 MB")).toBeInTheDocument();
    expect(screen.getByText("3.0 / 20.0 GB")).toBeInTheDocument();
    // The unreachable sandbox has no disk figure of its own, and gets the
    // same dash the memory column already gives it rather than a 0 that
    // would read as an empty disk.
    expect(screen.getAllByText("—").length).toBeGreaterThan(0);

    // Every row belongs to a run, and says which -- the substance of what
    // changed here, not just a renamed field. The table used to lead with
    // a slot number that meant nothing outside the dispatch loop; it
    // leads with the run id now, which is also what model.Run.Sandbox
    // records, so a row joins straight onto the run it belongs to.
    expect(
      screen.getByRole("columnheader", { name: "Run" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "t1-r1" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "t2-r1" })).toBeInTheDocument();
  });

  it("shows a host error without hiding the sandbox list", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [
        {
          sandbox: "t1-r1",
          backend: "host",
          name: "/tmp/sandbox-0",
          ready: true,
        },
      ],
      hostError: "not on linux",
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText(/not on linux/)).toBeInTheDocument();
    expect(screen.getByText("/tmp/sandbox-0")).toBeInTheDocument();
  });

  it("re-fetches when Refresh is clicked", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, sandboxes: [], host: null })
      .mockResolvedValueOnce({
        enabled: true,
        sandboxes: [
          { sandbox: "t1-r1", backend: "host", name: "/tmp/s0", ready: true },
        ],
        host: null,
      });
    const user = userEvent.setup();
    render(<SandboxHealthPage showError={() => {}} />);

    expect(
      await screen.findByText("No runs in flight, so no sandboxes exist."),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Refresh" }));

    expect(await screen.findByText("/tmp/s0")).toBeInTheDocument();
  });

  it("labels the host and per-sandbox trend charts", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [
        {
          sandbox: "t1-r1",
          backend: "kontur",
          name: "g-t1-r1",
          ready: true,
          loadAverage: "0.1 0.2 0.3",
          memoryUsedMB: 100,
          memoryTotalMB: 200,
        },
      ],
      host: {
        loadAverage1: 0.5,
        loadAverage5: 0.4,
        loadAverage15: 0.3,
        memoryUsedMB: 512,
        memoryTotalMB: 1024,
      },
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(
      await screen.findByText("CPU (1 min load average)"),
    ).toBeInTheDocument();
    expect(screen.getByText("Memory (MB)")).toBeInTheDocument();
    // The host's disk trends live in the disk table's own Trend column
    // now, one per filesystem, rather than in a third chart beside these
    // two (grain/task-148).
    expect(screen.getByText("No disk figures available.")).toBeInTheDocument();
    expect(screen.getByText("CPU trend")).toBeInTheDocument();
    expect(screen.getByText("Memory trend")).toBeInTheDocument();
    expect(screen.getByText("Disk trend")).toBeInTheDocument();
  });

  it("accumulates trend history across polls", async () => {
    api
      .mockResolvedValueOnce({
        enabled: true,
        sandboxes: [],
        host: {
          loadAverage1: 0.5,
          loadAverage5: 0.4,
          loadAverage15: 0.3,
          memoryUsedMB: 100,
          memoryTotalMB: 1000,
        },
      })
      .mockResolvedValueOnce({
        enabled: true,
        sandboxes: [],
        host: {
          loadAverage1: 0.6,
          loadAverage5: 0.4,
          loadAverage15: 0.3,
          memoryUsedMB: 200,
          memoryTotalMB: 1000,
        },
      });
    const user = userEvent.setup();
    render(<SandboxHealthPage showError={() => {}} />);

    await screen.findByText("CPU (1 min load average)");
    // Two host charts beside each other -- CPU and memory. The disk
    // trends moved into the disk table (grain/task-148), and this poll's
    // host section reports no disk at all, so there is no third one.
    expect(screen.getAllByLabelText("Not enough data yet")).toHaveLength(2);

    await user.click(screen.getByRole("button", { name: "Refresh" }));

    expect(
      await screen.findByLabelText("Trend, latest value 0.6"),
    ).toBeInTheDocument();
  });
});

describe("appendHistory", () => {
  const empty = { host: { cpu: [], mem: [], disks: {} }, sandboxes: {} };

  // Keyed by the sandbox's own name, which is the run's id -- not by a
  // slot number, and not by array position. Two sandboxes in one poll
  // therefore get a series each, which is what makes a per-sandbox trend
  // chart follow the right sandbox across polls as runs come and go.
  it("keeps a separate series per sandbox, keyed by name", () => {
    const result = appendHistory(empty, {
      sandboxes: [
        {
          sandbox: "t1-r1",
          ready: true,
          loadAverage: "0.25 0.5 0.75",
          memoryUsedMB: 40,
        },
        {
          sandbox: "t2-r1",
          ready: true,
          loadAverage: "1.5 1.0 0.5",
          memoryUsedMB: 90,
        },
      ],
    });
    expect(Object.keys(result.sandboxes).sort()).toEqual(["t1-r1", "t2-r1"]);
    expect(result.sandboxes["t1-r1"]).toEqual({
      cpu: [0.25],
      mem: [40],
      disk: [],
    });
    expect(result.sandboxes["t2-r1"]).toEqual({
      cpu: [1.5],
      mem: [90],
      disk: [],
    });
  });

  it("skips a poll with no host stats", () => {
    expect(appendHistory(empty, { enabled: true, sandboxes: [] })).toEqual(
      empty,
    );
  });

  it("appends host CPU, memory and one disk series per filesystem", () => {
    const result = appendHistory(empty, {
      host: {
        loadAverage1: 1.5,
        memoryUsedMB: 300,
        disks: [
          {
            holds: ["store"],
            path: "/var/lib/grain",
            usedMB: 4000,
            totalMB: 20000,
          },
          {
            holds: ["sandboxes", "docker"],
            path: "/var/lib/grain-sandbox",
            usedMB: 61000,
            totalMB: 100000,
          },
        ],
      },
      sandboxes: [],
    });
    expect(result.host).toEqual({
      cpu: [1.5],
      mem: [300],
      // Keyed by path, so a chart follows the same filesystem across
      // polls however the list around it changes.
      disks: { "/var/lib/grain": [4000], "/var/lib/grain-sandbox": [61000] },
    });
  });

  // 0/0 is how one disk with no reading reports itself (a volume that
  // has stopped answering statfs) -- plotting the 0 would draw an empty
  // disk rather than a missing sample.
  it("skips a disk sample when that filesystem has no reading", () => {
    const result = appendHistory(empty, {
      host: {
        loadAverage1: 1.5,
        memoryUsedMB: 300,
        disks: [
          {
            holds: ["sandboxes"],
            path: "/var/lib/grain-sandbox",
            usedMB: 0,
            totalMB: 0,
            error: "no such file or directory",
          },
        ],
      },
      sandboxes: [],
    });
    expect(result.host).toEqual({
      cpu: [1.5],
      mem: [300],
      disks: { "/var/lib/grain-sandbox": [] },
    });
  });

  // A filesystem the host has stopped reporting takes its series with it
  // rather than accumulating forever behind a chart nothing draws.
  it("drops the series of a filesystem no longer reported", () => {
    const first = appendHistory(empty, {
      host: {
        loadAverage1: 1.5,
        memoryUsedMB: 300,
        disks: [
          { holds: ["docker"], path: "/docker", usedMB: 10, totalMB: 100 },
        ],
      },
      sandboxes: [],
    });
    const second = appendHistory(first, {
      host: {
        loadAverage1: 1.5,
        memoryUsedMB: 300,
        disks: [{ holds: ["store"], path: "/store", usedMB: 20, totalMB: 100 }],
      },
      sandboxes: [],
    });
    expect(second.host.disks).toEqual({ "/store": [20] });
  });

  it("skips a sandbox that is not ready", () => {
    const result = appendHistory(empty, {
      sandboxes: [{ sandbox: "t1-r1", ready: false, error: "boom" }],
    });
    expect(result.sandboxes["t1-r1"]).toEqual({ cpu: [], mem: [], disk: [] });
  });

  it("appends a ready sandbox's load average, memory and disk", () => {
    const result = appendHistory(empty, {
      sandboxes: [
        {
          sandbox: "t1-r1",
          ready: true,
          loadAverage: "0.25 0.5 0.75",
          memoryUsedMB: 40,
          memoryTotalMB: 100,
          diskUsedMB: 3072,
          diskTotalMB: 20480,
        },
      ],
    });
    expect(result.sandboxes["t1-r1"]).toEqual({
      cpu: [0.25],
      mem: [40],
      disk: [3072],
    });
  });

  it("caps each series at 60 samples", () => {
    let history = empty;
    for (let i = 0; i < 65; i++) {
      history = appendHistory(history, {
        host: { loadAverage1: i, memoryUsedMB: i },
        sandboxes: [],
      });
    }
    expect(history.host.cpu).toHaveLength(60);
    expect(history.host.cpu[0]).toBe(5);
    expect(history.host.cpu[59]).toBe(64);
  });
});
