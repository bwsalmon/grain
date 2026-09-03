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

  // A repo that carries default capabilities of its own is listed even
  // when nothing else here mentions it: PUT /api/repos/{owner}/{name}/
  // capabilities never required an allowlist entry, and its own page is
  // the only place that set can be edited.
  it("also lists a repo that only carries default capabilities of its own", () => {
    const config = {
      targetRepos: [],
      repoDefaultCapabilities: { "acme/orphan": ["gcp-key"] },
      capabilities: [{ id: "gcp-key", name: "GCP key" }],
    };
    renderList({ config });

    const row = screen.getByText("acme/orphan").closest("li");
    expect(within(row).getByText("Defaults only")).toBeInTheDocument();
  });

  it("does not mark a repo that carries defaults and has tasks of its own as defaults-only", () => {
    const config = { targetRepos: [], repoDefaultCapabilities: { "acme/gadgets": ["gcp-key"] } };
    renderList({ config });

    const row = screen.getByText("acme/gadgets").closest("li");
    expect(within(row).queryByText("Defaults only")).not.toBeInTheDocument();
  });

  // grain/task-111: a row is a name and its counts. Everything that used
  // to sit on one -- New branch, Capabilities, Releases, Remove, "+",
  // and the chevron that folded the repo's tasks out in place -- is on
  // the repo's own page now.
  it("carries no per-row buttons at all", () => {
    const config = { targetRepos: ["acme/widgets"] };
    renderList({ config });

    const row = screen.getByText("acme/widgets").closest("li");
    expect(within(row).queryAllByRole("button")).toHaveLength(0);
    expect(screen.queryByText("Fix the widget")).not.toBeInTheDocument();
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

  it("shows an empty message when there are no known repos", () => {
    renderList({ tasks: [] });
    expect(screen.getByText("No repos yet -- add one above, or file a task with a target repo.")).toBeInTheDocument();
  });

  it("adds a repo and refreshes config on success", async () => {
    api.mockResolvedValueOnce({});
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    renderList({ tasks: [], onRefreshConfig });

    await user.type(screen.getByPlaceholderText("owner/name"), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    expect(api).toHaveBeenCalledWith("/api/repos", { method: "POST", body: JSON.stringify({ repo: "acme/widgets" }) });
    expect(onRefreshConfig).toHaveBeenCalledTimes(1);
    vi.unstubAllGlobals();
  });

  it("reports the error and does not refresh config when adding fails", async () => {
    api.mockRejectedValueOnce(new Error("repo must be owner/name"));
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const onRefreshConfig = vi.fn();
    const showError = vi.fn();
    const user = userEvent.setup();
    renderList({ tasks: [], onRefreshConfig, showError });

    await user.type(screen.getByPlaceholderText("owner/name"), "not-a-repo");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "repo must be owner/name" }));
    expect(onRefreshConfig).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  // grain/task-45: an empty targetRepos means unrestricted, so the first
  // add narrows the deployment rather than widening it. The pane says so
  // twice -- standing on the page, and again at the click.
  it("says the deployment is unrestricted, and what adding the first repo would mean", () => {
    renderList({ config: { targetRepos: [] } });

    expect(screen.getByText(/an empty allowlist is what means unrestricted/)).toBeInTheDocument();
    expect(screen.getByText(/restricts it to that one repo/)).toBeInTheDocument();
  });

  it("says a one-repo allowlist allows only that repo", () => {
    renderList({ config: { targetRepos: ["acme/widgets"] } });

    expect(screen.getByText(/allows only acme\/widgets/)).toBeInTheDocument();
    expect(screen.queryByText(/an empty allowlist is what means unrestricted/)).not.toBeInTheDocument();
  });

  it("drops both notes once the allowlist names more than one repo", () => {
    renderList({ config: { targetRepos: ["acme/widgets", "acme/gadgets"] } });

    expect(screen.queryByText(/an empty allowlist is what means unrestricted/)).not.toBeInTheDocument();
    expect(screen.queryByText(/allows only/)).not.toBeInTheDocument();
  });

  it("confirms the first add, naming the repos that would fall off the allowlist", async () => {
    api.mockResolvedValueOnce({});
    const confirmed = vi.fn().mockReturnValue(true);
    vi.stubGlobal("confirm", confirmed);
    const user = userEvent.setup();
    renderList({ config: { targetRepos: [] } });

    await user.type(screen.getByPlaceholderText("owner/name"), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    const msg = confirmed.mock.calls[0][0];
    expect(msg).toContain("only repo this deployment allows");
    // The repo being added is not one of the repos that fall off it.
    const falling = msg.split("Off the allowlist as of this click: ")[1];
    expect(falling).toContain("acme/gadgets");
    expect(falling).not.toContain("acme/widgets");
    expect(api).toHaveBeenCalledWith("/api/repos", { method: "POST", body: JSON.stringify({ repo: "acme/widgets" }) });
    vi.unstubAllGlobals();
  });

  it("does not add the first repo when the confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(false));
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    renderList({ config: { targetRepos: [] }, onRefreshConfig });

    await user.type(screen.getByPlaceholderText("owner/name"), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    expect(api).not.toHaveBeenCalled();
    expect(onRefreshConfig).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("does not confirm an add that only widens an allowlist that already restricts", async () => {
    api.mockResolvedValueOnce({});
    const confirmed = vi.fn().mockReturnValue(true);
    vi.stubGlobal("confirm", confirmed);
    const user = userEvent.setup();
    renderList({ config: { targetRepos: ["acme/widgets"] } });

    await user.type(screen.getByPlaceholderText("owner/name"), "acme/gadgets");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    expect(confirmed).not.toHaveBeenCalled();
    expect(api).toHaveBeenCalledWith("/api/repos", { method: "POST", body: JSON.stringify({ repo: "acme/gadgets" }) });
    vi.unstubAllGlobals();
  });
});
