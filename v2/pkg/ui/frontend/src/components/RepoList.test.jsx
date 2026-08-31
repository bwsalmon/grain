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
});
