import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import SettingsOverlay from "./SettingsOverlay.jsx";
import { ThemeModeProvider } from "../ThemeModeContext.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const settings = {
  configured: true,
  pollInterval: "30s",
  maxConcurrent: 2,
  geminiModel: "gemini-2.5-pro",
  maxAgentTurns: 40,
  githubHost: "github.com",
  githubInsecureHttp: false,
  gcpProject: "",
  gcpServiceAccountEmail: "",
  targetRepos: ["acme/widgets"],
};

describe("SettingsOverlay", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("loads settings and populates the form with them", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByDisplayValue("30s")).toBeInTheDocument();
    expect(screen.getByDisplayValue("2")).toBeInTheDocument();
    expect(screen.getByDisplayValue("gemini-2.5-pro")).toBeInTheDocument();
  });

  it("points to the repos pane instead of editing target repos itself", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByText(/Target repos are managed from the Repos pane/)).toBeInTheDocument();
    expect(screen.queryByText("acme/widgets")).not.toBeInTheDocument();
  });

  it("shows an unconfigured note when settings have never been saved", async () => {
    api.mockResolvedValueOnce({ ...settings, configured: false });
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/Not configured yet/)).toBeInTheDocument();
  });

  it("only includes changed fields in the PUT payload", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={onClose} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    const pollInput = screen.getByLabelText(/Poll interval/);
    await user.clear(pollInput);
    await user.type(pollInput, "60s");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ pollInterval: "60s" }),
    });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("sends an empty payload when nothing changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", { method: "PUT", body: JSON.stringify({}) });
  });

  // bwsalmon/agents#476: the global backlog-order switch.
  it("toggles newestFirst and includes it in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    const checkbox = screen.getByRole("checkbox", { name: /Work through the backlog newest-first/ });
    expect(checkbox).not.toBeChecked();
    await user.click(checkbox);

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ newestFirst: true }),
    });
  });

  it("leaves newestFirst out of the payload when already on and left alone", async () => {
    api.mockResolvedValueOnce({ ...settings, newestFirst: true }).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByRole("checkbox", { name: /Work through the backlog newest-first/ })).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", { method: "PUT", body: JSON.stringify({}) });
  });

  // bwsalmon/agents#534: the deployment-wide default sandbox shape.
  it("sets sandboxCpus/sandboxMemoryMb and includes them in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    const cpusInput = screen.getByLabelText(/Sandbox vCPUs/);
    await user.clear(cpusInput);
    await user.type(cpusInput, "4");
    const memoryInput = screen.getByLabelText(/Sandbox memory/);
    await user.clear(memoryInput);
    await user.type(memoryInput, "8192");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ sandboxCpus: 4, sandboxMemoryMb: 8192 }),
    });
  });

  it("leaves sandboxCpus/sandboxMemoryMb out of the payload when unchanged", async () => {
    api.mockResolvedValueOnce({ ...settings, sandboxCpus: 4, sandboxMemoryMb: 8192 }).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByLabelText(/Sandbox vCPUs/)).toHaveValue(4);
    expect(screen.getByLabelText(/Sandbox memory/)).toHaveValue(8192);
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", { method: "PUT", body: JSON.stringify({}) });
  });

  // bwsalmon/agents#537: the global "hide closed tasks by default" switch.
  it("toggles showClosedByDefault and includes it in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    const checkbox = screen.getByRole("checkbox", { name: /Show closed tasks by default/ });
    expect(checkbox).not.toBeChecked();
    await user.click(checkbox);

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ showClosedByDefault: true }),
    });
  });

  it("leaves showClosedByDefault out of the payload when already on and left alone", async () => {
    api.mockResolvedValueOnce({ ...settings, showClosedByDefault: true }).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByRole("checkbox", { name: /Show closed tasks by default/ })).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", { method: "PUT", body: JSON.stringify({}) });
  });

  it("reports the error and does not close on a failed save", async () => {
    api.mockResolvedValueOnce(settings).mockRejectedValueOnce(new Error("pollInterval must be positive"));
    const showError = vi.fn();
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={onClose} showError={showError} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "pollInterval must be positive" }));
    expect(onClose).not.toHaveBeenCalled();
  });

  it("does not show the danger zone when reboot is not enabled", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay config={{ rebootEnabled: false }} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.queryByRole("button", { name: "Reboot host" })).not.toBeInTheDocument();
  });

  it("reboots the host after confirmation", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    render(<SettingsOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(api).toHaveBeenCalledWith("/api/host/reboot", { method: "POST" });
    vi.unstubAllGlobals();
  });

  it("does not reboot when the confirmation is declined", async () => {
    api.mockResolvedValueOnce(settings);
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(false));
    const user = userEvent.setup();
    render(<SettingsOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

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
    api.mockResolvedValueOnce(settings).mockRejectedValueOnce(new TypeError("Failed to fetch"));
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    const showError = vi.fn();
    render(<SettingsOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={showError} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(showError).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("still shows an error for a real reboot failure", async () => {
    api.mockResolvedValueOnce(settings).mockRejectedValueOnce(new Error("reboot is not available"));
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    const showError = vi.fn();
    render(<SettingsOverlay config={{ rebootEnabled: true }} onClose={() => {}} showError={showError} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Reboot host" }));

    expect(showError).toHaveBeenCalledWith(new Error("reboot is not available"));
    vi.unstubAllGlobals();
  });

  it("switches to the Secrets tab and shows its panel", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({ enabled: false });
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("tab", { name: "Secrets" }));

    expect(await screen.findByText(/this UI was not started with a local secrets directory/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Poll interval/)).not.toBeInTheDocument();
  });

  it("switches to the Upgrade tab and shows its panel", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({ enabled: false });
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("tab", { name: "Upgrade" }));

    expect(await screen.findByText(/no -upgrade-src-dir configured/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Poll interval/)).not.toBeInTheDocument();
  });

  // bwsalmon/agents#535: light/dark/auto is a per-browser preference
  // (ThemeModeContext, localStorage), not a deployment-wide setting, so
  // it applies immediately instead of going through the Save/PUT flow
  // the rest of this tab uses.
  describe("appearance", () => {
    afterEach(() => {
      localStorage.clear();
    });

    it("defaults to Auto and applies a choice immediately without saving", async () => {
      api.mockResolvedValueOnce(settings);
      const user = userEvent.setup();
      render(
        <ThemeModeProvider>
          <SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />
        </ThemeModeProvider>,
      );
      await screen.findByDisplayValue("30s");

      expect(screen.getByRole("radio", { name: "Auto" })).toBeChecked();

      await user.click(screen.getByRole("radio", { name: "Dark" }));

      expect(screen.getByRole("radio", { name: "Dark" })).toBeChecked();
      expect(localStorage.getItem("grain.themeMode")).toBe("dark");
      expect(api).not.toHaveBeenCalledWith("/api/settings", expect.objectContaining({ method: "PUT" }));
    });

    it("reflects a previously stored mode", async () => {
      localStorage.setItem("grain.themeMode", "light");
      api.mockResolvedValueOnce(settings);
      render(
        <ThemeModeProvider>
          <SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />
        </ThemeModeProvider>,
      );
      await screen.findByDisplayValue("30s");

      expect(screen.getByRole("radio", { name: "Light" })).toBeChecked();
    });
  });
});
