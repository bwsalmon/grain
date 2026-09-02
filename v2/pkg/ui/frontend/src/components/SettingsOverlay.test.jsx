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
  claudeModel: "claude-sonnet-5",
  maxAgentTurns: 40,
  githubHost: "github.com",
  githubInsecureHttp: false,
  gcpProject: "",
  gcpServiceAccountEmail: "",
  targetRepos: ["acme/widgets"],
  sandboxCpusDefault: 2,
  sandboxMemoryMbDefault: 2048,
};

describe("SettingsOverlay", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("loads settings and populates the form with them", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByDisplayValue("30s")).toBeInTheDocument();
    expect(screen.getByDisplayValue("2")).toBeInTheDocument();
    expect(screen.getByDisplayValue("gemini-2.5-pro")).toBeInTheDocument();
    expect(screen.getByDisplayValue("claude-sonnet-5")).toBeInTheDocument();
  });

  it("points to the repos pane instead of editing target repos itself", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByText(/Target repos are managed from the Repos pane/)).toBeInTheDocument();
    expect(screen.queryByText("acme/widgets")).not.toBeInTheDocument();
  });

  it("shows an unconfigured note when settings have never been saved", async () => {
    api.mockResolvedValueOnce({ ...settings, configured: false });
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/Not configured yet/)).toBeInTheDocument();
  });

  // bwsalmon/grain: the framework a run is driven by and the credential
  // it runs as are the same decision, so the keys live in this pane
  // rather than only in Secrets, seeded from the Settings response the
  // pane already fetched (no second request).
  it("offers a key field per agent framework, marked set or not", async () => {
    api.mockResolvedValueOnce({ ...settings, agentKeysEnabled: true, claudeOAuthTokenSet: true });
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByLabelText("Gemini API key")).toBeInTheDocument();
    expect(screen.getByLabelText("Claude Code OAuth token")).toBeInTheDocument();
    expect(screen.getByText("set")).toBeInTheDocument();
    expect(screen.getByText("not set")).toBeInTheDocument();
    expect(api).toHaveBeenCalledTimes(1);
  });

  it("only includes changed fields in the PUT payload", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={onClose} showError={() => {}} />);
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
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", { method: "PUT", body: JSON.stringify({}) });
  });

  // bwsalmon/agents#476: the global backlog-order switch.
  it("toggles newestFirst and includes it in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
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
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByRole("checkbox", { name: /Work through the backlog newest-first/ })).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", { method: "PUT", body: JSON.stringify({}) });
  });

  // bwsalmon/agents#534: the deployment-wide default sandbox shape.
  it("sets sandboxCpus/sandboxMemoryMb and includes them in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
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
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByLabelText(/Sandbox vCPUs/)).toHaveValue(4);
    expect(screen.getByLabelText(/Sandbox memory/)).toHaveValue(8192);
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", { method: "PUT", body: JSON.stringify({}) });
  });

  // bwsalmon/agents#610: an unset override shows kontur's own default as a
  // placeholder -- fainter than a real value -- rather than a literal 0 that
  // reads as a deliberately zeroed-out sandbox.
  it("shows kontur's default shape as a placeholder, not a literal 0, when unset", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    const cpusInput = screen.getByLabelText(/Sandbox vCPUs/);
    const memoryInput = screen.getByLabelText(/Sandbox memory/);
    expect(cpusInput).toHaveValue(null);
    expect(cpusInput).toHaveAttribute("placeholder", "2");
    expect(memoryInput).toHaveValue(null);
    expect(memoryInput).toHaveAttribute("placeholder", "2048");
  });

  // bwsalmon/agents#610: clearing a real override back to blank is how an
  // operator returns to the default, so it has to send an explicit 0 rather
  // than being silently skipped as "left alone" the way every other field's
  // empty box is.
  it("sends an explicit 0 when a sandbox shape override is cleared back to blank", async () => {
    api.mockResolvedValueOnce({ ...settings, sandboxCpus: 4, sandboxMemoryMb: 8192 }).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.clear(screen.getByLabelText(/Sandbox vCPUs/));
    await user.clear(screen.getByLabelText(/Sandbox memory/));
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ sandboxCpus: 0, sandboxMemoryMb: 0 }),
    });
  });

  // bwsalmon/agents#537: the global "hide closed tasks by default" switch.
  it("toggles showClosedByDefault and includes it in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
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
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByRole("checkbox", { name: /Show closed tasks by default/ })).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", { method: "PUT", body: JSON.stringify({}) });
  });

  // bwsalmon/agents#609: which agent.Framework a run is driven by.
  it("switches agentFramework to claude and includes it in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByRole("radio", { name: "Antigravity" })).toBeChecked();
    await user.click(screen.getByRole("radio", { name: "Claude" }));

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ agentFramework: "claude" }),
    });
  });

  it("leaves agentFramework out of the payload when already claude and left alone", async () => {
    api.mockResolvedValueOnce({ ...settings, agentFramework: "claude" }).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByRole("radio", { name: "Claude" })).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", { method: "PUT", body: JSON.stringify({}) });
  });

  it("reports the error and does not close on a failed save", async () => {
    api.mockResolvedValueOnce(settings).mockRejectedValueOnce(new Error("pollInterval must be positive"));
    const showError = vi.fn();
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={onClose} showError={showError} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "pollInterval must be positive" }));
    expect(onClose).not.toHaveBeenCalled();
  });

  it("switches to the Capabilities tab and shows its panel", async () => {
    const capabilities = [
      { id: "self-debug", name: "Self debug", description: "Read grain's own source", ready: true },
      {
        id: "gcp-key", name: "GCP key", description: "Mint a GCP key", ready: false,
        missingConfig: ["GCP project", "GCP service account email"],
      },
    ];
    api.mockResolvedValueOnce({ ...settings, capabilities });
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("tab", { name: "Capabilities" }));

    expect(await screen.findByText("Self debug")).toBeInTheDocument();
    expect(screen.getByText("GCP key")).toBeInTheDocument();
    expect(screen.getByText(/Needs: GCP project, GCP service account email/)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Poll interval/)).not.toBeInTheDocument();
  });

  it("switches to the Secrets tab and shows its panel", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({ enabled: false });
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("tab", { name: "Secrets" }));

    expect(await screen.findByText(/this UI was not started with a local secrets directory/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Poll interval/)).not.toBeInTheDocument();
  });

  it("switches to the Upgrade tab and shows its panel", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({ enabled: false });
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
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
          <SettingsOverlay onClose={() => {}} showError={() => {}} />
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
          <SettingsOverlay onClose={() => {}} showError={() => {}} />
        </ThemeModeProvider>,
      );
      await screen.findByDisplayValue("30s");

      expect(screen.getByRole("radio", { name: "Light" })).toBeChecked();
    });
  });
});
