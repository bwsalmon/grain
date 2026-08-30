import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import TaskList from "./TaskList.jsx";

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

  it("shows an empty message when nothing matches the filter", () => {
    renderList({ tasks: [] });
    expect(screen.getByText("No tasks in this state.")).toBeInTheDocument();
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
});
