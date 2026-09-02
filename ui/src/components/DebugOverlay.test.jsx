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

  afterEach(() => {
    api.mockReset();
  });

  it("shows Logs by default, with Sandbox health and Restart as other tabs", async () => {
    api.mockResolvedValueOnce(noLogs);
    render(<DebugOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/no log sources configured|not available/i)).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Logs" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Sandbox health" })).toBeInTheDocument();
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
