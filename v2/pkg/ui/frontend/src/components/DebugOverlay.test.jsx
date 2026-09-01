import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import DebugOverlay from "./DebugOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

// bwsalmon/agents#640: Logs, Sandbox health and the reboot control moved
// here from Settings' own Debug tab -- opening this overlay mounts
// LogsPage and SandboxHealthPage, so every test below feeds their own
// GET /api/logs and GET /api/sandboxes calls a response.
describe("DebugOverlay", () => {
  const noLogs = { enabled: false };
  const noSandboxes = { enabled: false };

  afterEach(() => {
    api.mockReset();
  });

  it("shows Logs and Sandbox health alongside the danger zone", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce(noSandboxes);
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/no log sources configured/i)).toBeInTheDocument();
    expect(await screen.findByText(/no sandbox pool or host stats configured/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reboot host" })).toBeInTheDocument();
  });

  it("does not show the danger zone when reboot is not enabled", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce(noSandboxes);
    render(<DebugOverlay config={{ rebootEnabled: false }} onClose={() => {}} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);

    expect(screen.queryByRole("button", { name: "Reboot host" })).not.toBeInTheDocument();
  });

  it("reboots the host after confirmation", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce(noSandboxes).mockResolvedValueOnce({});
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(api).toHaveBeenCalledWith("/api/host/reboot", { method: "POST" });
    vi.unstubAllGlobals();
  });

  it("does not reboot when the confirmation is declined", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce(noSandboxes);
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(false));
    const user = userEvent.setup();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(api).toHaveBeenCalledTimes(2);
    vi.unstubAllGlobals();
  });

  // bwsalmon/agents#581: a successful reboot cuts the connection out
  // from under the response before it finishes its round trip, which
  // the browser reports as a TypeError -- fetch's own shape for a
  // network-level failure (api.js never throws that type itself). That
  // used to surface as an error banner on the one action where it is
  // actually a sign of success, making the button look broken.
  it("does not show an error when the connection drops as the reboot itself would cause", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce(noSandboxes)
      .mockRejectedValueOnce(new TypeError("Failed to fetch"));
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    const showError = vi.fn();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={showError} />);
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(showError).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("still shows an error for a real reboot failure", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce(noSandboxes)
      .mockRejectedValueOnce(new Error("reboot is not available"));
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    const showError = vi.fn();
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={showError} />);
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(showError).toHaveBeenCalledWith(new Error("reboot is not available"));
    vi.unstubAllGlobals();
  });
});
