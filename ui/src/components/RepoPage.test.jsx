import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import RepoPage from "./RepoPage.jsx";
import api from "../api.js";
import { loadRepoView, saveRepoView } from "../board.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const tasks = [
  {
    id: "1",
    title: "Fix the widget",
    repo: "acme/widgets",
    state: "queued",
    blocked: false,
    capabilities: [],
  },
  {
    id: "2",
    title: "Ship the widget",
    repo: "acme/widgets",
    state: "running",
    blocked: false,
    capabilities: [],
  },
  {
    id: "3",
    title: "Recall the widget",
    repo: "acme/widgets",
    state: "queued",
    blocked: true,
    blockedBy: ["1"],
    capabilities: [],
  },
  {
    id: "4",
    title: "Ship the gadget",
    repo: "acme/gadgets",
    state: "completed",
    blocked: false,
    capabilities: [],
  },
];

const noCaps = {
  repo: "acme/widgets",
  defaultCapabilities: [],
  deploymentDefaultCapabilities: [],
  effectiveDefaultCapabilities: [],
};

const noPrompt = {
  repo: "acme/widgets",
  promptExtension: "",
  deploymentPromptExtension: "",
  effectivePromptExtension: "",
};

const noSetup = {
  repo: "acme/widgets",
  setupCommand: "",
};

// routeApi answers the four GETs the page issues on mount -- its branch
// list, its capability sets, its prompt extension and its setup command
// -- and leaves anything else to the per-test mockResolvedValueOnce
// queue, so a test only has to say what it is actually about.
function routeApi({
  branches = [],
  caps = noCaps,
  prompt = noPrompt,
  setup = noSetup,
} = {}) {
  api.mockImplementation((path, opts) => {
    if (!opts && /\/branches$/.test(path)) return Promise.resolve(branches);
    if (!opts && /\/capabilities$/.test(path)) return Promise.resolve(caps);
    if (!opts && /\/prompt-extension$/.test(path))
      return Promise.resolve(prompt);
    if (!opts && /\/setup-command$/.test(path)) return Promise.resolve(setup);
    return Promise.resolve({});
  });
}

function renderPage(overrides = {}) {
  const props = {
    repo: "acme/widgets",
    tasks,
    config: null,
    onBack: vi.fn(),
    onNewTask: vi.fn(),
    onOpenReleases: vi.fn(),
    onRefreshConfig: vi.fn(),
    showError: vi.fn(),
    // App hands the page both views and the page picks: here each one
    // is a line saying which it is, so a test can tell them apart.
    children: (view) => (
      <div>{view === "board" ? "task board" : "task list"}</div>
    ),
    ...overrides,
  };
  render(<RepoPage {...props} />);
  return props;
}

describe("RepoPage", () => {
  // The List/Board switch is stored per browser (board.js), so one
  // test's choice would otherwise be the next test's starting view.
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    api.mockReset();
  });

  it("heads the page with the repo, its per-state counts and its total", async () => {
    routeApi();
    renderPage();

    expect(
      screen.getByRole("heading", { name: "acme/widgets" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Queued 2")).toBeInTheDocument();
    expect(screen.getByText("Running 1")).toBeInTheDocument();
    expect(screen.getByText("Blocked 1")).toBeInTheDocument();
    expect(screen.getByText("3 tasks")).toBeInTheDocument();
    // The repo's own tasks are App's scoped TaskList, passed in.
    expect(screen.getByText("task list")).toBeInTheDocument();
    await screen.findByLabelText("Default capabilities");
  });

  it("shows the repo's tasks as a board when the switch is moved, and remembers it", async () => {
    routeApi();
    renderPage();

    await userEvent.click(screen.getByRole("button", { name: "Board" }));
    expect(screen.getByText("task board")).toBeInTheDocument();
    expect(screen.queryByText("task list")).not.toBeInTheDocument();
    expect(loadRepoView()).toBe("board");

    await userEvent.click(screen.getByRole("button", { name: "List" }));
    expect(screen.getByText("task list")).toBeInTheDocument();
    expect(loadRepoView()).toBe("list");
    await screen.findByLabelText("Default capabilities");
  });

  it("opens on the view this browser last left a repo page on", async () => {
    saveRepoView("board");
    routeApi();
    renderPage();

    expect(screen.getByText("task board")).toBeInTheDocument();
    await screen.findByLabelText("Default capabilities");
  });

  // A ToggleButtonGroup reports null when the chosen button is pressed
  // again; there is no third view to fall to.
  it("stays put when the view it is already showing is pressed", async () => {
    routeApi();
    renderPage();

    await userEvent.click(screen.getByRole("button", { name: "List" }));
    expect(screen.getByText("task list")).toBeInTheDocument();
    await screen.findByLabelText("Default capabilities");
  });

  it("loads the repo's branches and capabilities on landing, with no button to press first", async () => {
    routeApi({
      branches: [
        {
          name: "myfeat",
          status: "created",
          createdAt: "2026-01-01T00:00:00Z",
        },
      ],
    });
    renderPage();

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/branches");
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/capabilities");
    expect((await screen.findByText("myfeat")).closest("li")).toHaveTextContent(
      "myfeat -- created",
    );
  });

  // grain/task-176: a name already on the repo is added by adopting the
  // ref that is there rather than by cutting one, and the list says which
  // of the two happened -- the status the reconciler landed on, verbatim.
  it("says a branch was adopted rather than created", async () => {
    routeApi({
      branches: [
        {
          name: "myfeat",
          status: "adopted",
          createdAt: "2026-01-01T00:00:00Z",
        },
      ],
    });
    renderPage();

    expect((await screen.findByText("myfeat")).closest("li")).toHaveTextContent(
      "myfeat -- adopted",
    );
  });

  it("goes back to the repo list", async () => {
    routeApi();
    const onBack = vi.fn();
    const user = userEvent.setup();
    renderPage({ onBack });

    await user.click(screen.getByRole("button", { name: /Repos/ }));

    expect(onBack).toHaveBeenCalled();
  });

  it("files a new task against this repo and opens its releases", async () => {
    routeApi();
    const onNewTask = vi.fn();
    const onOpenReleases = vi.fn();
    const user = userEvent.setup();
    renderPage({ onNewTask, onOpenReleases });

    await user.click(screen.getByRole("button", { name: "New task" }));
    await user.click(screen.getByRole("button", { name: "Releases" }));

    expect(onNewTask).toHaveBeenCalledWith("acme/widgets");
    expect(onOpenReleases).toHaveBeenCalledWith("acme/widgets");
  });

  it("adds a branch and refreshes the branch list on success", async () => {
    routeApi();
    const user = userEvent.setup();
    renderPage();
    await screen.findByLabelText("Default capabilities");

    api.mockImplementationOnce(() =>
      Promise.resolve({
        repo: "acme/widgets",
        name: "myfeat",
        status: "pending",
        createdAt: "2026-01-01T00:00:00Z",
      }),
    );
    api.mockImplementationOnce(() =>
      Promise.resolve([
        {
          repo: "acme/widgets",
          name: "myfeat",
          status: "pending",
          createdAt: "2026-01-01T00:00:00Z",
        },
      ]),
    );

    await user.type(screen.getByPlaceholderText("feature/foo"), "myfeat");
    await user.click(screen.getByRole("button", { name: "Add branch" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/branches", {
      method: "POST",
      body: JSON.stringify({ name: "myfeat" }),
    });
    expect((await screen.findByText("myfeat")).closest("li")).toHaveTextContent(
      "myfeat -- pending",
    );
  });

  it("reports the error when adding a branch fails, without clearing the field", async () => {
    routeApi();
    const showError = vi.fn();
    const user = userEvent.setup();
    renderPage({ showError });
    await screen.findByLabelText("Default capabilities");

    api.mockImplementationOnce(() =>
      Promise.reject(new Error("invalid branch name")),
    );

    await user.type(screen.getByPlaceholderText("feature/foo"), "bad name");
    await user.click(screen.getByRole("button", { name: "Add branch" }));

    expect(showError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "invalid branch name" }),
    );
    expect(screen.getByPlaceholderText("feature/foo")).toHaveValue("bad name");
  });

  // grain/task-24: a repo's own default capability set is edited beside
  // the repo it belongs to rather than on the deployment-wide Settings
  // pane -- and the form says what a task filed against this repo would
  // actually start with, which is the union of both layers.
  it("says what a task filed against this repo starts with", async () => {
    routeApi({
      caps: {
        repo: "acme/widgets",
        defaultCapabilities: ["gcp-key"],
        deploymentDefaultCapabilities: ["gemini-key"],
        effectiveDefaultCapabilities: ["gemini-key", "gcp-key"],
      },
    });
    const config = {
      capabilities: [
        { id: "gcp-key", name: "GCP key" },
        { id: "gemini-key", name: "Gemini key" },
      ],
    };
    renderPage({ config });

    expect(
      await screen.findByText(
        /A task filed against acme\/widgets starts with:/,
      ),
    ).toHaveTextContent("Gemini key, GCP key");
  });

  // Both sets GET reports come back as stored, retired ids included, so
  // that one chosen before a build retired it can still be seen and
  // unticked. What a task starts with is the filtered union, though --
  // (*Client).defaultCapabilities drops a retired id before any grant is
  // written -- so this line must not list one.
  it("leaves a retired id out of what a task filed against the repo starts with", async () => {
    routeApi({
      caps: {
        repo: "acme/widgets",
        defaultCapabilities: ["gcp-key", "scratch-repo"],
        deploymentDefaultCapabilities: ["gemini-key", "old-deployment-key"],
        effectiveDefaultCapabilities: ["gemini-key", "gcp-key"],
      },
    });
    const config = {
      capabilities: [
        { id: "gcp-key", name: "GCP key" },
        { id: "gemini-key", name: "Gemini key" },
      ],
    };
    renderPage({ config });

    const line = await screen.findByText(
      /A task filed against acme\/widgets starts with:/,
    );
    expect(line).toHaveTextContent("Gemini key, GCP key");
    expect(line).not.toHaveTextContent("scratch-repo");
    expect(line).not.toHaveTextContent("old-deployment-key");
  });

  it("says a repo whose only defaults are retired ids starts with nothing", async () => {
    routeApi({
      caps: {
        repo: "acme/widgets",
        defaultCapabilities: ["scratch-repo"],
        deploymentDefaultCapabilities: ["old-deployment-key"],
        effectiveDefaultCapabilities: [],
      },
    });
    const config = { capabilities: [{ id: "gcp-key", name: "GCP key" }] };
    renderPage({ config });

    expect(
      await screen.findByText(
        /A task filed against acme\/widgets starts with:/,
      ),
    ).toHaveTextContent("nothing -- only what whoever files it ticks");
  });

  it("saves the repo's default capabilities and refreshes the config the new-task form seeds from", async () => {
    routeApi();
    const config = {
      capabilities: [
        { id: "gcp-key", name: "GCP key" },
        { id: "gemini-key", name: "Gemini key" },
      ],
    };
    const onRefreshConfig = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage({ config, onRefreshConfig });

    await user.click(await screen.findByLabelText("Default capabilities"));
    await user.click(await screen.findByRole("option", { name: "GCP key" }));
    await user.keyboard("{Escape}");

    api.mockImplementationOnce(() =>
      Promise.resolve({
        repo: "acme/widgets",
        defaultCapabilities: ["gcp-key"],
        deploymentDefaultCapabilities: [],
        effectiveDefaultCapabilities: ["gcp-key"],
      }),
    );
    await user.click(screen.getByRole("button", { name: "Save capabilities" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/capabilities", {
      method: "PUT",
      body: JSON.stringify({ defaultCapabilities: ["gcp-key"] }),
    });
    expect(onRefreshConfig).toHaveBeenCalled();
  });

  // grain/task-43: the per-repo set is reported as stored too, so a
  // capability retired since this repo named it arrives ticked with no
  // row in config.capabilities to untick it -- and PUT rejects the whole
  // set as "unknown capability" every time this form is saved with it
  // still there. Its own row is the only way out.
  it("offers a row for a stored repo default this build no longer lists", async () => {
    routeApi({
      caps: {
        repo: "acme/widgets",
        defaultCapabilities: ["gcp-key", "scratch-repo"],
        deploymentDefaultCapabilities: [],
        effectiveDefaultCapabilities: ["gcp-key"],
      },
    });
    const config = { capabilities: [{ id: "gcp-key", name: "GCP key" }] };
    const user = userEvent.setup();
    renderPage({ config });

    await user.click(await screen.findByLabelText("Default capabilities"));
    const retired = await screen.findByRole("option", { name: /scratch-repo/ });
    expect(retired).toHaveTextContent(
      "No longer offered -- untick to remove it",
    );

    await user.click(retired);
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Save capabilities" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/capabilities", {
      method: "PUT",
      body: JSON.stringify({ defaultCapabilities: ["gcp-key"] }),
    });
  });

  it("reports the error when saving the repo's default capabilities fails", async () => {
    routeApi();
    const config = { capabilities: [{ id: "gcp-key", name: "GCP key" }] };
    const showError = vi.fn();
    const user = userEvent.setup();
    renderPage({ config, showError });
    await screen.findByLabelText("Default capabilities");

    api.mockImplementationOnce(() =>
      Promise.reject(new Error("unknown capability nope")),
    );
    await user.click(screen.getByRole("button", { name: "Save capabilities" }));

    expect(showError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "unknown capability nope" }),
    );
  });

  it("only offers Remove for a repo that is on the allowlist", async () => {
    routeApi();
    renderPage({ config: { targetRepos: ["acme/gadgets"] } });

    expect(
      screen.queryByRole("button", { name: "Remove" }),
    ).not.toBeInTheDocument();
    await screen.findByLabelText("Default capabilities");
  });

  it("removes the repo after confirmation and returns to the list", async () => {
    routeApi();
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const onBack = vi.fn();
    const onRefreshConfig = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage({
      config: { targetRepos: ["acme/widgets"] },
      onBack,
      onRefreshConfig,
    });

    await user.click(screen.getByRole("button", { name: "Remove" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets", {
      method: "DELETE",
    });
    expect(onRefreshConfig).toHaveBeenCalledTimes(1);
    expect(onBack).toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("does not remove the repo when the confirmation is declined", async () => {
    routeApi();
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(false));
    const onBack = vi.fn();
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    renderPage({
      config: { targetRepos: ["acme/widgets"] },
      onBack,
      onRefreshConfig,
    });
    await screen.findByLabelText("Default capabilities");

    await user.click(screen.getByRole("button", { name: "Remove" }));

    expect(api).not.toHaveBeenCalledWith("/api/repos/acme/widgets", {
      method: "DELETE",
    });
    expect(onRefreshConfig).not.toHaveBeenCalled();
    expect(onBack).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  // A repo listed only because it carries defaults of its own has no
  // tasks and no allowlist entry to explain why this page exists at all.
  it("says why a repo with nothing but defaults of its own is known here", async () => {
    routeApi();
    const config = {
      targetRepos: [],
      repoDefaultCapabilities: { "acme/orphan": ["gcp-key"] },
      capabilities: [{ id: "gcp-key", name: "GCP key" }],
    };
    renderPage({ repo: "acme/orphan", config });

    expect(screen.getByText(/known here only because it/)).toBeInTheDocument();
    await screen.findByLabelText("Default capabilities");
  });

  // grain/task-114: a repo's own standing instructions, edited here for
  // the same reason its capabilities are -- on the page of the repo they
  // belong to -- and shown alongside the deployment-wide text they are
  // appended to, which is the thing somebody writing them is writing
  // after.
  it("loads this repo's own prompt extension and the deployment text it is appended to", async () => {
    routeApi({
      prompt: {
        repo: "acme/widgets",
        promptExtension: "Migrations live in db/.",
        deploymentPromptExtension: "Run `make lint` before you push.",
        effectivePromptExtension:
          "Run `make lint` before you push.\n\nMigrations live in db/.",
      },
    });
    renderPage();

    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/prompt-extension",
    );
    expect(
      await screen.findByLabelText(/Prompt extension for acme\/widgets/),
    ).toHaveValue("Migrations live in db/.");
    expect(
      screen.getByText(/Run `make lint` before you push./),
    ).toBeInTheDocument();
  });

  it("says so when the deployment adds nothing of its own", async () => {
    routeApi();
    renderPage();

    await screen.findByLabelText(/Prompt extension for acme\/widgets/);
    expect(
      screen.getByText(/Deployment-wide, set in Settings/),
    ).toHaveTextContent("nothing");
  });

  // The refresh matters as much as the save: config.reposWithPromptExtension
  // is one of the three sources repoRows lists a repo from, so a repo
  // whose standing instructions are the only thing this deployment knows
  // it by is missing from the list page until the config is re-read --
  // and a repo that just had its last ones cleared stays on it.
  it("saves the repo's own prompt extension and refreshes the config the repo list is built from", async () => {
    routeApi();
    const onRefreshConfig = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage({ onRefreshConfig });

    await user.type(
      await screen.findByLabelText(/Prompt extension for acme\/widgets/),
      "Migrations live in db/.",
    );

    api.mockImplementationOnce(() =>
      Promise.resolve({
        repo: "acme/widgets",
        promptExtension: "Migrations live in db/.",
        deploymentPromptExtension: "",
        effectivePromptExtension: "Migrations live in db/.",
      }),
    );
    await user.click(
      screen.getByRole("button", { name: "Save prompt extension" }),
    );

    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/prompt-extension",
      {
        method: "PUT",
        body: JSON.stringify({ promptExtension: "Migrations live in db/." }),
      },
    );
    expect(onRefreshConfig).toHaveBeenCalled();
  });

  it("reports the error when saving the repo's prompt extension fails", async () => {
    routeApi();
    const showError = vi.fn();
    const user = userEvent.setup();
    renderPage({ showError });
    await screen.findByLabelText(/Prompt extension for acme\/widgets/);

    api.mockImplementationOnce(() =>
      Promise.reject(new Error("store is down")),
    );
    await user.click(
      screen.getByRole("button", { name: "Save prompt extension" }),
    );

    expect(showError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "store is down" }),
    );
  });

  // The setup command is a property of the repo's toolchain rather than
  // of any one task, so it belongs on this page for the reason the two
  // forms above it do -- and it is the only one whose effect a run sees
  // before its first turn (grain/task-154).
  it("loads this repo's setup command", async () => {
    routeApi({ setup: { repo: "acme/widgets", setupCommand: "make deps" } });
    renderPage();

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/setup-command");
    expect(
      await screen.findByLabelText(/Setup command for acme\/widgets/),
    ).toHaveValue("make deps");
  });

  // Refreshing the config matters here for the same reason it does for
  // the prompt extension: config.reposWithSetupCommand is one of the
  // sources repoRows lists a repo from, so a repo whose setup command is
  // the only thing this deployment knows it by is missing from the list
  // page until the config is re-read.
  it("saves the repo's setup command and refreshes the config the repo list is built from", async () => {
    routeApi();
    const onRefreshConfig = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage({ onRefreshConfig });

    await user.type(
      await screen.findByLabelText(/Setup command for acme\/widgets/),
      "make deps",
    );

    api.mockImplementationOnce(() =>
      Promise.resolve({ repo: "acme/widgets", setupCommand: "make deps" }),
    );
    await user.click(
      screen.getByRole("button", { name: "Save setup command" }),
    );

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/setup-command", {
      method: "PUT",
      body: JSON.stringify({ setupCommand: "make deps" }),
    });
    expect(onRefreshConfig).toHaveBeenCalled();
  });

  it("reports the error when saving the repo's setup command fails", async () => {
    routeApi();
    const showError = vi.fn();
    const user = userEvent.setup();
    renderPage({ showError });
    await screen.findByLabelText(/Setup command for acme\/widgets/);

    api.mockImplementationOnce(() =>
      Promise.reject(new Error("store is down")),
    );
    await user.click(
      screen.getByRole("button", { name: "Save setup command" }),
    );

    expect(showError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "store is down" }),
    );
  });
});
