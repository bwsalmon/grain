import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import RepoList from "./RepoList.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const tasks = [
  { id: "1", title: "Fix the widget", repo: "acme/widgets", state: "queued", blocked: false, capabilities: [] },
  { id: "2", title: "Ship the widget", repo: "acme/widgets", state: "running", blocked: false, capabilities: [] },
  { id: "3", title: "Recall the widget", repo: "acme/widgets", state: "queued", blocked: true, blockedBy: ["1"], capabilities: [] },
  { id: "4", title: "Ship the gadget", repo: "acme/gadgets", state: "completed", blocked: false, capabilities: [] },
  { id: "5", title: "Untargeted proposal", repo: "", state: "proposed", blocked: false, capabilities: [] },
];

function renderList(overrides = {}) {
  const props = {
    tasks,
    config: null,
    onOpenRepo: vi.fn(),
    onOpenReleases: vi.fn(),
    onOpenTask: vi.fn(),
    onNewTask: vi.fn(),
    onRefreshConfig: vi.fn(),
    showError: vi.fn(),
    ...overrides,
  };
  render(<RepoList {...props} />);
  return props;
}

describe("RepoList", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists one row per repo with per-state counts and a total", () => {
    renderList();

    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("acme/gadgets")).toBeInTheDocument();
    expect(screen.getByText("Queued 2")).toBeInTheDocument();
    expect(screen.getByText("Running 1")).toBeInTheDocument();
    expect(screen.getByText("Blocked 1")).toBeInTheDocument();
    expect(screen.getByText("3 tasks")).toBeInTheDocument();
    expect(screen.getByText("1 task")).toBeInTheDocument();
  });

  it("omits tasks with no target repo", () => {
    renderList();
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
  });

  it("also lists a targetRepos entry that has no tasks yet", () => {
    const config = { targetRepos: ["acme/widgets", "acme/newrepo"] };
    renderList({ config });

    expect(screen.getAllByRole("listitem")).toHaveLength(3);
    expect(screen.getByText("acme/newrepo")).toBeInTheDocument();
    expect(screen.getByText("0 tasks")).toBeInTheDocument();
  });

  it("filters the list by repo name", async () => {
    const user = userEvent.setup();
    renderList();

    await user.type(screen.getByPlaceholderText("Search repos…"), "gadgets");

    expect(screen.getByText("acme/gadgets")).toBeInTheDocument();
    expect(screen.queryByText("acme/widgets")).not.toBeInTheDocument();
  });

  it("shows a message when a search matches no repos", async () => {
    const user = userEvent.setup();
    renderList();

    await user.type(screen.getByPlaceholderText("Search repos…"), "nope");

    expect(screen.getByText("No repos match your search.")).toBeInTheDocument();
  });

  it("calls onOpenRepo when a row is clicked", async () => {
    const onOpenRepo = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenRepo });

    await user.click(screen.getByText("acme/gadgets"));

    expect(onOpenRepo).toHaveBeenCalledWith("acme/gadgets");
  });

  it("calls onOpenReleases, not onOpenRepo, when a row's Releases button is clicked", async () => {
    const onOpenRepo = vi.fn();
    const onOpenReleases = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenRepo, onOpenReleases });

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Releases" }));

    expect(onOpenReleases).toHaveBeenCalledWith("acme/gadgets");
    expect(onOpenRepo).not.toHaveBeenCalled();
  });

  it("shows an empty message when there are no known repos", () => {
    renderList({ tasks: [] });
    expect(screen.getByText("No repos yet -- add one above, or file a task with a target repo.")).toBeInTheDocument();
  });

  it("adds a repo and refreshes config on success", async () => {
    api.mockResolvedValueOnce({});
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    renderList({ tasks: [], onRefreshConfig });

    await user.type(screen.getByPlaceholderText("owner/name"), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    expect(api).toHaveBeenCalledWith("/api/repos", { method: "POST", body: JSON.stringify({ repo: "acme/widgets" }) });
    expect(onRefreshConfig).toHaveBeenCalledTimes(1);
  });

  it("reports the error and does not refresh config when adding fails", async () => {
    api.mockRejectedValueOnce(new Error("repo must be owner/name"));
    const onRefreshConfig = vi.fn();
    const showError = vi.fn();
    const user = userEvent.setup();
    renderList({ tasks: [], onRefreshConfig, showError });

    await user.type(screen.getByPlaceholderText("owner/name"), "not-a-repo");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "repo must be owner/name" }));
    expect(onRefreshConfig).not.toHaveBeenCalled();
  });

  it("only offers Remove on a row that is in config.targetRepos", () => {
    const config = { targetRepos: ["acme/widgets"] };
    renderList({ config });

    const widgetsRow = screen.getByText("acme/widgets").closest("li");
    const gadgetsRow = screen.getByText("acme/gadgets").closest("li");
    expect(within(widgetsRow).getByRole("button", { name: "Remove" })).toBeInTheDocument();
    expect(within(gadgetsRow).queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
  });

  it("removes a repo after confirmation, without also opening it", async () => {
    api.mockResolvedValueOnce({});
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const onOpenRepo = vi.fn();
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    const config = { targetRepos: ["acme/widgets"] };
    renderList({ config, onOpenRepo, onRefreshConfig });

    const row = screen.getByText("acme/widgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Remove" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets", { method: "DELETE" });
    expect(onRefreshConfig).toHaveBeenCalledTimes(1);
    expect(onOpenRepo).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("does not remove a repo when the confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(false));
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    const config = { targetRepos: ["acme/widgets"] };
    renderList({ config, onRefreshConfig });

    const row = screen.getByText("acme/widgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Remove" }));

    expect(api).not.toHaveBeenCalled();
    expect(onRefreshConfig).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("folds a repo's tasks out of view until the chevron is clicked, without opening the repo", async () => {
    const onOpenRepo = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenRepo });

    expect(screen.queryByText("Fix the widget")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Show tasks for acme/widgets" }));

    expect(screen.getByText("Fix the widget")).toBeInTheDocument();
    expect(screen.getByText("Ship the widget")).toBeInTheDocument();
    expect(screen.getByText("Recall the widget")).toBeInTheDocument();
    expect(screen.queryByText("Ship the gadget")).not.toBeInTheDocument();
    expect(onOpenRepo).not.toHaveBeenCalled();
  });

  it("hides a repo's tasks again when its chevron is toggled a second time", async () => {
    const user = userEvent.setup();
    renderList();

    const toggle = () => screen.getByRole("button", { name: /tasks for acme\/widgets/ });
    await user.click(toggle());
    expect(screen.getByText("Fix the widget")).toBeInTheDocument();

    await user.click(toggle());
    expect(screen.queryByText("Fix the widget")).not.toBeInTheDocument();
  });

  it("opens a task from a folded-out sublist", async () => {
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenTask });

    await user.click(screen.getByRole("button", { name: "Show tasks for acme/widgets" }));
    await user.click(screen.getByText("Fix the widget"));

    expect(onOpenTask).toHaveBeenCalledWith("1");
  });

  it("calls onNewTask with the repo, not onOpenRepo, when a row's + button is clicked", async () => {
    const onOpenRepo = vi.fn();
    const onNewTask = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenRepo, onNewTask });

    await user.click(screen.getByRole("button", { name: "New task under acme/gadgets" }));

    expect(onNewTask).toHaveBeenCalledWith("acme/gadgets");
    expect(onOpenRepo).not.toHaveBeenCalled();
  });

  it("opens a row's New branch form and loads its branches, without also opening the repo", async () => {
    api.mockResolvedValueOnce([]);
    const onOpenRepo = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenRepo });

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "New branch" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/gadgets/branches");
    expect(screen.getByPlaceholderText("feature/foo")).toBeInTheDocument();
    expect(onOpenRepo).not.toHaveBeenCalled();
  });

  it("closes a row's New branch form when its button is clicked again", async () => {
    api.mockResolvedValue([]);
    const user = userEvent.setup();
    renderList();

    const row = screen.getByText("acme/gadgets").closest("li");
    const toggle = () => within(row).getByRole("button", { name: "New branch" });
    await user.click(toggle());
    expect(screen.getByPlaceholderText("feature/foo")).toBeInTheDocument();

    await user.click(toggle());
    expect(screen.queryByPlaceholderText("feature/foo")).not.toBeInTheDocument();
  });

  it("creates a branch and refreshes the branch list on success", async () => {
    api.mockResolvedValueOnce([]); // the load on opening the form
    api.mockResolvedValueOnce({ repo: "acme/gadgets", name: "myfeat", status: "pending", createdAt: "2026-01-01T00:00:00Z" });
    api.mockResolvedValueOnce([{ repo: "acme/gadgets", name: "myfeat", status: "pending", createdAt: "2026-01-01T00:00:00Z" }]);
    const user = userEvent.setup();
    renderList();

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "New branch" }));
    await user.type(screen.getByPlaceholderText("feature/foo"), "myfeat");
    await user.click(within(row).getByRole("button", { name: "Create branch" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/gadgets/branches", {
      method: "POST", body: JSON.stringify({ name: "myfeat" }),
    });
    const item = await screen.findByText("myfeat");
    expect(item.closest("li")).toHaveTextContent("myfeat -- pending");
  });

  it("reports the error when creating a branch fails, without clearing the field", async () => {
    api.mockResolvedValueOnce([]); // the load on opening the form
    api.mockRejectedValueOnce(new Error("invalid branch name"));
    const showError = vi.fn();
    const user = userEvent.setup();
    renderList({ showError });

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "New branch" }));
    await user.type(screen.getByPlaceholderText("feature/foo"), "bad name");
    await user.click(within(row).getByRole("button", { name: "Create branch" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "invalid branch name" }));
    expect(screen.getByPlaceholderText("feature/foo")).toHaveValue("bad name");
  });

  // grain/task-24: a repo's own default capability set is edited here,
  // next to the repo it belongs to, rather than on the deployment-wide
  // Settings pane -- and the form says what a task filed against this
  // repo would actually start with, which is the union of both layers.
  it("opens a row's Capabilities form and loads that repo's own defaults", async () => {
    api.mockResolvedValueOnce({
      repo: "acme/gadgets",
      defaultCapabilities: ["gcp-key"],
      deploymentDefaultCapabilities: ["gemini-key"],
      effectiveDefaultCapabilities: ["gemini-key", "gcp-key"],
    });
    const config = { capabilities: [{ id: "gcp-key", name: "GCP key" }, { id: "gemini-key", name: "Gemini key" }] };
    const onOpenRepo = vi.fn();
    const user = userEvent.setup();
    renderList({ config, onOpenRepo });

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Capabilities" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/gadgets/capabilities");
    expect(await screen.findByText(/A task filed against acme\/gadgets starts with:/)).toHaveTextContent(
      "Gemini key, GCP key",
    );
    expect(onOpenRepo).not.toHaveBeenCalled();
  });

  // Both sets GET reports come back as stored, retired ids included, so
  // that one chosen before a build retired it can still be seen and
  // unticked. What a task starts with is the filtered union, though --
  // (*Client).defaultCapabilities drops a retired id before any grant is
  // written -- so this line must not list one.
  it("leaves a retired id out of what a task filed against the repo starts with", async () => {
    api.mockResolvedValueOnce({
      repo: "acme/gadgets",
      defaultCapabilities: ["gcp-key", "scratch-repo"],
      deploymentDefaultCapabilities: ["gemini-key", "old-deployment-key"],
      effectiveDefaultCapabilities: ["gemini-key", "gcp-key"],
    });
    const config = { capabilities: [{ id: "gcp-key", name: "GCP key" }, { id: "gemini-key", name: "Gemini key" }] };
    const user = userEvent.setup();
    renderList({ config });

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Capabilities" }));

    const line = await screen.findByText(/A task filed against acme\/gadgets starts with:/);
    expect(line).toHaveTextContent("Gemini key, GCP key");
    expect(line).not.toHaveTextContent("scratch-repo");
    expect(line).not.toHaveTextContent("old-deployment-key");
  });

  it("says a repo whose only defaults are retired ids starts with nothing", async () => {
    api.mockResolvedValueOnce({
      repo: "acme/gadgets",
      defaultCapabilities: ["scratch-repo"],
      deploymentDefaultCapabilities: ["old-deployment-key"],
      effectiveDefaultCapabilities: [],
    });
    const config = { capabilities: [{ id: "gcp-key", name: "GCP key" }] };
    const user = userEvent.setup();
    renderList({ config });

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Capabilities" }));

    expect(await screen.findByText(/A task filed against acme\/gadgets starts with:/)).toHaveTextContent(
      "nothing -- only what whoever files it ticks",
    );
  });

  it("saves a repo's default capabilities and refreshes the config the new-task form seeds from", async () => {
    api.mockResolvedValueOnce({
      repo: "acme/gadgets",
      defaultCapabilities: [],
      deploymentDefaultCapabilities: [],
      effectiveDefaultCapabilities: [],
    });
    api.mockResolvedValueOnce({
      repo: "acme/gadgets",
      defaultCapabilities: ["gcp-key"],
      deploymentDefaultCapabilities: [],
      effectiveDefaultCapabilities: ["gcp-key"],
    });
    const config = { capabilities: [{ id: "gcp-key", name: "GCP key" }, { id: "gemini-key", name: "Gemini key" }] };
    const onRefreshConfig = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderList({ config, onRefreshConfig });

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Capabilities" }));
    await user.click(await screen.findByLabelText("Default capabilities"));
    await user.click(await screen.findByRole("option", { name: "GCP key" }));
    await user.keyboard("{Escape}");
    await user.click(within(row).getByRole("button", { name: "Save capabilities" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/gadgets/capabilities", {
      method: "PUT", body: JSON.stringify({ defaultCapabilities: ["gcp-key"] }),
    });
    expect(onRefreshConfig).toHaveBeenCalled();
  });

  it("reports the error when saving a repo's default capabilities fails", async () => {
    api.mockResolvedValueOnce({
      repo: "acme/gadgets",
      defaultCapabilities: [],
      deploymentDefaultCapabilities: [],
      effectiveDefaultCapabilities: [],
    });
    api.mockRejectedValueOnce(new Error("unknown capability nope"));
    const config = { capabilities: [{ id: "gcp-key", name: "GCP key" }] };
    const showError = vi.fn();
    const user = userEvent.setup();
    renderList({ config, showError });

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Capabilities" }));
    await screen.findByLabelText("Default capabilities");
    await user.click(within(row).getByRole("button", { name: "Save capabilities" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "unknown capability nope" }));
  });
});
