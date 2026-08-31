import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import SandboxHealthPage from "./SandboxHealthPage.jsx";
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
    expect(await screen.findByText("No sandboxes tracked yet.")).toBeInTheDocument();
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
        { slot: "0", backend: "kontur", name: "grain-0", ready: true, loadAverage: "0.1 0.2 0.3", memoryUsedMB: 100, memoryTotalMB: 200 },
        { slot: "1", backend: "kontur", name: "grain-1", ready: false, error: "connection refused" },
      ],
      host: null,
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText("grain-0")).toBeInTheDocument();
    expect(screen.getByText("ready")).toBeInTheDocument();
    expect(screen.getByText("connection refused")).toBeInTheDocument();
    expect(screen.getByText("0.1 0.2 0.3")).toBeInTheDocument();
    expect(screen.getByText("100 / 200 MB")).toBeInTheDocument();
  });

  it("shows a host error without hiding the sandbox list", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      sandboxes: [{ slot: "0", backend: "host", name: "/tmp/sandbox-0", ready: true }],
      hostError: "not on linux",
    });
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText(/not on linux/)).toBeInTheDocument();
    expect(screen.getByText("/tmp/sandbox-0")).toBeInTheDocument();
  });

  it("re-fetches when Refresh is clicked", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, sandboxes: [], host: null })
      .mockResolvedValueOnce({ enabled: true, sandboxes: [{ slot: "0", backend: "host", name: "/tmp/s0", ready: true }], host: null });
    const user = userEvent.setup();
    render(<SandboxHealthPage showError={() => {}} />);

    expect(await screen.findByText("No sandboxes tracked yet.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Refresh" }));

    expect(await screen.findByText("/tmp/s0")).toBeInTheDocument();
  });
});
