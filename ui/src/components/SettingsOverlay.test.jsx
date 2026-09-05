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
  maxWorkers: 2,
  maxMergers: 1,
  geminiModel: "gemini-2.5-pro",
  geminiEffort: "high",
  geminiEfforts: ["low", "medium", "high"],
  claudeModel: "claude-sonnet-5",
  maxAgentTurns: 40,
  githubHost: "github.com",
  githubInsecureHttp: false,
  gcpProject: "",
  gcpServiceAccountEmail: "",
  targetRepos: ["acme/widgets"],
  sandboxCpusDefault: 2,
  sandboxMemoryMbDefault: 8192,
  sandboxDiskGbDefault: 30,
};

describe("SettingsOverlay", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("loads settings and populates the General tab with them", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByDisplayValue("30s")).toBeInTheDocument();
    expect(screen.getByLabelText(/Max worker agents/)).toHaveValue(2);
    expect(screen.getByLabelText(/Max merge agents/)).toHaveValue(1);
  });

  // grain/task-115: a full-height pane beside the sidebar, not a
  // centered dialog -- with the tab strip in the pane's fixed header, so
  // switching tabs never means scrolling back up past a long tab's
  // fields to find them.
  it("fills the pane beside the sidebar, with its tabs in the fixed header", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(document.querySelector(".MuiDialog-paper")).toHaveClass(
      "MuiDialog-paperFullScreen",
    );
    const head = document.querySelector(".overlay-pane-header");
    expect(head).toContainElement(
      screen.getByRole("tab", { name: "Capabilities" }),
    );
    expect(head).toContainElement(
      screen.getByRole("heading", { name: "Settings" }),
    );
    expect(
      document.querySelector(".overlay-pane .pane-form"),
    ).toBeInTheDocument();
  });

  it("populates the Agents tab with them", async () => {
    api.mockResolvedValueOnce(settings);
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("tab", { name: "Agents" }));

    expect(screen.getByDisplayValue("gemini-2.5-pro")).toBeInTheDocument();
    expect(screen.getByDisplayValue("claude-sonnet-5")).toBeInTheDocument();
    expect(screen.getByDisplayValue("40")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Antigravity" })).toBeChecked();
  });

  // agy is given a model and a reasoning effort as two values, so the
  // effort is a field of its own -- offered from the vocabulary the
  // server reports rather than typed, since anything else fails the run
  // it reaches before that run starts.
  it("offers the reasoning effort agy takes beside the Gemini model", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Agents" }));

    const picker = screen.getByLabelText(/Gemini reasoning effort/);
    expect(picker).toHaveTextContent("high");
    await user.click(picker);
    await user.click(screen.getByRole("option", { name: "low" }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenLastCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ geminiEffort: "low" }),
    });
  });

  it("keeps agent framework, model and keys off the General tab", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.queryByLabelText(/Gemini model/)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("radio", { name: "Antigravity" }),
    ).not.toBeInTheDocument();
  });

  it("points to the repos pane instead of editing target repos itself", async () => {
    api.mockResolvedValueOnce(settings);
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(
      screen.getByText(/Target repos are managed from the Repos pane/),
    ).toBeInTheDocument();
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
    api.mockResolvedValueOnce({
      ...settings,
      agentKeysEnabled: true,
      claudeOAuthTokenSet: true,
    });
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Agents" }));

    expect(screen.getByLabelText("Gemini API key")).toBeInTheDocument();
    expect(
      screen.getByLabelText("Claude Code OAuth token"),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("OpenAI API key")).toBeInTheDocument();
    expect(screen.getByText("set")).toBeInTheDocument();
    // One chip per framework with no key stored: two of the three here.
    expect(screen.getAllByText("not set")).toHaveLength(2);
    // Nothing is fetched for the chips themselves. The one other request
    // this tab makes is the model picker's own catalog (grain/task-365,
    // GeminiModelField), which is a subprocess on the daemon and so is
    // deliberately not folded into the Settings response.
    expect(api.mock.calls.map((call) => call[0])).toEqual([
      "/api/settings",
      "/api/agent-models",
    ]);
  });

  it("only includes changed fields in the PUT payload", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    const pollInput = screen.getByLabelText(/Poll interval/);
    await user.clear(pollInput);
    await user.type(pollInput, "60s");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ pollInterval: "60s" }),
    });
  });

  // grain/task-102: Save saves and leaves. It briefly stayed open so a
  // second tab could be saved without a reopen, but a tab only submits
  // the fields it owns, so a saved pane and an unsaved one looked alike
  // with nothing left to do in either.
  it("closes the pane after a successful save", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={onClose} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(onClose).toHaveBeenCalled();
  });

  // Every tab's Save closes it, not just General's -- each is the same
  // save() behind its own form.
  it("closes the pane after a save from a tab other than General", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={onClose} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Sandbox" }));

    const cpusInput = screen.getByLabelText(/Sandbox vCPUs/);
    await user.clear(cpusInput);
    await user.type(cpusInput, "4");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ sandboxCpus: 4 }),
    });
    expect(onClose).toHaveBeenCalled();
  });

  // Half of what this pane writes is read back out of /api/config, which
  // App fetches once at mount: the target repo allowlist the repos page
  // lists from, the default capabilities and task defaults the new-task
  // form starts from, and the deployment's own prompt extension. Without
  // this, all of them kept answering with what the page was told at load
  // until it was reloaded.
  it("refreshes the app's config after a successful save", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const onSaved = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    render(
      <SettingsOverlay
        onClose={() => {}}
        onSaved={onSaved}
        showError={() => {}}
      />,
    );
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(onSaved).toHaveBeenCalled();
  });

  // A save that failed wrote nothing, so there is nothing to re-read --
  // and the error the pane reports has to be the save's own.
  it("does not refresh the config when the save failed", async () => {
    api
      .mockResolvedValueOnce(settings)
      .mockRejectedValueOnce(new Error("store is down"));
    const onSaved = vi.fn();
    const showError = vi.fn();
    const user = userEvent.setup();
    render(
      <SettingsOverlay
        onClose={() => {}}
        onSaved={onSaved}
        showError={showError}
      />,
    );
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(onSaved).not.toHaveBeenCalled();
    expect(showError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "store is down" }),
    );
  });

  it("sends an empty payload when nothing changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({}),
    });
  });

  // bwsalmon/agents#476: the global backlog-order switch.
  it("toggles newestFirst and includes it in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    const checkbox = screen.getByRole("checkbox", {
      name: /Work through the backlog newest-first/,
    });
    expect(checkbox).not.toBeChecked();
    await user.click(checkbox);

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ newestFirst: true }),
    });
  });

  it("leaves newestFirst out of the payload when already on and left alone", async () => {
    api
      .mockResolvedValueOnce({ ...settings, newestFirst: true })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(
      screen.getByRole("checkbox", {
        name: /Work through the backlog newest-first/,
      }),
    ).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({}),
    });
  });

  // bwsalmon/agents#534: the deployment-wide default sandbox shape.
  it("sets sandboxCpus/sandboxMemoryMb and includes them in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Sandbox" }));

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
    api
      .mockResolvedValueOnce({
        ...settings,
        sandboxCpus: 4,
        sandboxMemoryMb: 8192,
      })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Sandbox" }));

    expect(screen.getByLabelText(/Sandbox vCPUs/)).toHaveValue(4);
    expect(screen.getByLabelText(/Sandbox memory/)).toHaveValue(8192);
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({}),
    });
  });

  // grain/task-41: the same treatment for the third dimension of that
  // shape -- including the faint placeholder default it used to lack,
  // back when an unset disk meant "however large the guest image behind
  // it is" rather than a size grain names and passes itself
  // (sandboxDiskGbDefault).
  it("sets sandboxDiskGb, showing grain's own default as its placeholder", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Sandbox" }));

    const diskInput = screen.getByLabelText(/Sandbox disk/);
    expect(diskInput).toHaveValue(null);
    expect(diskInput).toHaveAttribute("placeholder", "30");
    await user.type(diskInput, "40");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ sandboxDiskGb: 40 }),
    });
  });

  it("sends an explicit 0 when sandboxDiskGb is cleared back to blank", async () => {
    api
      .mockResolvedValueOnce({ ...settings, sandboxDiskGb: 40 })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Sandbox" }));

    expect(screen.getByLabelText(/Sandbox disk/)).toHaveValue(40);
    await user.clear(screen.getByLabelText(/Sandbox disk/));
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ sandboxDiskGb: 0 }),
    });
  });

  // bwsalmon/agents#610: an unset override shows grain's own default as a
  // placeholder -- fainter than a real value -- rather than a literal 0 that
  // reads as a deliberately zeroed-out sandbox.
  it("shows grain's default shape as a placeholder, not a literal 0, when unset", async () => {
    api.mockResolvedValueOnce(settings);
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Sandbox" }));

    const cpusInput = screen.getByLabelText(/Sandbox vCPUs/);
    const memoryInput = screen.getByLabelText(/Sandbox memory/);
    expect(cpusInput).toHaveValue(null);
    expect(cpusInput).toHaveAttribute("placeholder", "2");
    expect(memoryInput).toHaveValue(null);
    expect(memoryInput).toHaveAttribute("placeholder", "8192");
  });

  // bwsalmon/agents#610: clearing a real override back to blank is how an
  // operator returns to the default, so it has to send an explicit 0 rather
  // than being silently skipped as "left alone" the way every other field's
  // empty box is.
  it("sends an explicit 0 when a sandbox shape override is cleared back to blank", async () => {
    api
      .mockResolvedValueOnce({
        ...settings,
        sandboxCpus: 4,
        sandboxMemoryMb: 8192,
      })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Sandbox" }));

    await user.clear(screen.getByLabelText(/Sandbox vCPUs/));
    await user.clear(screen.getByLabelText(/Sandbox memory/));
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ sandboxCpus: 0, sandboxMemoryMb: 0 }),
    });
  });

  // grain/task-69: naming the deployment, so the sidebar and the browser
  // tab can say which one this is.
  it("sends environmentName when the deployment is given a name", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.type(screen.getByLabelText(/Environment name/), "staging");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ environmentName: "staging" }),
    });
  });

  // Clearing the box is a real change, not "leave it alone": unnaming a
  // deployment has to be sendable, so "" goes in the payload.
  it("sends an empty environmentName when a configured name is cleared", async () => {
    api
      .mockResolvedValueOnce({ ...settings, environmentName: "staging" })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.clear(screen.getByLabelText(/Environment name/));
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ environmentName: "" }),
    });
  });

  // grain/task-368: the deployment's own clock. It is a deployment
  // setting rather than a per-browser one because the daemon reads it
  // too -- it decides the hour a wall-clock schedule fires -- so the
  // selector is here beside the deployment's name.
  it("sends the chosen time zone", async () => {
    api
      .mockResolvedValueOnce({ ...settings, timeZone: "America/Los_Angeles" })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    const field = screen.getByLabelText(/Time zone/);
    expect(field).toHaveValue("America/Los_Angeles");
    await user.selectOptions(field, "Europe/Berlin");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ timeZone: "Europe/Berlin" }),
    });
  });

  // An unchanged zone stays out of the payload, the same as every other
  // field on this form -- including the case that used to send one by
  // accident: a native <select> whose value matches no option selects
  // the first one, so a deployment reporting no zone at all must still
  // have something for "" to land on.
  it("leaves the time zone out of the payload when it is not touched", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(screen.getByLabelText(/Time zone/)).toHaveValue("");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({}),
    });
  });

  // bwsalmon/agents#537: the global "hide closed tasks by default" switch.
  it("toggles showClosedByDefault and includes it in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    const checkbox = screen.getByRole("checkbox", {
      name: /Show closed tasks by default/,
    });
    expect(checkbox).not.toBeChecked();
    await user.click(checkbox);

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ showClosedByDefault: true }),
    });
  });

  it("leaves showClosedByDefault out of the payload when already on and left alone", async () => {
    api
      .mockResolvedValueOnce({ ...settings, showClosedByDefault: true })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    expect(
      screen.getByRole("checkbox", { name: /Show closed tasks by default/ }),
    ).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({}),
    });
  });

  // bwsalmon/agents#609: which agent.Framework a run is driven by.
  it("switches agentFramework to claude and includes it in the payload only when changed", async () => {
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Agents" }));

    expect(screen.getByRole("radio", { name: "Antigravity" })).toBeChecked();
    await user.click(screen.getByRole("radio", { name: "Claude" }));

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ agentFramework: "claude" }),
    });
  });

  it("leaves agentFramework out of the payload when already claude and left alone", async () => {
    api
      .mockResolvedValueOnce({ ...settings, agentFramework: "claude" })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Agents" }));

    expect(screen.getByRole("radio", { name: "Claude" })).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({}),
    });
  });

  it("reports the error and does not close on a failed save", async () => {
    api
      .mockResolvedValueOnce(settings)
      .mockRejectedValueOnce(new Error("pollInterval must be positive"));
    const showError = vi.fn();
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={onClose} showError={showError} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(showError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "pollInterval must be positive" }),
    );
    expect(onClose).not.toHaveBeenCalled();
  });

  it("switches to the GitHub tab and shows its fields", async () => {
    // The tab's own second request: the named GitHub tokens listed under
    // the host fields (GitHubTokensSection, grain/task-137).
    api.mockResolvedValueOnce(settings).mockResolvedValueOnce({
      enabled: true,
      dir: "/data/secrets/github",
      tokens: [{ name: "bot", default: true, patterns: ["*"], present: true }],
      restartRequired: false,
    });
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("tab", { name: "GitHub" }));

    expect(screen.getByDisplayValue("github.com")).toBeInTheDocument();
    expect(screen.queryByLabelText(/Poll interval/)).not.toBeInTheDocument();
    expect(api).toHaveBeenCalledWith("/api/github-tokens");
    expect(await screen.findByText("Named tokens")).toBeInTheDocument();
    expect(await screen.findByText("deployment default")).toBeInTheDocument();
  });

  it("switches to the Capabilities tab, offers the GCP fields and shows the panel", async () => {
    const capabilities = [
      {
        id: "self-debug",
        name: "Self debug",
        description: "Read grain's own source",
        ready: true,
      },
      {
        id: "gcp-key",
        name: "GCP key",
        description: "Mint a GCP key",
        ready: false,
        missingConfig: ["GCP project", "GCP service account email"],
      },
    ];
    // The tab's own second request: "Other secrets" at its foot lists
    // whatever nothing above claims (grain/task-110).
    api
      .mockResolvedValueOnce({
        ...settings,
        capabilities,
        gcpProject: "acme-proj",
      })
      .mockResolvedValueOnce({ enabled: false });
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("tab", { name: "Capabilities" }));

    expect(screen.getByDisplayValue("acme-proj")).toBeInTheDocument();
    expect(await screen.findByText("Self debug")).toBeInTheDocument();
    expect(screen.getByText("GCP key")).toBeInTheDocument();
    expect(
      screen.getByText(/Needs: GCP project, GCP service account email/),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText(/Poll interval/)).not.toBeInTheDocument();
  });

  it("only includes changed GCP fields in the Capabilities tab's own payload", async () => {
    api
      .mockResolvedValueOnce(settings)
      .mockResolvedValueOnce({ enabled: false })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Capabilities" }));

    await user.type(screen.getByLabelText(/GCP project/), "acme-proj");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ gcpProject: "acme-proj" }),
    });
  });

  // grain/task-14: which capabilities every new task is filed
  // holding is a deployment setting, chosen on the same tab that reports
  // whether each one is ready. Only grantable ones are offered -- a
  // default no task could be granted by hand would fail at every filing.
  it("picks default capabilities on the Capabilities tab and sends the whole set", async () => {
    const capabilities = [
      {
        id: "gcp-key",
        name: "GCP key",
        description: "Mint a GCP key",
        ready: true,
        grantable: true,
      },
      {
        id: "gemini-key",
        name: "Gemini key",
        description: "Mint a Gemini key",
        ready: true,
        grantable: true,
      },
      {
        id: "retired",
        name: "Retired",
        description: "No picker row",
        ready: true,
        grantable: false,
      },
    ];
    api
      .mockResolvedValueOnce({ ...settings, capabilities })
      .mockResolvedValueOnce({ enabled: false })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Capabilities" }));

    await user.click(screen.getByLabelText("Default capabilities"));
    expect(
      screen.queryByRole("option", { name: "Retired" }),
    ).not.toBeInTheDocument();
    await user.click(await screen.findByRole("option", { name: "GCP key" }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ defaultCapabilities: ["gcp-key"] }),
    });
  });

  // grain/task-43: settings.defaultCapabilities is reported as stored,
  // retired ids included, so an id whose row this build has dropped
  // arrives selected with nothing in the listing to untick it. Without a
  // row of its own it sticks in the selection and every later save of
  // this tab sends it back, which UpdateSettings refuses as "unknown
  // capability" -- a pane nobody can save. The extra row is what clears
  // it.
  it("offers a row for a stored default capability this build no longer lists", async () => {
    const capabilities = [
      {
        id: "gcp-key",
        name: "GCP key",
        description: "Mint a GCP key",
        ready: true,
        grantable: true,
      },
    ];
    api
      .mockResolvedValueOnce({
        ...settings,
        capabilities,
        defaultCapabilities: ["gcp-key", "scratch-repo"],
      })
      .mockResolvedValueOnce({ enabled: false })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Capabilities" }));

    await user.click(screen.getByLabelText("Default capabilities"));
    const row = await screen.findByRole("option", { name: /scratch-repo/ });
    expect(row).toHaveTextContent("No longer offered -- untick to remove it");

    await user.click(row);
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ defaultCapabilities: ["gcp-key"] }),
    });
  });

  it("leaves default capabilities out of the payload when they are not touched", async () => {
    const capabilities = [
      {
        id: "gcp-key",
        name: "GCP key",
        description: "Mint a GCP key",
        ready: true,
        grantable: true,
      },
    ];
    api
      .mockResolvedValueOnce({
        ...settings,
        capabilities,
        defaultCapabilities: ["gcp-key"],
      })
      .mockResolvedValueOnce({ enabled: false })
      .mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");
    await user.click(screen.getByRole("tab", { name: "Capabilities" }));

    await user.type(screen.getByLabelText(/GCP project/), "acme-proj");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/settings", {
      method: "PUT",
      body: JSON.stringify({ gcpProject: "acme-proj" }),
    });
  });

  // grain/task-110: Secrets is not a tab any more. Each secret is set
  // where it is used -- the agent credentials on Agents, a capability's
  // own beside it on Capabilities -- and what is left over is listed at
  // the foot of that same Capabilities tab.
  describe("secrets, where they are used", () => {
    const capabilities = [
      {
        id: "gcp-key",
        name: "GCP key",
        description: "Mint a GCP key",
        ready: false,
        grantable: true,
        missingSecrets: ["gcp-key-minter"],
        secrets: [
          {
            name: "gcp-key-minter",
            secret: "gcp-key-minter",
            key: "value",
            set: false,
          },
        ],
      },
    ];

    it("has no Secrets tab of its own", async () => {
      api.mockResolvedValueOnce(settings);
      render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
      await screen.findByDisplayValue("30s");

      expect(
        screen.queryByRole("tab", { name: "Secrets" }),
      ).not.toBeInTheDocument();
    });

    it("sets a capability's own secret from the Capabilities tab, then re-reads settings", async () => {
      api
        .mockResolvedValueOnce({ ...settings, capabilities })
        .mockResolvedValueOnce({ enabled: true, secrets: [] })
        .mockResolvedValueOnce({})
        .mockResolvedValueOnce({
          ...settings,
          capabilities: [{ ...capabilities[0], ready: true }],
        });
      const user = userEvent.setup();
      render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
      await screen.findByDisplayValue("30s");
      await user.click(screen.getByRole("tab", { name: "Capabilities" }));

      await user.type(await screen.findByLabelText("gcp-key-minter"), "a-key");
      await user.click(screen.getByRole("button", { name: "Set" }));

      expect(api).toHaveBeenCalledWith("/api/secrets/gcp-key-minter/value", {
        method: "PUT",
        body: JSON.stringify({ value: "a-key" }),
      });
      // The write moves the capability's readiness, which only a fresh
      // GET reports -- the mutation's own reply is the secrets listing.
      expect(await screen.findByText("Ready")).toBeInTheDocument();
    });

    it("lists a secret nothing on the pane claims, and leaves out the ones something does", async () => {
      api
        .mockResolvedValueOnce({ ...settings, capabilities })
        .mockResolvedValueOnce({
          enabled: true,
          secrets: [
            { name: "gcp-key-minter", keys: ["value"] },
            { name: "gemini-api-key", keys: ["value"] },
            { name: "buildkite", keys: ["token"] },
          ],
        });
      const user = userEvent.setup();
      render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
      await screen.findByDisplayValue("30s");

      await user.click(screen.getByRole("tab", { name: "Capabilities" }));

      expect(await screen.findByText("Other secrets")).toBeInTheDocument();
      expect(screen.getByText("buildkite")).toBeInTheDocument();
      expect(screen.queryByText("gemini-api-key")).not.toBeInTheDocument();
      // Asserted through the per-key delete control, which only the
      // "Other secrets" list has: the minter's name appears above too,
      // on the gcp-key field that owns it.
      expect(screen.getByTitle("delete buildkite/token")).toBeInTheDocument();
      expect(
        screen.queryByTitle("delete gcp-key-minter/value"),
      ).not.toBeInTheDocument();
    });
  });

  it("switches to the Upgrade tab and shows its panel", async () => {
    api
      .mockResolvedValueOnce(settings)
      .mockResolvedValueOnce({ enabled: false });
    const user = userEvent.setup();
    render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByDisplayValue("30s");

    await user.click(screen.getByRole("tab", { name: "Upgrade" }));

    expect(
      await screen.findByText(/no -upgrade-src-dir configured/i),
    ).toBeInTheDocument();
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
      expect(api).not.toHaveBeenCalledWith(
        "/api/settings",
        expect.objectContaining({ method: "PUT" }),
      );
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
  // Almost every setting here reaches the running daemon within a poll
  // interval; the handful that cannot come back as restartRequired, and
  // the ones already changed and waiting on a restart as pendingRestart.
  describe("settings that need a restart", () => {
    const restartRequired = ["githubHost", "githubInsecureHttp"];

    it("annotates a restart-only field before anything has been changed", async () => {
      api.mockResolvedValueOnce({
        ...settings,
        restartRequired,
        pendingRestart: [],
      });
      const user = userEvent.setup();
      render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
      await screen.findByDisplayValue("30s");

      await user.click(screen.getByRole("tab", { name: "GitHub" }));

      expect(screen.getAllByText("needs restart").length).toBe(2);
      expect(
        screen.getAllByText(/Takes effect when the daemon restarts/).length,
      ).toBe(2);
      expect(
        screen.queryByText(/Saved, but not applied yet/),
      ).not.toBeInTheDocument();
    });

    it("says which settings are saved but not applied yet", async () => {
      api.mockResolvedValueOnce({
        ...settings,
        restartRequired,
        pendingRestart: ["githubHost"],
      });
      const user = userEvent.setup();
      render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
      await screen.findByDisplayValue("30s");

      // The banner is on the overlay itself, so it is visible from
      // whichever tab happens to be open.
      expect(
        screen.getByText(/Saved, but not applied yet: GitHub host/),
      ).toBeInTheDocument();

      await user.click(screen.getByRole("tab", { name: "GitHub" }));

      expect(screen.getByText("restart to apply")).toBeInTheDocument();
      expect(screen.getByText(/Changed, but not applied/)).toBeInTheDocument();
      // The other restart-only field has not been changed, so it keeps
      // the plain annotation rather than the warning.
      expect(screen.getByText("needs restart")).toBeInTheDocument();
    });

    it("annotates nothing when the deployment reports no restart-only settings", async () => {
      api.mockResolvedValueOnce({
        ...settings,
        restartRequired: [],
        pendingRestart: [],
      });
      const user = userEvent.setup();
      render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
      await screen.findByDisplayValue("30s");

      await user.click(screen.getByRole("tab", { name: "GitHub" }));

      expect(screen.queryByText("needs restart")).not.toBeInTheDocument();
      expect(
        screen.queryByText(/Takes effect when the daemon restarts/),
      ).not.toBeInTheDocument();
    });
  });

  // The deployment-wide layer of the prompt extension (grain/task-114),
  // on the Agents tab because it is about what the agent is told.
  describe("prompt extension", () => {
    it("populates the box with what the deployment already says", async () => {
      api.mockResolvedValueOnce({
        ...settings,
        promptExtension: "Run `make lint` before you push.",
      });
      const user = userEvent.setup();
      render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
      await screen.findByDisplayValue("30s");

      await user.click(screen.getByRole("tab", { name: "Agents" }));

      expect(
        screen.getByLabelText(/Standing instructions for every run/),
      ).toHaveValue("Run `make lint` before you push.");
    });

    it("sends the edited text with the Agents tab's own save", async () => {
      api.mockResolvedValueOnce(settings).mockResolvedValueOnce({});
      const user = userEvent.setup();
      render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
      await screen.findByDisplayValue("30s");
      await user.click(screen.getByRole("tab", { name: "Agents" }));

      await user.type(
        screen.getByLabelText(/Standing instructions for every run/),
        "Run `make lint`.",
      );
      await user.click(screen.getByRole("button", { name: "Save" }));

      expect(api).toHaveBeenCalledWith("/api/settings", {
        method: "PUT",
        body: JSON.stringify({ promptExtension: "Run `make lint`." }),
      });
    });

    // Cleared back to blank is a deliberate "tell runs nothing extra",
    // not "leave it alone" -- so it has to reach the PUT as "".
    it("sends an empty string when the box is cleared", async () => {
      api
        .mockResolvedValueOnce({
          ...settings,
          promptExtension: "Run `make lint`.",
        })
        .mockResolvedValueOnce({});
      const user = userEvent.setup();
      render(<SettingsOverlay onClose={() => {}} showError={() => {}} />);
      await screen.findByDisplayValue("30s");
      await user.click(screen.getByRole("tab", { name: "Agents" }));

      await user.clear(
        screen.getByLabelText(/Standing instructions for every run/),
      );
      await user.click(screen.getByRole("button", { name: "Save" }));

      expect(api).toHaveBeenCalledWith("/api/settings", {
        method: "PUT",
        body: JSON.stringify({ promptExtension: "" }),
      });
    });
  });
});
