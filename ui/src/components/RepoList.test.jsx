import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import RepoList from "./RepoList.jsx";
import api from "../api.js";

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
  {
    id: "5",
    title: "Untargeted proposal",
    repo: "",
    state: "proposed",
    blocked: false,
    capabilities: [],
  },
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
  // The row order is this browser's own now (listOrder.js), so one
  // test's drag must not be the next test's starting order.
  beforeEach(() => {
    localStorage.clear();
  });

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

  // The same argument for the other kind of per-repo configuration
  // (grain/task-114): a repo whose only presence here is standing
  // instructions of its own, which reach every run against it and can
  // only be read or edited from its page.
  it("also lists a repo known only for its own prompt extension", () => {
    const config = {
      targetRepos: [],
      reposWithPromptExtension: ["acme/prompt-only"],
    };
    renderList({ config });

    const row = screen.getByText("acme/prompt-only").closest("li");
    expect(within(row).getByText("Defaults only")).toBeInTheDocument();
  });

  it("does not mark a repo that carries defaults and has tasks of its own as defaults-only", () => {
    const config = {
      targetRepos: [],
      repoDefaultCapabilities: { "acme/gadgets": ["gcp-key"] },
    };
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
    expect(
      screen.getByText(
        "No repos yet -- add one above, or file a task with a target repo.",
      ),
    ).toBeInTheDocument();
  });

  it("adds a repo and refreshes config on success", async () => {
    api.mockResolvedValueOnce({});
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    renderList({ tasks: [], onRefreshConfig });

    await user.type(screen.getByPlaceholderText("owner/name"), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    expect(api).toHaveBeenCalledWith("/api/repos", {
      method: "POST",
      body: JSON.stringify({ repo: "acme/widgets" }),
    });
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

    expect(showError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "repo must be owner/name" }),
    );
    expect(onRefreshConfig).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  // grain/task-45: an empty targetRepos means unrestricted, so the first
  // add narrows the deployment rather than widening it. The pane says so
  // twice -- standing on the page, and again at the click.
  it("says the deployment is unrestricted, and what adding the first repo would mean", () => {
    renderList({ config: { targetRepos: [] } });

    expect(
      screen.getByText(/an empty allowlist is what means unrestricted/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/restricts it to that one repo/),
    ).toBeInTheDocument();
  });

  it("says a one-repo allowlist allows only that repo", () => {
    renderList({ config: { targetRepos: ["acme/widgets"] } });

    expect(screen.getByText(/allows only acme\/widgets/)).toBeInTheDocument();
    expect(
      screen.queryByText(/an empty allowlist is what means unrestricted/),
    ).not.toBeInTheDocument();
  });

  it("drops both notes once the allowlist names more than one repo", () => {
    renderList({ config: { targetRepos: ["acme/widgets", "acme/gadgets"] } });

    expect(
      screen.queryByText(/an empty allowlist is what means unrestricted/),
    ).not.toBeInTheDocument();
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
    expect(api).toHaveBeenCalledWith("/api/repos", {
      method: "POST",
      body: JSON.stringify({ repo: "acme/widgets" }),
    });
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

  // grain/task-327: a row opens with a status dot, so a repo with an
  // agent working in it right now is visible without reading its counts.
  describe("status dot", () => {
    it("calls a repo with a running task active", () => {
      renderList();

      const row = screen.getByText("acme/widgets").closest("li");
      expect(
        within(row).getByLabelText("Active -- 1 task running now"),
      ).toBeInTheDocument();
    });

    // "Queued for merge" is not an ending: the task is still waiting on
    // the merge queue, so the repo still has work outstanding.
    it("calls a repo with work but nothing running open", () => {
      renderList();

      const row = screen.getByText("acme/gadgets").closest("li");
      expect(
        within(row).getByLabelText("1 open task, none running right now"),
      ).toBeInTheDocument();
    });

    it("calls a repo whose every task is closed idle", () => {
      renderList({
        tasks: [
          {
            id: "9",
            title: "Old news",
            repo: "acme/done",
            state: "closed",
            blocked: false,
            capabilities: [],
          },
        ],
      });

      const row = screen.getByText("acme/done").closest("li");
      expect(
        within(row).getByLabelText("Idle -- every task here is closed"),
      ).toBeInTheDocument();
    });

    it("calls a repo with no tasks at all idle", () => {
      renderList({ config: { targetRepos: ["acme/fresh"] }, tasks: [] });

      const row = screen.getByText("acme/fresh").closest("li");
      expect(
        within(row).getByLabelText("Idle -- no tasks here yet"),
      ).toBeInTheDocument();
    });
  });

  // grain/task-327: the rows can be dragged into an order of this
  // browser's own -- a display order, stored here and nowhere else,
  // which is why nothing is posted to the API when one moves.
  describe("custom order", () => {
    const namesInOrder = () =>
      [...document.querySelectorAll(".repo-list-name")].map(
        (e) => e.textContent,
      );

    const rowFor = (repo) => screen.getByText(repo).closest("li");

    it("opens in alphabetical order, before anything has been dragged", () => {
      renderList();
      expect(namesInOrder()).toEqual(["acme/gadgets", "acme/widgets"]);
    });

    it("keeps a dragged row where it was dropped, across a remount", () => {
      const { unmount } = render(
        <RepoList
          tasks={tasks}
          config={null}
          onOpenRepo={vi.fn()}
          onRefreshConfig={vi.fn()}
          showError={vi.fn()}
        />,
      );

      fireEvent.dragStart(rowFor("acme/widgets"));
      fireEvent.dragOver(rowFor("acme/gadgets"));
      fireEvent.drop(rowFor("acme/gadgets"));

      expect(namesInOrder()).toEqual(["acme/widgets", "acme/gadgets"]);
      expect(api).not.toHaveBeenCalled();

      unmount();
      renderList();
      expect(namesInOrder()).toEqual(["acme/widgets", "acme/gadgets"]);
    });

    it("drops a row at the very end of the list", () => {
      renderList();

      fireEvent.dragStart(rowFor("acme/gadgets"));
      // The trailing drop zone only renders once a drag is in flight.
      fireEvent.dragOver(document.querySelector(".repo-list .task-drop-end"));
      fireEvent.drop(document.querySelector(".repo-list .task-drop-end"));

      expect(namesInOrder()).toEqual(["acme/widgets", "acme/gadgets"]);
    });

    it("clicking the drag handle does not open the repo", async () => {
      const onOpenRepo = vi.fn();
      const user = userEvent.setup();
      renderList({ onOpenRepo });

      await user.click(document.querySelector(".task-drag-handle"));

      expect(onOpenRepo).not.toHaveBeenCalled();
    });

    // Every other order is one this page computes, so there is nowhere
    // for a drop to land: the handles go with the gesture.
    it("withdraws the handles when the list is sorted some other way", async () => {
      const user = userEvent.setup();
      renderList();

      expect(document.querySelectorAll(".task-drag-handle")).toHaveLength(2);

      await user.click(screen.getByLabelText("Sort"));
      await user.click(
        await screen.findByRole("option", { name: "Name (A–Z)" }),
      );

      expect(document.querySelectorAll(".task-drag-handle")).toHaveLength(0);
    });

    it("sorts the busiest repo first when asked for most tasks", async () => {
      const user = userEvent.setup();
      renderList();

      await user.click(screen.getByLabelText("Sort"));
      await user.click(
        await screen.findByRole("option", { name: "Most tasks" }),
      );

      expect(namesInOrder()).toEqual(["acme/widgets", "acme/gadgets"]);
    });

    it("sorts the repo with a run in flight first when asked for active first", async () => {
      const user = userEvent.setup();
      renderList();

      await user.click(screen.getByLabelText("Sort"));
      await user.click(
        await screen.findByRole("option", { name: "Active first" }),
      );

      expect(namesInOrder()).toEqual(["acme/widgets", "acme/gadgets"]);
    });
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
    expect(api).toHaveBeenCalledWith("/api/repos", {
      method: "POST",
      body: JSON.stringify({ repo: "acme/gadgets" }),
    });
    vi.unstubAllGlobals();
  });
});
