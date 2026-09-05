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

  it("shows Logs by default, with Sandbox health, Top, Root shell and Restart as other tabs", async () => {
    api.mockResolvedValueOnce(noLogs);
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );

    expect(
      await screen.findByText(/no log sources configured|not available/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Logs" })).toBeInTheDocument();
    expect(
      screen.getByRole("tab", { name: "Sandbox health" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Top" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Root shell" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Restart" })).toBeInTheDocument();
    expect(api).toHaveBeenCalledTimes(1);
  });

  // grain/task-13: the last of the reading panes and the first that
  // changes the machine -- one command as root on the host, for the
  // failure the three tabs before it could not explain. It needs no
  // fetch of its own to render: GET /api/config has already said whether
  // this deployment has a responder behind the route.
  it("shows the root shell on its own tab, without a call of its own", async () => {
    api.mockResolvedValueOnce(noLogs);
    const user = userEvent.setup();
    render(
      <DebugOverlay
        config={{ rebootEnabled: true, rootShellEnabled: true }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Root shell" }));

    expect(
      screen.getByRole("button", { name: /run as root/i }),
    ).toBeInTheDocument();
    expect(api).toHaveBeenCalledTimes(1);
  });

  it("says the root shell is unavailable where the deployment has none", async () => {
    api.mockResolvedValueOnce(noLogs);
    const user = userEvent.setup();
    render(
      <DebugOverlay
        config={{ rebootEnabled: true, rootShellEnabled: false }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Root shell" }));

    expect(screen.getByText(/no root shell configured/i)).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /run as root/i }),
    ).not.toBeInTheDocument();
  });

  // grain/task-173: the throughput and latency report is a sidebar
  // destination of its own now (MetricsOverlay.jsx), not a tab in here.
  it("does not show Metrics as a tab", async () => {
    api.mockResolvedValueOnce(noLogs);
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByText(/no log sources configured|not available/i);

    expect(
      screen.queryByRole("tab", { name: "Metrics" }),
    ).not.toBeInTheDocument();
  });

  // grain/task-115: these panels fill the content area beside the
  // sidebar now rather than the widest centered box Overlay draws -- a
  // log tail and a sandbox table both wanted the room -- and the tab
  // strip is the pane's fixed header, so it stays put while one scrolls.
  it("fills the pane beside the sidebar, with its tabs in the fixed header", async () => {
    api.mockResolvedValueOnce(noLogs);
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByText(/no log sources configured|not available/i);

    expect(document.querySelector(".MuiDialog-paper")).toHaveClass(
      "MuiDialog-paperFullScreen",
    );
    const head = document.querySelector(".overlay-pane-header");
    expect(head).toContainElement(
      screen.getByRole("tab", { name: "Sandbox health" }),
    );
    expect(head).toContainElement(
      screen.getByRole("heading", { name: "Debug" }),
    );
  });

  it("shows Sandbox health on its own tab", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce(noSandboxes);
    const user = userEvent.setup();
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Sandbox health" }));

    expect(
      await screen.findByText(/no sandbox pool or host stats configured/i),
    ).toBeInTheDocument();
  });

  // grain/task-120: `top` on the daemon's own machine, next to the
  // aggregate host reading it explains -- and, like every panel here,
  // fetching only once its own tab is the active one.
  it("shows Top on its own tab, over GET /api/host/top", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce({
      enabled: true,
      lines: ["top - 12:00:00 up 3 days,  load average: 1.10, 0.90, 0.75"],
    });
    const user = userEvent.setup();
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Top" }));

    expect(await screen.findByText(/load average: 1.10/)).toBeInTheDocument();
    expect(api).toHaveBeenLastCalledWith("/api/host/top?lines=60");
  });

  it("shows the reboot control on the Restart tab when reboot is enabled", async () => {
    api.mockResolvedValueOnce(noLogs);
    const user = userEvent.setup();
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Restart" }));

    expect(
      screen.getByRole("button", { name: "Reboot host" }),
    ).toBeInTheDocument();
  });

  it("does not show the reboot control on the Restart tab when reboot is not enabled", async () => {
    api.mockResolvedValueOnce(noLogs);
    const user = userEvent.setup();
    render(
      <DebugOverlay
        config={{ rebootEnabled: false }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByText(/no log sources configured/i);

    await user.click(screen.getByRole("tab", { name: "Restart" }));

    expect(
      screen.queryByRole("button", { name: "Reboot host" }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText(/rebooting the host is not enabled/i),
    ).toBeInTheDocument();
  });

  it("reboots the host after confirmation", async () => {
    api.mockResolvedValueOnce(noLogs).mockResolvedValueOnce({});
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
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
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={() => {}}
      />,
    );
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
    api
      .mockResolvedValueOnce(noLogs)
      .mockRejectedValueOnce(new TypeError("Failed to fetch"));
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    const showError = vi.fn();
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={showError}
      />,
    );
    await screen.findByText(/no log sources configured/i);
    await user.click(screen.getByRole("tab", { name: "Restart" }));

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(showError).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("still shows an error for a real reboot failure", async () => {
    api
      .mockResolvedValueOnce(noLogs)
      .mockRejectedValueOnce(new Error("reboot is not available"));
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    const showError = vi.fn();
    render(
      <DebugOverlay
        config={{ rebootEnabled: true }}
        onClose={() => {}}
        showError={showError}
      />,
    );
    await screen.findByText(/no log sources configured/i);
    await user.click(screen.getByRole("tab", { name: "Restart" }));

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(showError).toHaveBeenCalledWith(
      new Error("reboot is not available"),
    );
    vi.unstubAllGlobals();
  });
});
