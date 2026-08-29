import { render, screen } from "@testing-library/react";
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
