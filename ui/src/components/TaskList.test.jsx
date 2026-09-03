import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import TaskList from "./TaskList.jsx";
import api from "../api.js";

// TaskList itself never calls the API; its rows' own prompt button does
// (PromptOverlay, below), and this keeps that one fetch out of jsdom.
vi.mock("../api.js", () => ({ default: vi.fn() }));

const tasks = [
  { id: 1, title: "Fix the thing", state: "queued", capabilities: [], blocked: false },
  { id: 2, title: "Ship the other thing", state: "running", capabilities: [], blocked: true, blockedBy: [1] },
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
  render(<TaskList {...props} />);
  return props;
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
    expect(document.querySelector(".content-header .count")).toHaveTextContent("2");
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

  it("shows a completion-phase chip for a completed task not on the merge queue", () => {
    renderList({
      tasks: [
        { id: 3, title: "Awaiting submit task", state: "completed", pullRequest: "acme/widgets#1", autoMerge: false, capabilities: [], blocked: false },
        { id: 4, title: "Queued for merge task", state: "completed", pullRequest: "acme/widgets#2", autoMerge: true, capabilities: [], blocked: false },
      ],
    });
    expect(screen.getByText("Awaiting submit")).toBeInTheDocument();
    // The row for 4 says it with its state dot's own title, which is
    // STATE_LABELS.completed -- no chip repeating it.
    expect(screen.queryByText("Queued to merge")).not.toBeInTheDocument();
    expect(screen.getAllByTitle("Queued for merge").length).toBeGreaterThan(0);
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
      { id: 3, title: "Long done", state: "closed", capabilities: [], blocked: false },
    ];

    it("hides closed tasks by default", () => {
      renderList({ tasks: withClosed });
      expect(screen.queryByText("Long done")).not.toBeInTheDocument();
      expect(document.querySelector(".content-header .count")).toHaveTextContent("2");
    });

    it("shows closed tasks once the toggle is checked", async () => {
      const user = userEvent.setup();
      renderList({ tasks: withClosed });

      await user.click(screen.getByRole("checkbox", { name: "Show closed tasks" }));

      expect(screen.getByText("Long done")).toBeInTheDocument();
    });

    it("starts showing closed tasks when config.showClosedByDefault is set", () => {
      renderList({ tasks: withClosed, config: { showClosedByDefault: true } });
      expect(screen.getByText("Long done")).toBeInTheDocument();
      expect(screen.getByRole("checkbox", { name: "Show closed tasks" })).toBeChecked();
    });

    it("does not offer the toggle when there are no closed tasks", () => {
      renderList();
      expect(screen.queryByRole("checkbox", { name: "Show closed tasks" })).not.toBeInTheDocument();
    });

    it("does not hide closed tasks while the Closed filter itself is selected", () => {
      renderList({ tasks: withClosed, stateFilter: "closed" });
      expect(screen.getByText("Long done")).toBeInTheDocument();
      expect(screen.queryByRole("checkbox", { name: "Show closed tasks" })).not.toBeInTheDocument();
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
      { id: 1, title: "First", state: "queued", capabilities: [], blocked: false },
      { id: 2, title: "Second", state: "queued", capabilities: [], blocked: false },
      { id: 3, title: "Third", state: "queued", capabilities: [], blocked: false },
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

    it("never starts a drag, and shows no drag handle, without an onReorder prop", () => {
      const { container } = render(
        <TaskList tasks={threeTasks} stateFilter="all" config={null} onOpenTask={vi.fn()}
          selected={new Set()} onToggleSelect={vi.fn()} onSelectAll={vi.fn()} />,
      );
      expect(container.querySelector(".task-drag-handle")).not.toBeInTheDocument();
      expect(rowFor("First")).toHaveAttribute("draggable", "false");
    });
  });

  it("badges a task a schedule filed, and leaves an ordinary one unbadged", () => {
    renderList({
      tasks: [
        { ...tasks[0], scheduled: true },
        tasks[1],
      ],
    });
    expect(screen.getByTitle("filed automatically by a schedule")).toHaveTextContent("scheduled");
    expect(screen.queryAllByTitle("filed automatically by a schedule")).toHaveLength(1);
  });

  // bwsalmon/agents#642: a suite run files its pass of tasks with nobody
  // typing them in, so they get scheduled's own badge treatment.
  it("badges a task a suite run filed, and leaves an ordinary one unbadged", () => {
    renderList({
      tasks: [
        { ...tasks[0], suiteRun: true },
        tasks[1],
      ],
    });
    expect(screen.getByTitle("filed automatically by a task suite run")).toHaveTextContent("suite");
    expect(screen.queryAllByTitle("filed automatically by a task suite run")).toHaveLength(1);
    // The suite chip is its own thing, not the schedule one relabelled.
    expect(screen.queryByTitle("filed automatically by a schedule")).not.toBeInTheDocument();
  });

  // bwsalmon/agents#378: a stacked task explains itself by sitting under
  // the task it repairs, so the chip is only for the rows where that
  // nesting is missing.
  describe("the merge-fix chip on a stacked task", () => {
    const parent = { id: 1, title: "Fix the thing", state: "queued", capabilities: [], blocked: false };
    const fix = {
      id: 2, title: "Repair the pull request", state: "queued", capabilities: [], blocked: false,
      stacked: true, generatedFrom: 1,
    };

    it("leaves a fix nested under its own parent unchipped", () => {
      renderList({ tasks: [parent, fix] });
      expect(document.querySelector(".task-sublist")).toHaveTextContent("Repair the pull request");
      expect(screen.queryByText("merge fix")).not.toBeInTheDocument();
    });

    it("chips a fix listed at top level because its parent is filtered out of the view", () => {
      renderList({ tasks: [{ ...parent, state: "completed" }, fix], stateFilter: "queued" });
      expect(document.querySelector(".task-sublist")).not.toBeInTheDocument();
      expect(screen.getByTitle("the merge queue's own automatic fix for 1")).toHaveTextContent("merge fix");
    });

    it("chips a fix whose parent is gone entirely", () => {
      renderList({ tasks: [fix] });
      expect(screen.getByTitle("the merge queue's own automatic fix for 1")).toHaveTextContent("merge fix");
    });

    it("chips a fix with no generatedFrom at all, without naming a parent", () => {
      renderList({ tasks: [{ ...fix, generatedFrom: undefined }] });
      expect(screen.getByTitle("the merge queue's own automatic fix for another task's pull request"))
        .toHaveTextContent("merge fix");
    });
  });

  // bwsalmon/agents#539
  it("badges an interactive task, and leaves an ordinary one unbadged", () => {
    renderList({
      tasks: [
        { ...tasks[0], interactive: true },
        tasks[1],
      ],
    });
    expect(screen.getByTitle("a live chat, not a background task")).toHaveTextContent("interactive");
    expect(screen.queryAllByTitle("a live chat, not a background task")).toHaveLength(1);
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
    expect(runningDot.querySelector(".grain-mark-sheet")).toHaveAttribute("title", "Running");
  });

  describe("search (bwsalmon/agents#460)", () => {
    it("filters down to tasks whose title matches the search text", async () => {
      const user = userEvent.setup();
      renderList();

      await user.type(screen.getByPlaceholderText("Search tasks…"), "ship");

      expect(screen.queryByText("Fix the thing")).not.toBeInTheDocument();
      expect(screen.getByText("Ship the other thing")).toBeInTheDocument();
      expect(document.querySelector(".content-header .count")).toHaveTextContent("1");
    });

    it("filters down to tasks whose id matches the search text, case-insensitively", async () => {
      const user = userEvent.setup();
      renderList({
        tasks: [
          { id: "abc-1", title: "Fix the thing", state: "queued", capabilities: [], blocked: false },
          { id: "xyz-2", title: "Ship the other thing", state: "running", capabilities: [], blocked: false },
        ],
      });

      await user.type(screen.getByPlaceholderText("Search tasks…"), "ABC");

      expect(screen.getByText("Fix the thing")).toBeInTheDocument();
      expect(screen.queryByText("Ship the other thing")).not.toBeInTheDocument();
    });

    it("shows a search-specific empty message when nothing matches", async () => {
      const user = userEvent.setup();
      renderList();

      await user.type(screen.getByPlaceholderText("Search tasks…"), "nothing matches this");

      expect(screen.getByText("No tasks match your search.")).toBeInTheDocument();
    });

    it("hides the toolbar entirely when there are no tasks at all", () => {
      renderList({ tasks: [] });
      expect(screen.queryByPlaceholderText("Search tasks…")).not.toBeInTheDocument();
    });
  });

  describe("sort (bwsalmon/agents#460)", () => {
    const threeTasks = [
      { id: 1, title: "Charlie", state: "queued", capabilities: [], blocked: false, createdAt: "2026-01-03T00:00:00Z" },
      { id: 2, title: "Alpha", state: "queued", capabilities: [], blocked: false, createdAt: "2026-01-01T00:00:00Z" },
      { id: 3, title: "Bravo", state: "queued", capabilities: [], blocked: false, createdAt: "2026-01-02T00:00:00Z" },
    ];

    function titlesInOrder() {
      return [...document.querySelectorAll(".task-title")].map((el) => el.textContent);
    }

    it("defaults to the given (backlog) order", () => {
      renderList({ tasks: threeTasks });
      expect(titlesInOrder()).toEqual(["Charlie", "Alpha", "Bravo"]);
    });

    it("sorts alphabetically by title when picked from the sort menu", async () => {
      const user = userEvent.setup();
      renderList({ tasks: threeTasks });

      await user.click(screen.getByLabelText("Sort"));
      await user.click(await screen.findByRole("option", { name: "Title (A–Z)" }));

      expect(titlesInOrder()).toEqual(["Alpha", "Bravo", "Charlie"]);
    });

    it("sorts oldest-first by createdAt when picked from the sort menu", async () => {
      const user = userEvent.setup();
      renderList({ tasks: threeTasks });

      await user.click(screen.getByLabelText("Sort"));
      await user.click(await screen.findByRole("option", { name: "Oldest first" }));

      expect(titlesInOrder()).toEqual(["Alpha", "Bravo", "Charlie"]);
    });

    it("disables drag-and-drop reordering once a non-backlog sort is chosen", async () => {
      const onReorder = vi.fn();
      const user = userEvent.setup();
      renderList({ tasks: threeTasks, onReorder });

      expect(rowFor("Charlie")).toHaveAttribute("draggable", "true");

      await user.click(screen.getByLabelText("Sort"));
      await user.click(await screen.findByRole("option", { name: "Title (A–Z)" }));

      expect(document.querySelector(".task-drag-handle")).not.toBeInTheDocument();
      expect(rowFor("Alpha")).toHaveAttribute("draggable", "false");

      fireEvent.dragStart(rowFor("Alpha"));
      fireEvent.drop(rowFor("Bravo"));
      expect(onReorder).not.toHaveBeenCalled();
    });
  });

  // grain/task-91: every row carries its own way to see what the agent
  // was actually told, which is neither the title nor the description
  // the row shows.
  describe("the prompt button", () => {
    it("opens the prompt for that row's own task, without opening the task", async () => {
      api.mockResolvedValueOnce({ prompt: "Ship the other thing\n\nWork in acme/widgets.", attempt: 1 });
      const onOpenTask = vi.fn();
      const user = userEvent.setup();
      renderList({ onOpenTask });

      await user.click(screen.getByRole("button", { name: "Show the prompt for 2" }));

      expect(await screen.findByText(/Work in acme\/widgets\./)).toBeInTheDocument();
      expect(api).toHaveBeenLastCalledWith("/api/tasks/2/prompt");
      expect(onOpenTask).not.toHaveBeenCalled();
    });

    it("gives every task its own button", () => {
      renderList();
      expect(screen.getByRole("button", { name: "Show the prompt for 1" })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Show the prompt for 2" })).toBeInTheDocument();
    });
  });
});
