import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import TaskList from "./TaskList.jsx";

const tasks = [
  {
    id: 1,
    title: "Fix the thing",
    state: "queued",
    capabilities: [],
    blocked: false,
  },
  {
    id: 2,
    title: "Ship the other thing",
    state: "running",
    capabilities: [],
    blocked: true,
    blockedBy: [1],
  },
];

function renderList(overrides = {}) {
  const props = {
    tasks,
    stateFilter: "all",
    config: null,
    onOpenTask: vi.fn(),
    selected: new Set(),
    onToggleSelect: vi.fn(),
    onSelectAll: vi.fn(),
    ...overrides,
  };
  const { rerender } = render(<TaskList {...props} />);
  // rerender takes another set of overrides, for a test that needs this
  // component to survive a change of props -- a poll bringing new task
  // states, the sidebar's own filter moving -- rather than mount fresh.
  return {
    ...props,
    rerender: (next) => rerender(<TaskList {...props} {...next} />),
  };
}

// row/rowFor is the draggable <li> a task's title sits inside -- what
// TaskList.jsx's onDragStart/onDragOver/onDrop are actually attached to,
// one level up from the text a test finds the row by.
function rowFor(title) {
  return screen.getByText(title).closest("li");
}

describe("TaskList", () => {
  it("lists every task when the filter is 'all'", () => {
    renderList();
    expect(screen.getByText("Fix the thing")).toBeInTheDocument();
    expect(screen.getByText("Ship the other thing")).toBeInTheDocument();
    expect(document.querySelector(".content-header .count")).toHaveTextContent(
      "2",
    );
  });

  it("filters down to a single state", () => {
    renderList({ stateFilter: "running" });
    expect(screen.queryByText("Fix the thing")).not.toBeInTheDocument();
    expect(screen.getByText("Ship the other thing")).toBeInTheDocument();
  });

  it("filters to blocked tasks", () => {
    renderList({ stateFilter: "blocked" });
    expect(screen.queryByText("Fix the thing")).not.toBeInTheDocument();
    expect(screen.getByText("Ship the other thing")).toBeInTheDocument();
  });

  // Each row says where its pull request stands with its own state
  // badge, whose title is STATE_LABELS[state] -- no chip repeating it.
  it("tells a task waiting on Submit apart from one on the merge queue", () => {
    renderList({
      tasks: [
        {
          id: 3,
          title: "Awaiting submit task",
          state: "awaiting_submit",
          pullRequest: "acme/widgets#1",
          autoMerge: false,
          capabilities: [],
          blocked: false,
        },
        {
          id: 4,
          title: "Queued for merge task",
          state: "completed",
          pullRequest: "acme/widgets#2",
          autoMerge: true,
          capabilities: [],
          blocked: false,
        },
      ],
    });
    expect(screen.getAllByTitle("Awaiting submit").length).toBeGreaterThan(0);
    expect(screen.getAllByTitle("Queued for merge").length).toBeGreaterThan(0);
  });

  // The one correction still worth a chip: on the queue in name only.
  it("shows a merge-blocked chip once the queue has given up", () => {
    renderList({
      tasks: [
        {
          id: 5,
          title: "Stuck task",
          state: "completed",
          pullRequest: "acme/widgets#3",
          autoMerge: true,
          mergeQueueBlockedAt: "2026-08-01T00:00:00Z",
          capabilities: [],
          blocked: false,
        },
      ],
    });
    expect(screen.getByText("Merge blocked")).toBeInTheDocument();
  });

  it("shows an empty message when nothing matches the filter", () => {
    renderList({ tasks: [] });
    expect(screen.getByText("No tasks in this state.")).toBeInTheDocument();
  });

  // bwsalmon/agents#537: closed tasks are hidden by default, with a
  // per-list toggle to bring them back and a deployment-wide setting
  // (config.showClosedByDefault) for what "by default" starts out as.
  describe("hiding closed tasks", () => {
    const withClosed = [
      ...tasks,
      {
        id: 3,
        title: "Long done",
        state: "closed",
        capabilities: [],
        blocked: false,
      },
    ];

    it("hides closed tasks by default", () => {
      renderList({ tasks: withClosed });
      expect(screen.queryByText("Long done")).not.toBeInTheDocument();
      expect(
        document.querySelector(".content-header .count"),
      ).toHaveTextContent("2");
    });

    it("shows closed tasks once the toggle is checked", async () => {
      const user = userEvent.setup();
      renderList({ tasks: withClosed });

      await user.click(
        screen.getByRole("checkbox", { name: "Show closed tasks" }),
      );

      expect(screen.getByText("Long done")).toBeInTheDocument();
    });

    it("starts showing closed tasks when config.showClosedByDefault is set", () => {
      renderList({ tasks: withClosed, config: { showClosedByDefault: true } });
      expect(screen.getByText("Long done")).toBeInTheDocument();
      expect(
        screen.getByRole("checkbox", { name: "Show closed tasks" }),
      ).toBeChecked();
    });

    it("does not offer the toggle when there are no closed tasks", () => {
      renderList();
      expect(
        screen.queryByRole("checkbox", { name: "Show closed tasks" }),
      ).not.toBeInTheDocument();
    });

    it("does not hide closed tasks while the Closed filter itself is selected", () => {
      renderList({ tasks: withClosed, stateFilter: "closed" });
      expect(screen.getByText("Long done")).toBeInTheDocument();
      expect(
        screen.queryByRole("checkbox", { name: "Show closed tasks" }),
      ).not.toBeInTheDocument();
    });
  });

  it("opens a task when its row is clicked", async () => {
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenTask });

    await user.click(screen.getByText("Fix the thing"));

    expect(onOpenTask).toHaveBeenCalledWith(1);
  });

  it("toggles a single task's selection without opening it", async () => {
    const onOpenTask = vi.fn();
    const onToggleSelect = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenTask, onToggleSelect });

    await user.click(screen.getAllByRole("checkbox")[1]);

    expect(onToggleSelect).toHaveBeenCalledWith(1);
    expect(onOpenTask).not.toHaveBeenCalled();
  });

  it("selects every visible task through the select-all checkbox", async () => {
    const onSelectAll = vi.fn();
    const user = userEvent.setup();
    renderList({ onSelectAll });

    await user.click(screen.getByLabelText("Select all"));

    expect(onSelectAll).toHaveBeenCalledWith([1, 2], true);
  });

  it("checks select-all once every visible task is already selected", () => {
    renderList({ selected: new Set([1, 2]) });
    expect(screen.getByLabelText("Select all")).toBeChecked();
  });

  describe("drag-and-drop reordering (bwsalmon/agents#476)", () => {
    const threeTasks = [
      {
        id: 1,
        title: "First",
        state: "queued",
        capabilities: [],
        blocked: false,
      },
      {
        id: 2,
        title: "Second",
        state: "queued",
        capabilities: [],
        blocked: false,
      },
      {
        id: 3,
        title: "Third",
        state: "queued",
        capabilities: [],
        blocked: false,
      },
    ];

    it("drops a task onto another, calling onReorder with its new neighbours", () => {
      const onReorder = vi.fn();
      renderList({ tasks: threeTasks, onReorder });

      fireEvent.dragStart(rowFor("Third"));
      fireEvent.dragOver(rowFor("First"));
      fireEvent.drop(rowFor("First"));

      // Third dropped onto First's row lands just before it -- no
      // preceding job, so afterId is null (the issue's own "moved to the
      // head of the list" case).
      expect(onReorder).toHaveBeenCalledWith([3], null, 1);
    });

    it("drops a task between two others", () => {
      const onReorder = vi.fn();
      renderList({ tasks: threeTasks, onReorder });

      fireEvent.dragStart(rowFor("Third"));
      fireEvent.dragOver(rowFor("Second"));
      fireEvent.drop(rowFor("Second"));

      expect(onReorder).toHaveBeenCalledWith([3], 1, 2);
    });

    it("drops a task at the very end of the list", () => {
      const onReorder = vi.fn();
      renderList({ tasks: threeTasks, onReorder });

      fireEvent.dragStart(rowFor("First"));
      // The trailing drop zone only renders once a drag is in flight.
      fireEvent.dragOver(document.querySelector(".task-drop-end"));
      fireEvent.drop(document.querySelector(".task-drop-end"));

      expect(onReorder).toHaveBeenCalledWith([1], 3, null);
    });

    it("drags every selected task as a block, in their existing backlog order, when dragging a selected row", () => {
      const onReorder = vi.fn();
      renderList({ tasks: threeTasks, onReorder, selected: new Set([1, 3]) });

      // Grabbing the un-selected middle task drags only itself.
      fireEvent.dragStart(rowFor("Second"));
      fireEvent.drop(rowFor("Third"));
      expect(onReorder).toHaveBeenLastCalledWith([2], 1, 3);

      // Grabbing a selected task drags the whole selection.
      fireEvent.dragStart(rowFor("Third"));
      fireEvent.drop(document.querySelector(".task-drop-end"));
      expect(onReorder).toHaveBeenLastCalledWith([1, 3], 2, null);
    });

    it("clicking the drag handle does not open the task (bwsalmon/agents#490)", async () => {
      const onOpenTask = vi.fn();
      const onReorder = vi.fn();
      const user = userEvent.setup();
      renderList({ tasks: threeTasks, onOpenTask, onReorder });

      await user.click(document.querySelector(".task-drag-handle"));

      expect(onOpenTask).not.toHaveBeenCalled();
    });

    // A stacked merge fix (bwsalmon/agents#378) is nested under the task
    // it repairs and never draggable -- the merge queue runs it ahead of
    // the backlog whatever the backlog says -- so it keeps the column the
    // handle sits in rather than letting its own contents slide a handle's
    // width left of every other row's.
    describe("the handle column on a nested merge fix", () => {
      const parent = {
        id: 1,
        title: "First",
        state: "queued",
        capabilities: [],
        blocked: false,
      };
      const fix = {
        id: 4,
        title: "Repair the pull request",
        state: "queued",
        capabilities: [],
        blocked: false,
        stacked: true,
        generatedFrom: 1,
      };
      const rowOf = (title) => screen.getByText(title).closest(".task-row");

      it("holds the column open, and empty, while the rows around it are draggable", () => {
        renderList({ tasks: [parent, fix], onReorder: vi.fn() });

        expect(
          rowOf("Repair the pull request").querySelector(
            ".task-drag-placeholder",
          ),
        ).toBeInTheDocument();
        expect(
          rowOf("Repair the pull request").querySelector(".task-drag-handle"),
        ).not.toBeInTheDocument();
        // The task it is nested under still gets a handle of its own.
        expect(
          rowOf("First").querySelector(".task-drag-handle"),
        ).toBeInTheDocument();
      });

      it("holds no column open when no row in the list has a handle either", () => {
        renderList({ tasks: [parent, fix] });

        expect(
          document.querySelector(".task-drag-placeholder"),
        ).not.toBeInTheDocument();
      });
    });

    it("never starts a drag, and shows no drag handle, without an onReorder prop", () => {
      const { container } = render(
        <TaskList
          tasks={threeTasks}
          stateFilter="all"
          config={null}
          onOpenTask={vi.fn()}
          selected={new Set()}
          onToggleSelect={vi.fn()}
          onSelectAll={vi.fn()}
        />,
      );
      expect(
        container.querySelector(".task-drag-handle"),
      ).not.toBeInTheDocument();
      expect(rowFor("First")).toHaveAttribute("draggable", "false");
    });
  });

  it("badges a task a schedule filed, and leaves an ordinary one unbadged", () => {
    renderList({
      tasks: [{ ...tasks[0], scheduled: true }, tasks[1]],
    });
    expect(
      screen.getByTitle("filed automatically by a schedule"),
    ).toHaveTextContent("scheduled");
    expect(
      screen.queryAllByTitle("filed automatically by a schedule"),
    ).toHaveLength(1);
  });

  // bwsalmon/agents#642: a suite run files its pass of tasks with nobody
  // typing them in, so they get scheduled's own badge treatment.
  it("badges a task a suite run filed, and leaves an ordinary one unbadged", () => {
    renderList({
      tasks: [{ ...tasks[0], suiteRun: true }, tasks[1]],
    });
    expect(
      screen.getByTitle("filed automatically by a suite run"),
    ).toHaveTextContent("suite");
    expect(
      screen.queryAllByTitle("filed automatically by a suite run"),
    ).toHaveLength(1);
    // The suite chip is its own thing, not the schedule one relabelled.
    expect(
      screen.queryByTitle("filed automatically by a schedule"),
    ).not.toBeInTheDocument();
  });

  // bwsalmon/agents#378: a stacked task explains itself by sitting under
  // the task it repairs, so the chip is only for the rows where that
  // nesting is missing.
  describe("the merge-fix chip on a stacked task", () => {
    const parent = {
      id: 1,
      title: "Fix the thing",
      state: "queued",
      capabilities: [],
      blocked: false,
    };
    const fix = {
      id: 2,
      title: "Repair the pull request",
      state: "queued",
      capabilities: [],
      blocked: false,
      stacked: true,
      generatedFrom: 1,
    };

    it("leaves a fix nested under its own parent unchipped", () => {
      renderList({ tasks: [parent, fix] });
      expect(document.querySelector(".task-sublist")).toHaveTextContent(
        "Repair the pull request",
      );
      expect(screen.queryByText("merge fix")).not.toBeInTheDocument();
    });

    it("chips a fix listed at top level because its parent is filtered out of the view", () => {
      renderList({
        tasks: [{ ...parent, state: "completed" }, fix],
        stateFilter: "queued",
      });
      expect(document.querySelector(".task-sublist")).not.toBeInTheDocument();
      expect(
        screen.getByTitle("the merge queue's own automatic fix for 1"),
      ).toHaveTextContent("merge fix");
    });

    it("chips a fix whose parent is gone entirely", () => {
      renderList({ tasks: [fix] });
      expect(
        screen.getByTitle("the merge queue's own automatic fix for 1"),
      ).toHaveTextContent("merge fix");
    });

    it("chips a fix with no generatedFrom at all, without naming a parent", () => {
      renderList({ tasks: [{ ...fix, generatedFrom: undefined }] });
      expect(
        screen.getByTitle(
          "the merge queue's own automatic fix for another task's pull request",
        ),
      ).toHaveTextContent("merge fix");
    });
  });

  // bwsalmon/agents#539
  it("badges an interactive task, and leaves an ordinary one unbadged", () => {
    renderList({
      tasks: [{ ...tasks[0], interactive: true }, tasks[1]],
    });
    expect(
      screen.getByTitle("a live chat, not a background task"),
    ).toHaveTextContent("interactive");
    expect(
      screen.queryAllByTitle("a live chat, not a background task"),
    ).toHaveLength(1);
  });

  // bwsalmon/agents#586
  it("gives a running task's state dot the grain mark instead of the plain CSS spinner", () => {
    renderList();
    const queuedDot = rowFor("Fix the thing").querySelector(".badge");
    const runningDot = rowFor("Ship the other thing").querySelector(".badge");

    expect(queuedDot).not.toHaveClass("badge-mark");
    expect(queuedDot).toBeEmptyDOMElement();
    expect(runningDot).toHaveClass("badge-mark");
    // At the badge's size the mark plays a pre-recorded sheet rather
    // than painting, so it needs no canvas and renders here the same way
    // it does in a browser -- see GrainMark.test.jsx for the mechanics.
    expect(runningDot.querySelector(".grain-mark-sheet")).toHaveAttribute(
      "title",
      "Running",
    );
  });

  // A run that is the merge queue repairing this task's own pull request
  // branch, rather than writing the change: the same mark, moving the
  // same way, in green instead of the accent (.grain-mark-repair), and a
  // badge that says which kind of work it is. A row that has gone back to
  // running has not gone back to the beginning.
  it("marks a task the merge queue is repairing apart from one merely running", () => {
    renderList({
      tasks: [{ ...tasks[1], state: "running", repairing: true }],
    });
    const dot = rowFor("Ship the other thing").querySelector(".badge");
    const mark = dot.querySelector(".grain-mark-sheet");

    expect(dot).toHaveClass("badge-mark");
    expect(mark).toHaveClass("grain-mark-repair");
    expect(mark).toHaveAttribute("title", "Repairing");
  });

  describe("search (bwsalmon/agents#460)", () => {
    it("filters down to tasks whose title matches the search text", async () => {
      const user = userEvent.setup();
      renderList();

      await user.type(screen.getByPlaceholderText("Search tasks…"), "ship");

      expect(screen.queryByText("Fix the thing")).not.toBeInTheDocument();
      expect(screen.getByText("Ship the other thing")).toBeInTheDocument();
      expect(
        document.querySelector(".content-header .count"),
      ).toHaveTextContent("1");
    });

    it("filters down to tasks whose id matches the search text, case-insensitively", async () => {
      const user = userEvent.setup();
      renderList({
        tasks: [
          {
            id: "abc-1",
            title: "Fix the thing",
            state: "queued",
            capabilities: [],
            blocked: false,
          },
          {
            id: "xyz-2",
            title: "Ship the other thing",
            state: "running",
            capabilities: [],
            blocked: false,
          },
        ],
      });

      await user.type(screen.getByPlaceholderText("Search tasks…"), "ABC");

      expect(screen.getByText("Fix the thing")).toBeInTheDocument();
      expect(
        screen.queryByText("Ship the other thing"),
      ).not.toBeInTheDocument();
    });

    it("shows a search-specific empty message when nothing matches", async () => {
      const user = userEvent.setup();
      renderList();

      await user.type(
        screen.getByPlaceholderText("Search tasks…"),
        "nothing matches this",
      );

      expect(
        screen.getByText("No tasks match your search."),
      ).toBeInTheDocument();
    });

    it("hides the toolbar entirely when there are no tasks at all", () => {
      renderList({ tasks: [] });
      expect(
        screen.queryByPlaceholderText("Search tasks…"),
      ).not.toBeInTheDocument();
    });
  });

  describe("sort (bwsalmon/agents#460)", () => {
    const threeTasks = [
      {
        id: 1,
        title: "Charlie",
        state: "queued",
        capabilities: [],
        blocked: false,
        createdAt: "2026-01-03T00:00:00Z",
      },
      {
        id: 2,
        title: "Alpha",
        state: "queued",
        capabilities: [],
        blocked: false,
        createdAt: "2026-01-01T00:00:00Z",
      },
      {
        id: 3,
        title: "Bravo",
        state: "queued",
        capabilities: [],
        blocked: false,
        createdAt: "2026-01-02T00:00:00Z",
      },
    ];

    function titlesInOrder() {
      return [...document.querySelectorAll(".task-title")].map(
        (el) => el.textContent,
      );
    }

    it("defaults to the given (backlog) order", () => {
      renderList({ tasks: threeTasks });
      expect(titlesInOrder()).toEqual(["Charlie", "Alpha", "Bravo"]);
    });

    it("sorts alphabetically by title when picked from the sort menu", async () => {
      const user = userEvent.setup();
      renderList({ tasks: threeTasks });

      await user.click(screen.getByLabelText("Sort"));
      await user.click(
        await screen.findByRole("option", { name: "Title (A–Z)" }),
      );

      expect(titlesInOrder()).toEqual(["Alpha", "Bravo", "Charlie"]);
    });

    it("sorts oldest-first by createdAt when picked from the sort menu", async () => {
      const user = userEvent.setup();
      renderList({ tasks: threeTasks });

      await user.click(screen.getByLabelText("Sort"));
      await user.click(
        await screen.findByRole("option", { name: "Oldest first" }),
      );

      expect(titlesInOrder()).toEqual(["Alpha", "Bravo", "Charlie"]);
    });

    // grain/task-288: the same menu now orders on the other attributes a
    // row shows -- state, repo, author -- with the backlog's own order
    // kept inside each group, since the sort is stable.
    it("sorts by state in the sidebar's own order", async () => {
      const user = userEvent.setup();
      renderList({
        tasks: [
          {
            id: 1,
            title: "Charlie",
            state: "completed",
            capabilities: [],
            blocked: false,
          },
          {
            id: 2,
            title: "Alpha",
            state: "proposed",
            capabilities: [],
            blocked: false,
          },
          {
            id: 3,
            title: "Bravo",
            state: "running",
            capabilities: [],
            blocked: false,
          },
        ],
      });

      await user.click(screen.getByLabelText("Sort"));
      await user.click(await screen.findByRole("option", { name: "State" }));

      expect(titlesInOrder()).toEqual(["Alpha", "Bravo", "Charlie"]);
    });

    it("sorts by repo, with the tasks that have none last", async () => {
      const user = userEvent.setup();
      renderList({
        tasks: [
          {
            id: 1,
            title: "Charlie",
            state: "queued",
            capabilities: [],
            blocked: false,
          },
          {
            id: 2,
            title: "Alpha",
            state: "queued",
            repo: "acme/gadgets",
            capabilities: [],
            blocked: false,
          },
          {
            id: 3,
            title: "Bravo",
            state: "queued",
            repo: "acme/widgets",
            capabilities: [],
            blocked: false,
          },
        ],
      });

      await user.click(screen.getByLabelText("Sort"));
      await user.click(
        await screen.findByRole("option", { name: "Repo (A–Z)" }),
      );

      expect(titlesInOrder()).toEqual(["Alpha", "Bravo", "Charlie"]);
    });

    it("sorts by author", async () => {
      const user = userEvent.setup();
      renderList({
        tasks: [
          {
            id: 1,
            title: "Charlie",
            state: "queued",
            author: "grace",
            capabilities: [],
            blocked: false,
          },
          {
            id: 2,
            title: "Alpha",
            state: "queued",
            author: "ada",
            capabilities: [],
            blocked: false,
          },
          {
            id: 3,
            title: "Bravo",
            state: "queued",
            author: "bob",
            capabilities: [],
            blocked: false,
          },
        ],
      });

      await user.click(screen.getByLabelText("Sort"));
      await user.click(
        await screen.findByRole("option", { name: "Author (A–Z)" }),
      );

      expect(titlesInOrder()).toEqual(["Alpha", "Bravo", "Charlie"]);
    });

    it("disables drag-and-drop reordering once a non-backlog sort is chosen", async () => {
      const onReorder = vi.fn();
      const user = userEvent.setup();
      renderList({ tasks: threeTasks, onReorder });

      expect(rowFor("Charlie")).toHaveAttribute("draggable", "true");

      await user.click(screen.getByLabelText("Sort"));
      await user.click(
        await screen.findByRole("option", { name: "Title (A–Z)" }),
      );

      expect(
        document.querySelector(".task-drag-handle"),
      ).not.toBeInTheDocument();
      expect(rowFor("Alpha")).toHaveAttribute("draggable", "false");

      fireEvent.dragStart(rowFor("Alpha"));
      fireEvent.drop(rowFor("Bravo"));
      expect(onReorder).not.toHaveBeenCalled();
    });
  });

  // grain/task-288: the toolbar narrows on the attributes the sidebar's
  // own state filter says nothing about -- repo, base branch,
  // capability, author, how the task got filed, whether it is a chat,
  // whether it merges itself.
  describe("filter (grain/task-288)", () => {
    const config = {
      capabilities: [
        { id: "gh", name: "GitHub" },
        { id: "gcp", name: "Google Cloud" },
      ],
    };
    const mixed = [
      {
        id: 1,
        title: "Widget work",
        state: "queued",
        blocked: false,
        repo: "acme/widgets",
        base: "main",
        author: "ada",
        capabilities: ["gh"],
        autoMerge: true,
      },
      {
        id: 2,
        title: "Gadget work",
        state: "queued",
        blocked: false,
        repo: "acme/gadgets",
        base: "release",
        author: "grace",
        capabilities: ["gcp"],
        scheduled: true,
      },
      {
        id: 3,
        title: "Loose work",
        state: "queued",
        blocked: false,
        author: "ada",
        capabilities: [],
        interactive: true,
      },
    ];

    function titles() {
      return [...document.querySelectorAll(".task-title")].map(
        (el) => el.textContent,
      );
    }

    async function pick(user, label, option) {
      await user.click(screen.getByLabelText(label));
      await user.click(await screen.findByRole("option", { name: option }));
    }

    it("narrows to one repo, and offers the tasks with no repo as their own answer", async () => {
      const user = userEvent.setup();
      renderList({ tasks: mixed, config });

      await pick(user, "Repo", "acme/widgets");
      expect(titles()).toEqual(["Widget work"]);

      await pick(user, "Repo", "No repo");
      expect(titles()).toEqual(["Loose work"]);
    });

    it("narrows to one base branch", async () => {
      const user = userEvent.setup();
      renderList({ tasks: mixed, config });

      await pick(user, "Base", "release");

      expect(titles()).toEqual(["Gadget work"]);
    });

    it("narrows to one capability, by the name the deployment gives it", async () => {
      const user = userEvent.setup();
      renderList({ tasks: mixed, config });

      await pick(user, "Capability", "Google Cloud");

      expect(titles()).toEqual(["Gadget work"]);
    });

    it("narrows to one author", async () => {
      const user = userEvent.setup();
      renderList({ tasks: mixed, config });

      await pick(user, "Author", "ada");

      expect(titles()).toEqual(["Widget work", "Loose work"]);
    });

    it("narrows to how the task was filed", async () => {
      const user = userEvent.setup();
      renderList({ tasks: mixed, config });

      await pick(user, "Origin", "Scheduled");
      expect(titles()).toEqual(["Gadget work"]);

      await pick(user, "Origin", "Filed by hand");
      expect(titles()).toEqual(["Widget work", "Loose work"]);
    });

    it("tells an interactive chat from a background task", async () => {
      const user = userEvent.setup();
      renderList({ tasks: mixed, config });

      await pick(user, "Kind", "Interactive");

      expect(titles()).toEqual(["Loose work"]);
    });

    it("narrows to the tasks that merge themselves", async () => {
      const user = userEvent.setup();
      renderList({ tasks: mixed, config });

      await pick(user, "Auto-merge", "On");

      expect(titles()).toEqual(["Widget work"]);
    });

    it("combines the filters with each other and with the search box", async () => {
      const user = userEvent.setup();
      renderList({ tasks: mixed, config });

      await pick(user, "Author", "ada");
      await pick(user, "Auto-merge", "Off");
      expect(titles()).toEqual(["Loose work"]);

      await user.type(screen.getByPlaceholderText("Search tasks…"), "widget");
      expect(titles()).toEqual([]);
      expect(
        screen.getByText("No tasks match your search."),
      ).toBeInTheDocument();
    });

    it("says the filters are what emptied the list", async () => {
      const user = userEvent.setup();
      renderList({
        tasks: [
          { ...mixed[0], capabilities: [] },
          { ...mixed[1], capabilities: ["gcp"] },
        ],
        config,
      });

      await pick(user, "Repo", "acme/widgets");
      await pick(user, "Capability", "Google Cloud");

      expect(
        screen.getByText("No tasks match these filters."),
      ).toBeInTheDocument();
    });

    it("puts the whole list back with one Clear", async () => {
      const user = userEvent.setup();
      renderList({ tasks: mixed, config });

      await pick(user, "Repo", "acme/widgets");
      await user.type(screen.getByPlaceholderText("Search tasks…"), "widget");
      expect(titles()).toEqual(["Widget work"]);

      await user.click(screen.getByRole("button", { name: "Clear" }));

      expect(titles()).toEqual(["Widget work", "Gadget work", "Loose work"]);
      expect(
        screen.queryByRole("button", { name: "Clear" }),
      ).not.toBeInTheDocument();
    });

    // The menus are built from the tasks in view, so an attribute they
    // all share -- the repo, on a repo's own page -- can narrow nothing
    // and is not offered at all.
    it("offers only the attributes that could narrow the list", () => {
      renderList({
        tasks: [
          {
            id: 1,
            title: "One",
            state: "queued",
            repo: "acme/widgets",
            author: "ada",
            capabilities: [],
            blocked: false,
          },
          {
            id: 2,
            title: "Two",
            state: "queued",
            repo: "acme/widgets",
            author: "ada",
            capabilities: [],
            blocked: false,
          },
        ],
        config,
      });

      expect(screen.queryByLabelText("Repo")).not.toBeInTheDocument();
      expect(screen.queryByLabelText("Author")).not.toBeInTheDocument();
      expect(screen.queryByLabelText("Capability")).not.toBeInTheDocument();
      expect(screen.queryByLabelText("Origin")).not.toBeInTheDocument();
      expect(screen.queryByLabelText("Kind")).not.toBeInTheDocument();
      expect(screen.queryByLabelText("Auto-merge")).not.toBeInTheDocument();
    });

    // A choice the sidebar's state filter has since made impossible must
    // not go on hiding everything: it reads as "any" again.
    it("forgets a choice no task in view can match any more", async () => {
      const user = userEvent.setup();
      const { rerender } = renderList({
        tasks: mixed,
        config,
        stateFilter: "all",
      });

      await pick(user, "Repo", "acme/widgets");
      expect(titles()).toEqual(["Widget work"]);

      rerender({
        tasks: mixed.map((t) => (t.id === 1 ? { ...t, state: "running" } : t)),
        config,
        stateFilter: "queued",
      });

      expect(titles()).toEqual(["Gadget work", "Loose work"]);
    });
  });

  // grain/task-175: the prompt an agent was handed is reached from the
  // task's own page (DetailOverlay's Prompt button), not from a button
  // on every row of every list the task appears in -- so a row carries
  // no prompt affordance at all, and TaskList never fetches one.
  it("carries no per-row prompt button", () => {
    renderList();
    expect(
      screen.queryByRole("button", { name: /prompt/i }),
    ).not.toBeInTheDocument();
  });

  // What the run itself says it is doing (grain/task-240, the
  // update_status tool): the one thing on a row that changes while you
  // watch it, and the only answer to "what has this task been doing for
  // the last half hour?" short of reading a transcript.
  describe("a running task's own status", () => {
    const running = (extra) => ({
      id: 7,
      title: "Ship the other thing",
      state: "running",
      capabilities: [],
      blocked: false,
      ...extra,
    });

    it("shows what the run says it is doing, with how long ago it said it", () => {
      renderList({
        tasks: [
          running({
            activity: "waiting for CI on the second push",
            activityAt: new Date(Date.now() - 5 * 60 * 1000).toISOString(),
          }),
        ],
      });
      expect(
        screen.getByText("waiting for CI on the second push"),
      ).toBeInTheDocument();
      expect(screen.getByText("5m")).toBeInTheDocument();
    });

    // A synopsis with no timestamp -- a row written by an older build --
    // is shown alone rather than with an invented age.
    it("shows a status with no timestamp on its own", () => {
      renderList({ tasks: [running({ activity: "running the test suite" })] });
      expect(screen.getByText("running the test suite")).toBeInTheDocument();
      expect(
        document.querySelector(".task-activity-age"),
      ).not.toBeInTheDocument();
    });

    // The API only carries one for a live run, but a poll's answer is up
    // to three seconds old: a task that has just finished must not keep
    // showing what it was doing, which beside a "Completed" badge would
    // read as a run still going.
    it("drops the status the moment the task stops running", () => {
      renderList({
        tasks: [
          running({
            state: "completed",
            activity: "waiting for CI on the second push",
          }),
        ],
      });
      expect(
        screen.queryByText("waiting for CI on the second push"),
      ).not.toBeInTheDocument();
    });

    it("says nothing at all for a run that has not said anything", () => {
      renderList({ tasks: [running()] });
      expect(document.querySelector(".task-activity")).not.toBeInTheDocument();
    });

    // grain/task-295: the stretch before the agent's first turn -- the
    // sandbox being built, the repo cloned -- is narrated by grain
    // itself, since there is no agent yet to narrate it. Everything else
    // that has ever appeared here was an agent's own words, so whose
    // sentence this is is marked rather than left to the wording.
    it("marks a status grain wrote during setup as grain's own", () => {
      renderList({
        tasks: [
          running({ activity: "cloning acme/widgets", activityBySetup: true }),
        ],
      });
      expect(screen.getByText("cloning acme/widgets")).toBeInTheDocument();
      expect(document.querySelector(".task-activity-by")).toHaveTextContent(
        "grain",
      );
      expect(document.querySelector(".task-activity")).toHaveAttribute(
        "title",
        expect.stringContaining("What grain is doing to get this run started"),
      );
    });

    it("leaves the run's own status unmarked", () => {
      renderList({ tasks: [running({ activity: "running the test suite" })] });
      expect(
        document.querySelector(".task-activity-by"),
      ).not.toBeInTheDocument();
      expect(document.querySelector(".task-activity")).toHaveAttribute(
        "title",
        expect.stringContaining("What this run says it is doing"),
      );
    });
  });
});
