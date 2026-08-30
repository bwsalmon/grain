import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import SettingsOverlay from "./SettingsOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const settings = {
  configured: true,
  pollInterval: "30s",
  slots: ["a", "b"],
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
    expect(screen.getByDisplayValue("a, b")).toBeInTheDocument();
    expect(screen.getByDisplayValue("gemini-2.5-pro")).toBeInTheDocument();
    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
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

  it("adds a target repo via the Add button", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.type(screen.getByPlaceholderText("owner/repo"), "acme/gadgets");
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(screen.getByText("acme/gadgets")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ targetRepos: ["acme/widgets", "acme/gadgets"] }),
    });
  });

  it("adds a target repo when Enter is pressed in the input", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.type(screen.getByPlaceholderText("owner/repo"), "acme/gadgets{enter}");
    expect(screen.getByText("acme/gadgets")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ targetRepos: ["acme/widgets", "acme/gadgets"] }),
    });
  });

  it("removes a target repo via its delete button", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByTitle("remove acme/widgets"));
    expect(screen.queryByText("acme/widgets")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ targetRepos: [] }),
    });
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
});
