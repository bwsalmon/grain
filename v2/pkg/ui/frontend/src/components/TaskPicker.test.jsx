import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import TaskPicker from "./TaskPicker.jsx";

const tasks = [
  { id: "12", title: "Fix the login bug" },
  { id: "15", title: "Add dark mode" },
  { id: "20", title: "Login page redesign" },
];

describe("TaskPicker", () => {
  it("filters by id or title as the user types, case-insensitively", async () => {
    const user = userEvent.setup();
    render(<TaskPicker tasks={tasks} onPick={() => {}} />);

    await user.type(screen.getByRole("textbox"), "login");

    expect(screen.getByText("Fix the login bug")).toBeInTheDocument();
    expect(screen.getByText("Login page redesign")).toBeInTheDocument();
    expect(screen.queryByText("Add dark mode")).not.toBeInTheDocument();
  });

  it("shows nothing until text is entered", async () => {
    render(<TaskPicker tasks={tasks} onPick={() => {}} />);
    expect(screen.queryByText("Fix the login bug")).not.toBeInTheDocument();
  });

  it("excludes ids in the exclude list", async () => {
    const user = userEvent.setup();
    render(<TaskPicker tasks={tasks} exclude={["12"]} onPick={() => {}} />);

    await user.type(screen.getByRole("textbox"), "login");

    expect(screen.queryByText("Fix the login bug")).not.toBeInTheDocument();
    expect(screen.getByText("Login page redesign")).toBeInTheDocument();
  });

  it("shows a no-matches message when nothing matches", async () => {
    const user = userEvent.setup();
    render(<TaskPicker tasks={tasks} onPick={() => {}} />);

    await user.type(screen.getByRole("textbox"), "xyz");

    expect(screen.getByText("No matching tasks")).toBeInTheDocument();
  });

  it("calls onPick and clears the query when a result is clicked", async () => {
    const onPick = vi.fn();
    const user = userEvent.setup();
    render(<TaskPicker tasks={tasks} onPick={onPick} />);

    const input = screen.getByRole("textbox");
    await user.type(input, "dark");
    await user.click(screen.getByText("Add dark mode"));

    expect(onPick).toHaveBeenCalledWith(tasks[1]);
    expect(input).toHaveValue("");
    expect(screen.queryByText("Add dark mode")).not.toBeInTheDocument();
  });

  it("navigates matches with the arrow keys and picks the highlighted one on Enter", async () => {
    const onPick = vi.fn();
    const user = userEvent.setup();
    render(<TaskPicker tasks={tasks} onPick={onPick} />);

    const input = screen.getByRole("textbox");
    await user.type(input, "login");
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{Enter}");

    expect(onPick).toHaveBeenCalledWith(tasks[2]);
  });

  it("closes the results on Escape", async () => {
    const user = userEvent.setup();
    render(<TaskPicker tasks={tasks} onPick={() => {}} />);

    await user.type(screen.getByRole("textbox"), "login");
    expect(screen.getByText("Fix the login bug")).toBeInTheDocument();

    await user.keyboard("{Escape}");
    expect(screen.queryByText("Fix the login bug")).not.toBeInTheDocument();
  });

  it("closes the results when clicking outside the picker", async () => {
    const user = userEvent.setup();
    render(
      <div>
        <TaskPicker tasks={tasks} onPick={() => {}} />
        <button>outside</button>
      </div>
    );

    await user.type(screen.getByRole("textbox"), "login");
    expect(screen.getByText("Fix the login bug")).toBeInTheDocument();

    await user.click(screen.getByText("outside"));
    expect(screen.queryByText("Fix the login bug")).not.toBeInTheDocument();
  });
});
